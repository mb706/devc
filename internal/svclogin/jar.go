package svclogin

// Cookie jars of cookie services. A login command prints its cookies as a
// JSON array on standard output; that output comes from a container that
// ran with a password and the network, so it is parsed strictly: known
// fields only, RFC 6265 names and values, values of at least MinCredential
// characters (finding D2-07: every cookie is scrubbed from responses), a
// domain that matches the service's host, bounded counts and sizes. devc
// stores the result with the time of the login as the realm secret
// _jar.<service>. Error messages never repeat what the output held (finding
// D2-08): cookies are named by their position, and JSON errors by kind
// and offset.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"
)

// Limits of login output and jars.
const (
	// MaxLoginOutput bounds what a login command may print.
	MaxLoginOutput = 256 << 10
	// maxCookies bounds the cookies of one jar; maxCookieValue and
	// maxCookieName their parts.
	maxCookies     = 64
	maxCookieValue = 4096
	maxCookieName  = 256
	// maxJar keeps a stored jar below the secret size limit.
	maxJar = 60 << 10
	// maxExpiry is the latest expiry accepted (year 9999).
	maxExpiry = 253402300799
)

// Cookie is one cookie of a jar. Expiry is in Unix seconds; 0 means a
// cookie without expiry, which lives as long as the jar (auth.refresh).
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
	Expiry int64  `json:"expiry"`
}

// Jar is the stored form of a login.
type Jar struct {
	V       int       `json:"v"`
	Service string    `json:"service"`
	LoginAt time.Time `json:"login_at"`
	Cookies []Cookie  `json:"cookies"`
}

// ParseLoginOutput reads a login command's output: a JSON array of cookies
// with exactly the fields name, value, domain, path and expiry (path and
// expiry may be left out), and nothing after it but white space. Every
// cookie must be valid and for host, the service's host; at least one must
// not have expired.
func ParseLoginOutput(b []byte, host string, now time.Time) ([]Cookie, error) {
	if len(b) > MaxLoginOutput {
		return nil, fmt.Errorf("the login command printed more than %d bytes", MaxLoginOutput)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, errors.New("the login command printed nothing; it must print the cookies as a JSON array")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var list []Cookie
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("the login command's output is not a JSON array of cookies (name, value, domain, path, expiry): %s", jsonProblem(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the login command printed something after the JSON array")
	}
	if list == nil {
		return nil, errors.New("the login command's output is not a JSON array of cookies")
	}
	for i := range list {
		normalizeCookie(&list[i])
		list[i].Domain = strings.ToLower(list[i].Domain)
	}
	if err := checkCookies(list, host); err != nil {
		return nil, err
	}
	if !anyLive(list, host, now) {
		return nil, errors.New("every cookie the login command printed has already expired")
	}
	return list, nil
}

// jsonProblem describes a decoding error without repeating any of the
// input (an unknown field's name, a string's content).
func jsonProblem(err error) string {
	var (
		se *json.SyntaxError
		te *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("invalid JSON at byte %d", se.Offset)
	case errors.As(err, &te):
		switch te.Field {
		case "", "name", "value", "domain", "path", "expiry":
			if te.Field == "" {
				return fmt.Sprintf("a value of the wrong type at byte %d", te.Offset)
			}
			return fmt.Sprintf("the field %s has the wrong type at byte %d", te.Field, te.Offset)
		}
		return fmt.Sprintf("a value of the wrong type at byte %d", te.Offset)
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		return "an unknown field"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the JSON ends early"
	}
	return "invalid JSON"
}

// normalizeCookie is the one form of a cookie where it is loaded (finding
// E2-02): name and value without surrounding spaces and tabs, as RFC 6265
// user agents store them, and "/" for a missing path. checkCookies then
// refuses anything a cookie value may not hold.
func normalizeCookie(c *Cookie) {
	c.Name = strings.Trim(c.Name, " \t")
	c.Value = strings.Trim(c.Value, " \t")
	if c.Path == "" {
		c.Path = "/"
	}
}

// checkCookies validates a list of cookies for host.
func checkCookies(list []Cookie, host string) error {
	if len(list) == 0 {
		return errors.New("the login produced no cookies")
	}
	if len(list) > maxCookies {
		return fmt.Errorf("more than %d cookies", maxCookies)
	}
	seen := map[string]bool{}
	for i, c := range list {
		what := fmt.Sprintf("cookie %d (in the order printed)", i+1)
		switch {
		case c.Name == "" || len(c.Name) > maxCookieName || !isToken(c.Name):
			return fmt.Errorf("%s: invalid name", what)
		case len(c.Value) > maxCookieValue:
			return fmt.Errorf("%s: the value is longer than %d bytes", what, maxCookieValue)
		case !cookieValue(c.Value):
			return fmt.Errorf("%s: the value contains characters a cookie value may not hold (white space, quotes, comma, semicolon, backslash or control characters)", what)
		case len(c.Value) < MinCredential:
			return fmt.Errorf("%s: the value has %d characters; devc needs at least %d to keep a cookie out of responses, so leave short cookies (a language, a consent flag) out of the login's output", what, len(c.Value), MinCredential)
		case !validDomain(c.Domain):
			return fmt.Errorf("%s: the domain is not a host name or address", what)
		case !DomainMatches(c.Domain, host):
			return fmt.Errorf("%s: its domain is not the service's host %s or a domain above it", what, host)
		case !validCookiePath(c.Path):
			return fmt.Errorf("%s: invalid path", what)
		case c.Expiry < 0 || c.Expiry > maxExpiry:
			return fmt.Errorf("%s: invalid expiry (Unix seconds, 0 for none)", what)
		}
		key := c.Name + "\x00" + strings.TrimPrefix(c.Domain, ".") + "\x00" + c.Path
		if seen[key] {
			return fmt.Errorf("%s: listed twice for the same domain and path", what)
		}
		seen[key] = true
	}
	return nil
}

// cookieValue: RFC 6265 cookie-octets.
func cookieValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x21 || c > 0x7e || c == '"' || c == ',' || c == ';' || c == '\\' {
			return false
		}
	}
	return true
}

func validDomain(d string) bool {
	d = strings.TrimPrefix(d, ".")
	if d == "" || len(d) > 253 {
		return false
	}
	if ip := net.ParseIP(d); ip != nil {
		return true
	}
	for _, l := range strings.Split(d, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			if c := l[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// DomainMatches reports whether a cookie for domain is sent to host: the
// same name, or host lies below domain (never for IP addresses).
func DomainMatches(domain, host string) bool {
	domain, host = strings.TrimPrefix(strings.ToLower(domain), "."), strings.ToLower(host)
	if domain == host {
		return true
	}
	if net.ParseIP(host) != nil {
		return false
	}
	return strings.HasSuffix(host, "."+domain) && strings.Contains(domain, ".")
}

func validCookiePath(p string) bool {
	if !strings.HasPrefix(p, "/") || len(p) > 1024 {
		return false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x21 || c > 0x7e || c == ';' || c == ',' {
			return false
		}
	}
	return true
}

// pathMatches is RFC 6265 path-match.
func pathMatches(cookiePath, reqPath string) bool {
	if cookiePath == reqPath {
		return true
	}
	if !strings.HasPrefix(reqPath, cookiePath) {
		return false
	}
	return strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/'
}

func live(c Cookie, host string, now time.Time) bool {
	return (c.Expiry == 0 || now.Unix() < c.Expiry) && DomainMatches(c.Domain, host)
}

func anyLive(list []Cookie, host string, now time.Time) bool {
	for _, c := range list {
		if live(c, host, now) {
			return true
		}
	}
	return false
}

// NewJar is the jar of a login of service at now.
func NewJar(service string, cookies []Cookie, now time.Time) *Jar {
	return &Jar{V: 1, Service: service, LoginAt: now.UTC(), Cookies: cookies}
}

// Marshal encodes the jar for the secret store.
func (j *Jar) Marshal() ([]byte, error) {
	b, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	if len(b) > maxJar {
		return nil, fmt.Errorf("the cookies take more than %d bytes", maxJar)
	}
	return b, nil
}

// ParseJar decodes a stored jar of service strictly and validates it again
// (a user may store one by hand).
func ParseJar(b []byte, service string) (*Jar, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var j Jar
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("the stored cookie jar is not a valid jar: %s", jsonProblem(err))
	}
	if j.V != 1 || j.Service != service || j.LoginAt.IsZero() {
		return nil, errors.New("the stored cookie jar is not a jar of this service")
	}
	for i := range j.Cookies {
		normalizeCookie(&j.Cookies[i])
	}
	// The host is checked when the cookies are used (Header): the jar
	// itself only needs to be well-formed.
	for _, c := range j.Cookies {
		if err := checkCookies([]Cookie{c}, strings.TrimPrefix(c.Domain, ".")); err != nil {
			return nil, fmt.Errorf("the stored cookie jar is invalid: %w", err)
		}
	}
	if len(j.Cookies) > maxCookies {
		return nil, fmt.Errorf("the stored cookie jar has more than %d cookies", maxCookies)
	}
	return &j, nil
}

// Header is the Cookie header for a request to host and path: the cookies
// that apply and have not expired, longer paths first (RFC 6265 5.4).
func (j *Jar) Header(host, path string, now time.Time) string {
	var use []Cookie
	for _, c := range j.Cookies {
		if live(c, host, now) && pathMatches(c.Path, path) {
			use = append(use, c)
		}
	}
	sort.SliceStable(use, func(a, b int) bool { return len(use[a].Path) > len(use[b].Path) })
	parts := make([]string, len(use))
	for i, c := range use {
		parts[i] = c.Name + "=" + c.Value
	}
	return strings.Join(parts, "; ")
}

// Credential is what the proxy adds to a request to host and path: the
// Cookie header of the cookies that apply, scrubbing every stored value
// and the header. When no cookie applies the header is empty, which is no
// credential: nothing is sent and only the stored values are scrubbed
// (finding E2-07).
func (j *Jar) Credential(host, path string, now time.Time) Credential {
	hdr := j.Header(host, path, now)
	scrub := j.Values()
	if hdr != "" {
		scrub = append(scrub, hdr)
	}
	return Credential{Header: "Cookie", Value: hdr, Scrub: scrub}
}

// Values are the cookie values, for scrubbing responses.
func (j *Jar) Values() []string {
	out := make([]string, 0, len(j.Cookies))
	for _, c := range j.Cookies {
		out = append(out, c.Value)
	}
	return out
}

// Expires is the earliest expiry among the cookies for host (zero when none
// expires).
func (j *Jar) Expires(host string) time.Time {
	var t time.Time
	for _, c := range j.Cookies {
		if c.Expiry > 0 && DomainMatches(c.Domain, host) {
			e := time.Unix(c.Expiry, 0).UTC()
			if t.IsZero() || e.Before(t) {
				t = e
			}
		}
	}
	return t
}

// Problem says why the jar cannot be used for host now ("" when it can):
// no login yet, a login older than refresh, or no live cookie for host.
func (j *Jar) Problem(refresh time.Duration, host string, now time.Time) string {
	switch {
	case j == nil:
		return "there has been no login yet"
	case now.Sub(j.LoginAt) > refresh:
		return fmt.Sprintf("the login of %s is older than auth.refresh (%s)", j.LoginAt.Format(time.RFC3339), refresh)
	case !anyLive(j.Cookies, host, now):
		return "the cookies of the last login have expired"
	}
	return ""
}
