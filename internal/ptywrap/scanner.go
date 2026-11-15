// Package ptywrap is the status wrapper (work order C09 section 1): it runs
// a harness on a pseudo-terminal of its own, copies bytes between that and
// its own terminal, answers the harness's Program Status Protocol query
// (OSC 7501, which tmux does not answer) and takes the harness's status
// reports out of the output stream.
//
// The protocol (Program Status Protocol, specification revision 0.4 of
// 2026-10-10, superlogical.com/rex/docs/build/program-status):
//
//	report  ESC ] 7501 ; <key=value pairs separated by ":"> ST
//	query   ESC ] 7501 ; ? ST
//	reply   ESC ] 7501 ; ? [: states=… : kinds=…] ST
//
// ST is ESC \ or BEL. A whole sequence is at most 4096 bytes. Keys are
// lower-case letters (at most 16); values use letters, digits and
// "_.,+/=-"; whitespace around both is removed; malformed pairs are
// skipped, unknown keys ignored, the last of repeated keys wins. state is
// required (idle, working, done, blocked, error or clear; anything else
// voids the report); kind (permission, question, auth) applies to blocked
// only; msg and title are base64 (padding optional) of UTF-8 without
// control characters, at most 2048 and 192 bytes decoded; a report that
// breaks a limit is discarded whole. id addresses a child record.
//
// Transparency comes first: everything the harness writes passes
// unchanged except complete OSC 7501 sequences of at most 4096 bytes, and
// everything the terminal sends passes unchanged; the only bytes the
// wrapper adds are the answer to a query, in the harness's input.
//
// Security rules: the harness is box code and its reports are hostile
// input; they are parsed strictly and turned into events through the same
// validation as hook events. The wrapper runs inside the box with the
// harness's own rights.
package ptywrap

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Protocol constants.
const (
	// MaxSequence bounds a whole OSC 7501 sequence; longer ones are passed
	// through untouched.
	MaxSequence = 4096
	maxKey      = 16
	maxMsg      = 2048
	maxMsgEnc   = 2732
	maxTitle    = 192
	maxTitleEnc = 256
	maxApp      = 32
	maxIDLen    = 128
	maxIDSeg    = 32
	maxIDDepth  = 8
)

const (
	esc = 0x1b
	bel = 0x07
)

var prefix = []byte("\x1b]7501;")

// Item is one OSC 7501 sequence taken out of the output.
type Item struct {
	// Body is the text between "7501;" and the terminator.
	Body string
	// Term is the terminator the harness used (BEL or ESC \); an answer
	// uses the same one.
	Term string
}

// Query reports whether the item is the detection query.
func (it Item) Query() bool { return strings.TrimSpace(it.Body) == "?" }

// Scanner finds OSC 7501 sequences in an output stream that arrives in
// arbitrary pieces. It holds back only bytes that may still become part of
// such a sequence: a prefix of "ESC ] 7501 ;" at the end of a piece, or a
// sequence whose terminator has not arrived yet (at most MaxSequence
// bytes).
type Scanner struct {
	held []byte
}

type matchResult int

const (
	noMatch matchResult = iota
	partial
	complete
)

// match looks at b, which starts with ESC: a complete sequence (its length
// and item), a possible start that needs more bytes, or no sequence.
func match(b []byte) (matchResult, int, Item) {
	n := min(len(b), len(prefix))
	if !bytes.Equal(b[:n], prefix[:n]) {
		return noMatch, 0, Item{}
	}
	if n < len(prefix) {
		return partial, 0, Item{}
	}
	for i := len(prefix); i < len(b); i++ {
		if i >= MaxSequence {
			return noMatch, 0, Item{} // too long: pass it through
		}
		switch c := b[i]; {
		case c == bel:
			if i+1 > MaxSequence {
				return noMatch, 0, Item{}
			}
			return complete, i + 1, Item{Body: string(b[len(prefix):i]), Term: "\a"}
		case c == esc:
			if i+1 == len(b) {
				return partial, 0, Item{}
			}
			if b[i+1] != '\\' || i+2 > MaxSequence {
				// Another escape sequence begins: the terminal would abort
				// this string; so does the scanner, leaving it untouched.
				return noMatch, 0, Item{}
			}
			return complete, i + 2, Item{Body: string(b[len(prefix):i]), Term: "\x1b\\"}
		case c < 0x20 || c == 0x7f:
			// A control character cannot be part of a report (CAN and SUB
			// abort a string in terminals); leave the bytes untouched.
			return noMatch, 0, Item{}
		}
	}
	if len(b) >= MaxSequence {
		return noMatch, 0, Item{}
	}
	return partial, 0, Item{}
}

// Feed scans the next piece p of the output. It appends what passes on to
// out and returns it with the sequences taken out, in order.
func (s *Scanner) Feed(out, p []byte) ([]byte, []Item) {
	buf := p
	if len(s.held) > 0 {
		buf = append(s.held, p...)
		s.held = nil
	}
	var items []Item
	start, pos := 0, 0
	for {
		j := bytes.IndexByte(buf[pos:], esc)
		if j < 0 {
			return append(out, buf[start:]...), items
		}
		k := pos + j
		res, n, it := match(buf[k:])
		switch res {
		case noMatch:
			pos = k + 1
		case partial:
			out = append(out, buf[start:k]...)
			s.held = append([]byte(nil), buf[k:]...)
			return out, items
		case complete:
			out = append(out, buf[start:k]...)
			items = append(items, it)
			start, pos = k+n, k+n
		}
	}
}

// Flush returns the bytes held back, unchanged (at the end of the output).
func (s *Scanner) Flush(out []byte) []byte {
	out = append(out, s.held...)
	s.held = nil
	return out
}

// Held reports how many bytes are held back.
func (s *Scanner) Held() int { return len(s.held) }

// HeldPrefix reports whether the bytes held back are only a possible start
// of a sequence (shorter than "ESC ] 7501 ;"), not a sequence whose
// terminator is missing.
func (s *Scanner) HeldPrefix() bool { return len(s.held) > 0 && len(s.held) < len(prefix) }

// States of a report.
const (
	StateClear = "clear"
)

// Report is one parsed status report.
type Report struct {
	State string
	// ID addresses a child record; empty for the program's own (root)
	// record.
	ID       string
	Kind     string
	App      string
	Title    string
	Msg      string
	Progress int // -1: absent
}

// Reply is the wrapper's answer to the detection query (without the
// terminator): the states and kinds it maps onto events.
const Reply = "\x1b]7501;?:states=idle,working,done,blocked,error:kinds=permission,question,auth"

var errVoid = errors.New("report voided")

func keyOK(k string) bool {
	if k == "" || len(k) > maxKey {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 'a' || k[i] > 'z' {
			return false
		}
	}
	return true
}

func valueOK(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("_.,+/=-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func segmentOK(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("_.+-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func idOK(id string) bool {
	if len(id) > maxIDLen {
		return false
	}
	segs := strings.Split(id, "/")
	if len(segs) > maxIDDepth {
		return false
	}
	for _, s := range segs {
		if !segmentOK(s, maxIDSeg) {
			return false
		}
	}
	return true
}

// decodeText decodes a base64 value (padding optional) into text without
// control characters.
func decodeText(v string, maxEnc, maxDec int) (string, error) {
	if len(v) > maxEnc {
		return "", errVoid
	}
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(v, "="))
	if err != nil || len(b) > maxDec || !utf8.Valid(b) {
		return "", errVoid
	}
	s := string(b)
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", errVoid
		}
	}
	return s, nil
}

// ParseReport parses the body of a report (the text after "7501;"). A
// report without a known state, with an invalid id, or breaking a limit
// is an error.
func ParseReport(body string) (Report, error) {
	pairs := map[string]string{}
	for _, p := range strings.Split(body, ":") {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !keyOK(k) || !valueOK(v) {
			continue
		}
		pairs[k] = v
	}
	r := Report{State: pairs["state"], Progress: -1}
	switch r.State {
	case "idle", "working", "done", "blocked", "error", StateClear:
	default:
		return Report{}, errVoid
	}
	if id, ok := pairs["id"]; ok {
		if !idOK(id) {
			return Report{}, errVoid
		}
		r.ID = id
	}
	if r.State == "blocked" {
		switch k := pairs["kind"]; k {
		case "permission", "question", "auth":
			r.Kind = k
		}
	}
	if p, ok := pairs["progress"]; ok && (r.State == "working" || r.State == "blocked") {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 && n <= 100 {
			r.Progress = n
		}
	}
	if a, ok := pairs["app"]; ok {
		if !segmentOK(a, maxApp) {
			return Report{}, errVoid
		}
		r.App = a
	}
	if t, ok := pairs["title"]; ok {
		s, err := decodeText(t, maxTitleEnc, maxTitle)
		if err != nil {
			return Report{}, err
		}
		r.Title = s
	}
	if m, ok := pairs["msg"]; ok {
		s, err := decodeText(m, maxMsgEnc, maxMsg)
		if err != nil {
			return Report{}, err
		}
		r.Msg = s
	}
	return r, nil
}
