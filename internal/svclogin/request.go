// Package svclogin implements service logins (work order C18): a box
// reaches a service through the daemon, which holds the credential and adds
// it on the way out. This package holds the parts that do not depend on the
// daemon: the strict check of a request head as the box sent it (ReadHead,
// CheckHead), the rules (Allowed), the reverse proxy that adds the
// credential and filters both directions (Proxy), cookie jars from login
// commands (ParseLoginOutput, Jar), the machine's login state (State) and
// the request log (Log).
//
// Security rules: everything from a box is hostile input (README rule 2).
// A request is checked on its raw bytes before net/http parses it, and
// anything ambiguous is refused: absolute-form targets, "..", encoded
// slashes, a Host other than the service's local address, duplicated or
// conflicting length headers, obsolete line folding, bare line feeds. The
// credential never reaches the box (rule 5): it is added after the box's
// own credential headers are removed, response headers are scrubbed of it,
// Set-Cookie never passes, and neither logs nor error messages contain
// headers or bodies. Login output is untrusted and parsed strictly with
// size limits.
package svclogin

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/mb706/devc/internal/config"
)

// Limits of a request head.
const (
	// MaxHead bounds a request head (request line and headers).
	MaxHead = 64 << 10
	// maxHeaderLines bounds the header fields of one request.
	maxHeaderLines = 100
)

// Refusal is a request the proxy answers itself, with Status and a message
// for the box. Status 0 means the connection ends without an answer (the
// client went away).
type Refusal struct {
	Status int
	Msg    string
}

func (r *Refusal) Error() string { return r.Msg }

func refuse(status int, format string, args ...any) *Refusal {
	return &Refusal{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// Head is a request head that passed CheckHead.
type Head struct {
	Method string
	// Target is the request target as sent; Path is its normalised path
	// (the form rules are compared with and the upstream receives) and
	// Query its query without "?".
	Target string
	Path   string
	Query  string
	Proto  string
	// Raw is the head as read, ending in an empty line.
	Raw []byte
}

// ReadHead reads one request head: lines ending in CRLF up to and including
// an empty line, at most MaxHead bytes. A line ending in a bare LF, or a
// head that is too long, is refused.
func ReadHead(r *bufio.Reader) ([]byte, *Refusal) {
	var head []byte
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(head)+len(line) > MaxHead {
			return nil, refuse(http.StatusRequestHeaderFieldsTooLarge, "the request head is larger than %d bytes", MaxHead)
		}
		if err != nil {
			if len(head) == 0 && len(line) == 0 && errors.Is(err, io.EOF) {
				return nil, &Refusal{}
			}
			return nil, &Refusal{Msg: "the request head is incomplete"}
		}
		if len(line) < 2 || line[len(line)-2] != '\r' {
			return nil, refuse(http.StatusBadRequest, "a line of the request head ends in a bare line feed; lines must end in CRLF")
		}
		head = append(head, line...)
		if len(line) == 2 {
			if len(head) == 2 {
				return nil, refuse(http.StatusBadRequest, "the request starts with an empty line")
			}
			return head, nil
		}
	}
}

// CheckHead checks a head read by ReadHead for the service listening on
// port in the box: an origin-form target with a normal path, HTTP/1.0 or
// 1.1, well-formed header fields without obsolete line folding, at most one
// Content-Length or a Transfer-Encoding of exactly "chunked" (never both),
// exactly one Host that is the service's local address, no protocol
// upgrade, and Expect only for 100-continue.
func CheckHead(raw []byte, port int) (*Head, *Refusal) {
	lines := strings.Split(strings.TrimSuffix(string(raw), "\r\n\r\n"), "\r\n")
	if len(lines) == 0 {
		return nil, refuse(http.StatusBadRequest, "empty request")
	}
	for _, l := range lines {
		if strings.ContainsAny(l, "\r\n\x00") {
			return nil, refuse(http.StatusBadRequest, "the request head contains a stray carriage return or NUL")
		}
	}
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return nil, refuse(http.StatusBadRequest, "malformed request line (expected: METHOD /path HTTP/1.1, separated by single spaces)")
	}
	h := &Head{Method: parts[0], Target: parts[1], Proto: parts[2], Raw: raw}
	if h.Proto != "HTTP/1.1" && h.Proto != "HTTP/1.0" {
		return nil, refuse(http.StatusHTTPVersionNotSupported, "%q is not supported; use HTTP/1.1", printable(h.Proto))
	}
	if !isToken(h.Method) {
		return nil, refuse(http.StatusBadRequest, "malformed method")
	}
	switch {
	case h.Target == "*" || h.Method == http.MethodConnect:
		return nil, refuse(http.StatusBadRequest, "CONNECT and asterisk-form requests are refused")
	case !strings.HasPrefix(h.Target, "/"):
		return nil, refuse(http.StatusBadRequest, "the target %q is not origin-form: send the path only (GET /path HTTP/1.1, with Host: 127.0.0.1:%d), not an absolute URL", printable(h.Target), port)
	}
	var (
		hosts            []string
		lengths, encodes int
		upgrade, expect  bool
		expectOK         = true
	)
	if len(lines)-1 > maxHeaderLines {
		return nil, refuse(http.StatusRequestHeaderFieldsTooLarge, "more than %d header fields", maxHeaderLines)
	}
	for _, l := range lines[1:] {
		if l[0] == ' ' || l[0] == '\t' {
			return nil, refuse(http.StatusBadRequest, "obsolete line folding (a header line starting with white space) is refused")
		}
		name, value, ok := strings.Cut(l, ":")
		if !ok || !isToken(name) {
			return nil, refuse(http.StatusBadRequest, "malformed header field %q", printable(truncate(l, 60)))
		}
		value = strings.Trim(value, " \t")
		for i := 0; i < len(value); i++ {
			if c := value[i]; c < 0x20 && c != '\t' || c == 0x7f {
				return nil, refuse(http.StatusBadRequest, "the header %s contains a control character", name)
			}
		}
		switch strings.ToLower(name) {
		case "host":
			hosts = append(hosts, value)
		case "content-length":
			lengths++
			if value == "" || len(value) > 18 || strings.Trim(value, "0123456789") != "" {
				return nil, refuse(http.StatusBadRequest, "malformed Content-Length")
			}
		case "transfer-encoding":
			encodes++
			if !strings.EqualFold(value, "chunked") {
				return nil, refuse(http.StatusNotImplemented, "Transfer-Encoding %q is not supported (only chunked)", printable(truncate(value, 40)))
			}
		case "upgrade", "http2-settings":
			upgrade = true
		case "connection":
			for _, t := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(t), "upgrade") {
					upgrade = true
				}
			}
		case "expect":
			expect = true
			expectOK = expectOK && strings.EqualFold(value, "100-continue")
		}
	}
	switch {
	case lengths > 1:
		return nil, refuse(http.StatusBadRequest, "more than one Content-Length")
	case encodes > 1:
		return nil, refuse(http.StatusBadRequest, "more than one Transfer-Encoding")
	case lengths > 0 && encodes > 0:
		return nil, refuse(http.StatusBadRequest, "both Content-Length and Transfer-Encoding")
	case encodes > 0 && h.Proto == "HTTP/1.0":
		return nil, refuse(http.StatusBadRequest, "Transfer-Encoding in an HTTP/1.0 request")
	case len(hosts) != 1:
		return nil, refuse(http.StatusBadRequest, "the request needs exactly one Host header (Host: 127.0.0.1:%d)", port)
	case !LocalHost(hosts[0], port):
		return nil, refuse(http.StatusBadRequest, "Host %q is not this service's local address; use Host: 127.0.0.1:%d", printable(truncate(hosts[0], 80)), port)
	case upgrade:
		return nil, refuse(http.StatusForbidden, "protocol upgrades (WebSocket, h2c) are not supported by devc's service proxy")
	case expect && !expectOK:
		return nil, refuse(http.StatusExpectationFailed, "only Expect: 100-continue is supported")
	}
	path, query, err := NormalizeTarget(h.Target)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "refused: %v", err)
	}
	h.Path, h.Query = path, query
	return h, nil
}

// LocalHost reports whether a Host header names the service's local
// address: 127.0.0.1 or localhost with the service's port.
func LocalHost(v string, port int) bool {
	p := ":" + strconv.Itoa(port)
	return v == "127.0.0.1"+p || strings.EqualFold(v, "localhost"+p)
}

// NormalizeTarget splits an origin-form target into its normalised path and
// raw query. Percent-encoded letters, digits and "-._~" are decoded (they
// mean the same either way); other encodings are kept in upper case. It
// refuses anything ambiguous: a fragment, characters not allowed in a URL,
// invalid or encoded "/", "\", "%" and control characters, "." and ".."
// segments (also before a ";" parameter), and empty segments except a
// final one.
func NormalizeTarget(target string) (string, string, error) {
	if !strings.HasPrefix(target, "/") {
		return "", "", errors.New("the target must start with /")
	}
	if strings.Contains(target, "#") {
		return "", "", errors.New("a fragment (#) in the target")
	}
	rawPath, query, _ := strings.Cut(target, "?")
	var b strings.Builder
	for i := 0; i < len(rawPath); i++ {
		c := rawPath[i]
		switch {
		case c == '%':
			if i+2 >= len(rawPath) || !isHex(rawPath[i+1]) || !isHex(rawPath[i+2]) {
				return "", "", errors.New("an invalid percent-encoding in the path")
			}
			d := unhex(rawPath[i+1])<<4 | unhex(rawPath[i+2])
			switch {
			case d == '/' || d == '\\':
				return "", "", errors.New("an encoded slash or backslash in the path")
			case d == '%':
				return "", "", errors.New("an encoded percent sign in the path (double encoding)")
			case d < 0x20 || d == 0x7f:
				return "", "", errors.New("an encoded control character in the path")
			case isUnreserved(d):
				b.WriteByte(d)
			default:
				b.WriteByte('%')
				b.WriteString(strings.ToUpper(rawPath[i+1 : i+3]))
			}
			i += 2
		case c == '\\':
			return "", "", errors.New("a backslash in the path")
		case isPathChar(c):
			b.WriteByte(c)
		default:
			return "", "", fmt.Errorf("the character %q is not allowed in a URL path", printable(string(c)))
		}
	}
	path := b.String()
	segs := strings.Split(path[1:], "/")
	for i, seg := range segs {
		base, _, _ := strings.Cut(seg, ";")
		if base == "." || base == ".." {
			return "", "", errors.New("a . or .. segment in the path")
		}
		if seg == "" && i != len(segs)-1 {
			return "", "", errors.New("an empty segment (//) in the path")
		}
	}
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '%':
			if i+2 >= len(query) || !isHex(query[i+1]) || !isHex(query[i+2]) {
				return "", "", errors.New("an invalid percent-encoding in the query")
			}
			i += 2
		case isPathChar(c) || c == '?':
		default:
			return "", "", fmt.Errorf("the character %q is not allowed in a URL query", printable(string(c)))
		}
	}
	return path, query, nil
}

// PathMatches reports whether a normalised path lies below a rule's path:
// equal to it, or below it at a segment boundary ("/api" matches "/api" and
// "/api/x" but not "/apix"; "/api/" matches "/api/x").
func PathMatches(prefix, path string) bool {
	if path == prefix {
		return true
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return strings.HasPrefix(path, prefix+"/")
}

// Allowed returns the index of the first rule that allows method on the
// normalised path, or -1.
func Allowed(rules []config.ServiceRule, method, path string) int {
	for i, r := range rules {
		if slices.Contains(r.Methods, method) && PathMatches(r.Path, path) {
			return i
		}
	}
	return -1
}

// Needed is the message for a request no rule allows: which rule would be
// needed, for the agent to ask the user.
func Needed(s *config.Service, method, path string) string {
	m := printable(method)
	if !slices.Contains(config.ServiceMethods, method) {
		return fmt.Sprintf("devc service %s: %s requests are never forwarded (allowed methods: %s)", s.Name, truncate(m, 20), strings.Join(config.ServiceMethods, ", "))
	}
	return fmt.Sprintf("devc service %s: no rule allows %s %s. If the task needs it, ask the user to add a rule to realms/%s/services/%s/service.toml:\n\n[[allow]]\nmethods = [%q]\npath = %q\n",
		s.Name, m, truncate(path, 300), s.Realm, s.Name, method, truncate(path, 300))
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

// isPathChar: RFC 3986 pchar without percent-encodings, and "/".
func isPathChar(c byte) bool {
	return isUnreserved(c) || strings.IndexByte("!$&'()*+,;=:@/", c) >= 0
}

// printable replaces bytes outside printable ASCII, for messages.
func printable(s string) string {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
