// Package requests holds the rules for requests (work order C10a section
// 2): strict validation of what a box asks for, risk classes, who may
// decide them, and the per-project request log in
// durable/realms/<realm>/requests/<project>.jsonl.
//
// Security rules: every field of a request comes from a box and is hostile
// input (README rule 2). Validate refuses unknown kinds, fields that do not
// belong to the kind, names outside their patterns, control characters and
// oversized text, so nothing invalid is ever stored or executed. A request
// never executes anything by itself. MayDecide is the only place that says
// which origin may decide which risk class: high-risk requests are decided
// only through the local socket.
package requests

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// Limits of request fields.
const (
	MaxPath     = 4096
	MaxNote     = proto.MaxNoteText
	MaxTitle    = 256
	MaxBody     = 8000
	MaxPackages = 32
	MinFor      = time.Minute
	MaxFor      = 7 * 24 * time.Hour
	DefaultFor  = time.Hour
	// Expiry is how long a request may stay pending.
	Expiry = 7 * 24 * time.Hour
)

// Package managers a package request may name, with the install command
// devc runs as root in the box.
var Managers = []string{"apt", "dnf", "apk"}

var (
	packageRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,63}$`)
	hostLabelRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	hostAliasRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
	portRE      = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
)

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid request")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// field is one optional field of RequestParams.
type field struct {
	name string
	set  bool
}

func fields(p *proto.RequestParams) []field {
	return []field{
		{"path", p.Path != ""}, {"name", p.Name != ""}, {"text", p.Text != ""},
		{"manager", p.Manager != ""}, {"packages", p.Packages != nil},
		{"target", p.Target != ""}, {"for", p.For != ""}, {"grant", p.Grant != ""},
		{"title", p.Title != ""}, {"body", p.Body != ""}, {"branch", p.Branch != ""},
	}
}

// kindFields are the fields each kind may carry.
var kindFields = map[string][]string{
	proto.RequestSeal:      {"path"},
	proto.RequestSavepoint: {"name"},
	proto.RequestNote:      {"text"},
	proto.RequestPackage:   {"manager", "packages"},
	proto.RequestNet:       {"target", "for"},
	proto.RequestGrant:     {"grant", "name"},
	proto.RequestPR:        {"title", "body", "branch"},
}

// Validate checks a request from a box and returns it normalised (a net
// request's duration gets its default and canonical form). Paths are
// checked here only for their shape; the daemon resolves them against the
// project's data root.
func Validate(p proto.RequestParams) (proto.RequestParams, error) {
	risk := proto.RequestRisk(p.Kind)
	if risk == "" {
		return p, invalid("unknown kind %q (one of %s)", proto.Printable(proto.Truncate(p.Kind, 40)), strings.Join(proto.RequestKinds, ", "))
	}
	if p.Session != "" && !names.ValidSessionID(p.Session) {
		return p, invalid("invalid session ID")
	}
	allowed := kindFields[p.Kind]
	for _, f := range fields(&p) {
		if f.set && !slices.Contains(allowed, f.name) {
			return p, invalid("a %s request has no %s", p.Kind, f.name)
		}
	}
	switch p.Kind {
	case proto.RequestSeal:
		if err := checkPath(p.Path); err != nil {
			return p, err
		}
	case proto.RequestSavepoint:
		if p.Name != "" {
			if err := names.Check("savepoint name", p.Name); err != nil {
				return p, invalid("%v", err)
			}
		}
	case proto.RequestNote:
		if err := checkText("text", p.Text, MaxNote, true, true); err != nil {
			return p, err
		}
	case proto.RequestPackage:
		if !slices.Contains(Managers, p.Manager) {
			return p, invalid("unknown package manager %q (one of %s)", proto.Printable(proto.Truncate(p.Manager, 40)), strings.Join(Managers, ", "))
		}
		if len(p.Packages) == 0 || len(p.Packages) > MaxPackages {
			return p, invalid("name between 1 and %d packages", MaxPackages)
		}
		seen := map[string]bool{}
		for _, n := range p.Packages {
			if !packageRE.MatchString(n) {
				return p, invalid("package name %q does not match %s", proto.Printable(proto.Truncate(n, 80)), packageRE)
			}
			if seen[n] {
				return p, invalid("package %s is named twice", n)
			}
			seen[n] = true
		}
	case proto.RequestNet:
		if err := checkTarget(p.Target); err != nil {
			return p, err
		}
		d := DefaultFor
		if p.For != "" {
			var err error
			if len(p.For) > 32 {
				return p, invalid("duration too long")
			}
			d, err = time.ParseDuration(p.For)
			if err != nil {
				return p, invalid("duration %q: use a Go duration such as 30m or 2h", proto.Printable(p.For))
			}
			if d < MinFor || d > MaxFor {
				return p, invalid("duration %s is outside %s to %s", d, MinFor, MaxFor)
			}
		}
		p.For = d.String()
	case proto.RequestGrant:
		switch p.Grant {
		case "ssh":
			if !hostAliasRE.MatchString(p.Name) {
				return p, invalid("%q is not an SSH host alias", proto.Printable(proto.Truncate(p.Name, 80)))
			}
		case "secret":
			if !names.ValidSecret(p.Name) {
				return p, invalid("%q is not a secret name", proto.Printable(proto.Truncate(p.Name, 80)))
			}
		default:
			return p, invalid("a grant is for ssh or secret, not %q", proto.Printable(proto.Truncate(p.Grant, 40)))
		}
	case proto.RequestPR:
		if err := checkText("title", p.Title, MaxTitle, false, true); err != nil {
			return p, err
		}
		if err := checkText("body", p.Body, MaxBody, true, false); err != nil {
			return p, err
		}
		if p.Branch != "" {
			if err := names.CheckRef(p.Branch); err != nil {
				return p, invalid("branch %q: %v", proto.Printable(proto.Truncate(p.Branch, 80)), err)
			}
		}
	}
	return p, nil
}

// checkPath checks the shape of a seal path: absolute or relative, no
// "..", no control characters, bounded.
func checkPath(p string) error {
	switch {
	case p == "":
		return invalid("the path is empty")
	case len(p) > MaxPath:
		return invalid("the path is longer than %d bytes", MaxPath)
	case !utf8.ValidString(p):
		return invalid("the path is not valid UTF-8")
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return invalid("the path contains a control character")
		}
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return invalid("\"..\" is not allowed in paths")
		}
	}
	return nil
}

// checkText checks free text: valid UTF-8, at most max characters, no
// control characters other than newline and tab (multiline) or none.
func checkText(what, s string, max int, multiline, required bool) error {
	switch {
	case s == "" && required:
		return invalid("the %s is empty", what)
	case !utf8.ValidString(s):
		return invalid("the %s is not valid UTF-8", what)
	case utf8.RuneCountInString(s) > max:
		return invalid("the %s is longer than %d characters", what, max)
	}
	for _, r := range s {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			return invalid("the %s contains the control character U+%04X", what, r)
		}
	}
	return nil
}

// checkTarget checks a network target: "lan", or host:port with a host
// name or an IPv4 address (boxes have no IPv6).
func checkTarget(t string) error {
	if t == "lan" {
		return nil
	}
	host, port, ok := strings.Cut(t, ":")
	if !ok || strings.Contains(port, ":") {
		return invalid("target %q: use host:port or lan", proto.Printable(proto.Truncate(t, 80)))
	}
	if !portRE.MatchString(port) {
		return invalid("target %q: invalid port", proto.Printable(proto.Truncate(t, 80)))
	}
	if n, _ := strconv.Atoi(port); n < 1 || n > 65535 {
		return invalid("target %q: invalid port", proto.Printable(proto.Truncate(t, 80)))
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return invalid("target %q: only IPv4 addresses (boxes have no IPv6)", proto.Printable(t))
		}
		return nil
	}
	if host == "" || len(host) > 253 {
		return invalid("target %q: invalid host", proto.Printable(proto.Truncate(t, 80)))
	}
	for _, l := range strings.Split(host, ".") {
		if !hostLabelRE.MatchString(l) {
			return invalid("target %q: invalid host", proto.Printable(proto.Truncate(t, 80)))
		}
	}
	return nil
}

// Summary describes a request in one line (for listings and events).
func Summary(p proto.RequestParams) string {
	var s string
	switch p.Kind {
	case proto.RequestSeal:
		s = "seal " + p.Path
	case proto.RequestSavepoint:
		s = "savepoint"
		if p.Name != "" {
			s += " " + p.Name
		}
	case proto.RequestNote:
		s = "note: " + p.Text
	case proto.RequestPackage:
		s = "package (" + p.Manager + "): " + strings.Join(p.Packages, " ")
	case proto.RequestNet:
		s = "network access to " + p.Target + " for " + p.For
	case proto.RequestGrant:
		s = "grant " + p.Grant + " " + p.Name
	case proto.RequestPR:
		s = "pull request: " + p.Title
		if p.Branch != "" {
			s += " (" + p.Branch + ")"
		}
	default:
		s = p.Kind
	}
	return proto.Printable(proto.Truncate(s, 200))
}

// MayDecide says whether a decision from origin may decide a request of
// risk class risk. It is the one place this rule lives: the local socket
// decides anything; the auto list and every other origin (a hub) decide
// low-risk requests only, until a remote approval exists that a
// compromised hub cannot forge (plan section 5).
func MayDecide(origin, risk string) error {
	switch {
	case origin == proto.OriginLocal:
		return nil
	case risk == proto.RiskLow && (origin == proto.OriginAuto || origin == proto.OriginHub):
		return nil
	case risk == proto.RiskLow:
		return fmt.Errorf("unknown origin %q", origin)
	}
	return errors.New("high-risk requests can be decided only with the CLI on this machine ('devc requests approve|deny')")
}

// EffectiveOrigin is the origin of a decision: the connection's own, or one
// the client declares when that is weaker (a local client acting for a
// hub). A declaration never raises an origin.
func EffectiveOrigin(conn, declared string) (string, error) {
	switch {
	case declared == "" || declared == conn:
		return conn, nil
	case conn == proto.OriginLocal && declared == proto.OriginHub:
		return declared, nil
	}
	return "", fmt.Errorf("origin %q cannot be declared on this connection", proto.Printable(proto.Truncate(declared, 40)))
}

// NewID returns a new request ID: "r" and 20 time-ordered base32hex
// characters, like a session ID.
func NewID() string { return "r" + names.NewSessionID()[1:] }

// Expired reports whether a pending request is past its expiry.
func Expired(r *proto.Request, now time.Time) bool {
	return r.Status == proto.RequestPending && now.Sub(r.Time) > Expiry
}

// CheckReason checks the reason given with a decision: at most max
// characters, no control characters other than newline and tab.
func CheckReason(s string, max int) error {
	if s == "" {
		return nil
	}
	return checkText("reason", s, max, true, false)
}
