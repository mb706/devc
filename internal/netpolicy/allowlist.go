package netpolicy

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Entry is one allowlist entry: a host name or a "*.suffix" pattern, with
// an optional port. Without a port it allows 80 and 443.
type Entry struct {
	// Pattern is a lower-case host name, or "*." followed by one.
	Pattern string
	// Port is the one port the entry allows; 0 allows 80 and 443.
	Port int
}

// String is the entry in configuration syntax (its canonical form).
func (e Entry) String() string {
	if e.Port == 0 {
		return e.Pattern
	}
	return e.Pattern + ":" + strconv.Itoa(e.Port)
}

// DefaultPorts are the ports an entry without a port allows.
var DefaultPorts = []int{80, 443}

var (
	hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// numericLabelRE matches a final label that some resolvers read as part
	// of an IPv4 address (decimal or hexadecimal, as inet_aton does).
	numericLabelRE = regexp.MustCompile(`^(0x[0-9a-f]*|[0-9]+)$`)
	portRE         = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
)

// ErrIPLiteral is returned for an IP address where a name is required.
var ErrIPLiteral = errors.New("IP addresses are not accepted, only host names")

// NormalizeHost returns a host name in canonical form (lower case, without
// a trailing dot) or an error: IP literals in any form, empty labels,
// characters outside letters, digits and hyphens, and a numeric last label
// (which some resolvers read as an address) are refused.
func NormalizeHost(s string) (string, error) {
	h := strings.ToLower(strings.TrimSuffix(s, "."))
	if strings.HasPrefix(h, "[") || strings.Contains(h, ":") {
		return "", ErrIPLiteral
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", ErrIPLiteral
	}
	if h == "" || len(h) > 253 {
		return "", fmt.Errorf("%q is not a host name", s)
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if !hostLabelRE.MatchString(l) {
			return "", fmt.Errorf("%q is not a host name", s)
		}
	}
	if numericLabelRE.MatchString(labels[len(labels)-1]) {
		return "", ErrIPLiteral
	}
	return h, nil
}

// ParsePort parses a TCP port number (1-65535, no sign or leading zero).
func ParsePort(s string) (int, error) {
	if !portRE.MatchString(s) {
		return 0, fmt.Errorf("%q is not a port number (1-65535)", s)
	}
	n, _ := strconv.Atoi(s)
	if n > 65535 {
		return 0, fmt.Errorf("%q is not a port number (1-65535)", s)
	}
	return n, nil
}

// ParseEntry parses an allowlist entry: "name", "*.suffix", either with
// ":port".
func ParseEntry(s string) (Entry, error) {
	pattern, port := s, ""
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		pattern, port = s[:i], s[i+1:]
	}
	var e Entry
	if port != "" || strings.HasSuffix(s, ":") {
		p, err := ParsePort(port)
		if err != nil {
			return e, fmt.Errorf("allowlist entry %q: %w", s, err)
		}
		e.Port = p
	}
	wild := strings.HasPrefix(pattern, "*.")
	name, err := NormalizeHost(strings.TrimPrefix(pattern, "*."))
	if err != nil {
		return e, fmt.Errorf("allowlist entry %q: %w", s, err)
	}
	if wild {
		// A wildcard over a top-level domain would allow nearly anything.
		if !strings.Contains(name, ".") {
			return e, fmt.Errorf("allowlist entry %q: a wildcard needs at least two labels after \"*.\"", s)
		}
		name = "*." + name
	}
	e.Pattern = name
	return e, nil
}

// Matches reports whether the entry allows host (in canonical form) and
// port. "*.example.org" matches every name below example.org, not
// example.org itself.
func (e Entry) Matches(host string, port int) bool {
	if e.Port == 0 {
		if !slices.Contains(DefaultPorts, port) {
			return false
		}
	} else if e.Port != port {
		return false
	}
	if sfx, ok := strings.CutPrefix(e.Pattern, "*"); ok {
		return strings.HasSuffix(host, sfx) && len(host) > len(sfx)
	}
	return host == e.Pattern
}

// EntryList is a set of entries.
type EntryList []Entry

// Allows reports whether any entry allows host and port.
func (l EntryList) Allows(host string, port int) bool {
	for _, e := range l {
		if e.Matches(host, port) {
			return true
		}
	}
	return false
}

// Strings returns the entries in canonical form.
func (l EntryList) Strings() []string {
	out := make([]string, len(l))
	for i, e := range l {
		out[i] = e.String()
	}
	return out
}

// Expand builds the allowlist of domains and presets, without duplicates,
// in a stable order (presets first, in the order given).
func Expand(domains, presets []string) (EntryList, error) {
	var out EntryList
	seen := map[Entry]bool{}
	add := func(s string) error {
		e, err := ParseEntry(s)
		if err != nil {
			return err
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
		return nil
	}
	for _, p := range presets {
		list, ok := Presets[p]
		if !ok {
			return nil, fmt.Errorf("unknown allowlist preset %q (known: %s)", p, strings.Join(PresetNames(), ", "))
		}
		for _, s := range list {
			if err := add(s); err != nil {
				return nil, err
			}
		}
	}
	for _, s := range domains {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Intersect keeps the entries of l that are also in other (by canonical
// form): what a box was started with and the configuration still allows.
func (l EntryList) Intersect(other []string) EntryList {
	var out EntryList
	for _, e := range l {
		if slices.Contains(other, e.String()) {
			out = append(out, e)
		}
	}
	return out
}
