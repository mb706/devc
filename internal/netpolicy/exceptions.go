// Network exceptions (work order C13 section 2). An exception is granted
// while a box runs, from the host only: by `devc net allow` or by an
// approved `net` request, which the daemon carries out through the same
// command. That is the difference from the allowlist in the
// configuration, which is a capability of the box's start (box.Capabilities)
// and needs the box to be brought up again when it grows. Exceptions are
// kept per project in <state>/local/boxes/<realm>/<project>/net-exceptions.json,
// which no box can reach; they expire by time, survive down and up while
// valid, and are applied according to the box's level:
//
//   - KindAddr at level internet: the resolved IPv4 addresses and the port
//     become elements of the allow4 set with a timeout (ExceptionScript);
//     at level allowlist a name (never an address) is allowed by the
//     proxy for that port.
//   - KindLAN at level internet: the LAN ranges become elements of the
//     lan4 interval set with a timeout.
//   - KindHost at every level: the box agent listens on 127.0.0.1:<port>
//     and relays to the same port on the host's loopback, which the daemon
//     dials only while the exception is in force.

package netpolicy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// Kinds of exceptions.
const (
	KindAddr = "addr"
	KindLAN  = "lan"
	KindHost = "host"
)

// Limits of exceptions.
const (
	// MinExceptionFor and MaxExceptionFor bound a timed exception; one
	// without expiry must be asked for as "forever".
	MinExceptionFor = time.Second
	MaxExceptionFor = 30 * 24 * time.Hour
	// DefaultExceptionFor is the duration without --for.
	DefaultExceptionFor = 2 * time.Hour
	// MaxExceptions bounds the exceptions of one project.
	MaxExceptions = 256
	// maxExceptionAddrs bounds the addresses one name may give.
	maxExceptionAddrs = 16
	// maxExceptionsFile bounds the record when it is read.
	maxExceptionsFile = 1 << 20
)

// Forever is the duration word for an exception without expiry.
const Forever = "forever"

// LANRanges are what a "lan" exception opens: the RFC 1918 ranges and
// carrier-grade NAT (which VPN overlays use). Link-local is not among them
// (cloud metadata services live there), and the host's own addresses stay
// rejected by a rule before the LAN set.
var LANRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

// Exception is one granted exception.
type Exception struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Host is the name or IPv4 address as given (KindAddr).
	Host string `json:"host,omitempty"`
	// Port is the port (KindAddr, KindHost).
	Port int `json:"port,omitempty"`
	// Addrs are the IPv4 addresses of Host when it was granted (KindAddr;
	// empty for a name that did not resolve, which then works only at
	// level allowlist).
	Addrs   []netip.Addr `json:"addrs,omitempty"`
	Created time.Time    `json:"created"`
	// Until is the expiry; nil means forever.
	Until *time.Time `json:"until,omitempty"`
	// Origin says who granted it: "cli" or "request <id>".
	Origin string `json:"origin"`
}

var (
	exceptionIDRE = regexp.MustCompile(`^x[0-9a-f]{6}$`)
	originRE      = regexp.MustCompile(`^(cli|request r[0-9a-v]{20})$`)
)

// ValidExceptionID reports whether s has the form of an exception ID.
func ValidExceptionID(s string) bool { return exceptionIDRE.MatchString(s) }

// NewExceptionID returns a random ID ("x" and six hex digits).
func NewExceptionID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "x" + hex.EncodeToString(b)
}

// Target is the parsed target of `devc net allow`.
type Target struct {
	Kind string
	Host string // KindAddr: a canonical host name or an IPv4 address
	Port int
}

// String is the target in command syntax.
func (t Target) String() string {
	switch t.Kind {
	case KindLAN:
		return "lan"
	case KindHost:
		return "host:" + strconv.Itoa(t.Port)
	}
	return t.Host + ":" + strconv.Itoa(t.Port)
}

// IsAddr reports whether a KindAddr target is an IPv4 address.
func (t Target) IsAddr() bool {
	_, err := netip.ParseAddr(t.Host)
	return err == nil
}

// ParseTarget parses "lan", "host:<port>" (a service on the host's
// loopback) or "<ipv4-or-name>:<port>".
func ParseTarget(s string) (Target, error) {
	if s == "lan" {
		return Target{Kind: KindLAN}, nil
	}
	host, port, ok := strings.Cut(s, ":")
	if !ok || strings.Contains(port, ":") {
		return Target{}, fmt.Errorf("%q: use <address-or-name>:<port>, host:<port> or lan", s)
	}
	p, err := ParsePort(port)
	if err != nil {
		return Target{}, fmt.Errorf("%q: %w", s, err)
	}
	switch host {
	case "host":
		return Target{Kind: KindHost, Port: p}, nil
	case "lan":
		return Target{}, fmt.Errorf("%q: \"lan\" takes no port", s)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return Target{}, fmt.Errorf("%q: only IPv4 addresses (boxes have no IPv6)", s)
		}
		if a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a == netip.MustParseAddr("255.255.255.255") {
			return Target{}, fmt.Errorf("%q: not an address a box can be given (for the host's loopback use host:<port>)", s)
		}
		return Target{Kind: KindAddr, Host: a.String(), Port: p}, nil
	}
	name, err := NormalizeHost(host)
	if err != nil {
		return Target{}, fmt.Errorf("%q: %w", s, err)
	}
	return Target{Kind: KindAddr, Host: name, Port: p}, nil
}

// ParseFor parses a duration of an exception: a Go duration between
// MinExceptionFor and MaxExceptionFor, rounded up to whole seconds, or
// "forever" (0, true).
func ParseFor(s string) (d time.Duration, forever bool, err error) {
	if s == "" {
		return DefaultExceptionFor, false, nil
	}
	if s == Forever {
		return 0, true, nil
	}
	d, err = time.ParseDuration(s)
	if err != nil {
		return 0, false, fmt.Errorf("--for %q: use a duration such as 30m or 2h, or %q", s, Forever)
	}
	d = (d + time.Second - 1).Truncate(time.Second)
	if d < MinExceptionFor || d > MaxExceptionFor {
		return 0, false, fmt.Errorf("--for %s is outside %s to %s; use %q for no expiry", d, MinExceptionFor, MaxExceptionFor, Forever)
	}
	return d, false, nil
}

// Active reports whether the exception is in force at now.
func (x *Exception) Active(now time.Time) bool { return x.Until == nil || now.Before(*x.Until) }

// Forever reports whether the exception has no expiry.
func (x *Exception) Forever() bool { return x.Until == nil }

// Remaining is the time left (0 for forever or expired).
func (x *Exception) Remaining(now time.Time) time.Duration {
	if x.Until == nil || !now.Before(*x.Until) {
		return 0
	}
	return x.Until.Sub(now)
}

// TargetString is the exception's target in command syntax.
func (x *Exception) TargetString() string {
	return Target{Kind: x.Kind, Host: x.Host, Port: x.Port}.String()
}

// validate checks one exception as read from or written to the record:
// the fields of its kind only, and an address target that lists exactly
// its own address.
func (x *Exception) validate() error {
	switch {
	case !ValidExceptionID(x.ID):
		return fmt.Errorf("bad exception id %q", x.ID)
	case x.Created.IsZero():
		return errors.New("no creation time")
	case x.Until != nil && !x.Until.After(x.Created):
		return errors.New("expiry before creation")
	case !originRE.MatchString(x.Origin):
		return fmt.Errorf("bad origin %q", x.Origin)
	}
	switch x.Kind {
	case KindLAN:
		if x.Host != "" || x.Port != 0 || len(x.Addrs) > 0 {
			return errors.New("a lan exception has no host, port or addresses")
		}
	case KindHost:
		if x.Host != "" || len(x.Addrs) > 0 || x.Port < 1 || x.Port > 65535 {
			return errors.New("a host exception has a port only")
		}
	case KindAddr:
		t, err := ParseTarget(x.Host + ":" + strconv.Itoa(x.Port))
		if err != nil || t.Kind != KindAddr || t.Host != x.Host {
			return fmt.Errorf("bad target %q", x.Host)
		}
		if len(x.Addrs) > maxExceptionAddrs {
			return errors.New("too many addresses")
		}
		for _, a := range x.Addrs {
			if !a.Is4() {
				return fmt.Errorf("bad address %s", a)
			}
		}
		if t.IsAddr() && (len(x.Addrs) != 1 || x.Addrs[0].String() != x.Host) {
			return errors.New("an address exception lists exactly its address")
		}
	default:
		return fmt.Errorf("unknown exception kind %q", x.Kind)
	}
	return nil
}

// ExceptionFile is one project's exceptions.
type ExceptionFile struct {
	V          int         `json:"v"`
	Exceptions []Exception `json:"exceptions"`
}

// ReadExceptions reads the exceptions at path (none when the file does not
// exist), without following a symlink, strictly. Expired exceptions are
// returned too; callers filter with Active.
func ReadExceptions(path string) ([]Exception, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxExceptionsFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxExceptionsFile {
		return nil, fmt.Errorf("%s is too large", path)
	}
	var ef ExceptionFile
	if err := proto.StrictJSON(b, &ef); err != nil || ef.V != 1 {
		return nil, fmt.Errorf("%s is corrupt", path)
	}
	if len(ef.Exceptions) > MaxExceptions {
		return nil, fmt.Errorf("%s holds too many exceptions", path)
	}
	seen := map[string]bool{}
	for i := range ef.Exceptions {
		if err := ef.Exceptions[i].validate(); err != nil {
			return nil, fmt.Errorf("%s is corrupt: %v", path, err)
		}
		if seen[ef.Exceptions[i].ID] {
			return nil, fmt.Errorf("%s is corrupt: exception %s twice", path, ef.Exceptions[i].ID)
		}
		seen[ef.Exceptions[i].ID] = true
	}
	return ef.Exceptions, nil
}

// WriteExceptions replaces the exceptions at path (0600, by rename),
// dropping those expired at now. An empty list removes the file.
func WriteExceptions(path string, list []Exception, now time.Time) error {
	var keep []Exception
	for _, x := range list {
		if x.Active(now) {
			if err := x.validate(); err != nil {
				return fmt.Errorf("exception %s: %w", x.ID, err)
			}
			keep = append(keep, x)
		}
	}
	if len(keep) > MaxExceptions {
		return fmt.Errorf("a project may have at most %d exceptions; revoke some first ('devc net ls', 'devc net revoke')", MaxExceptions)
	}
	if len(keep) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].Created.Before(keep[j].Created) })
	b, err := json.MarshalIndent(ExceptionFile{V: 1, Exceptions: keep}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-net-exceptions-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ActiveExceptions returns the exceptions in force at now.
func ActiveExceptions(list []Exception, now time.Time) []Exception {
	var out []Exception
	for _, x := range list {
		if x.Active(now) {
			out = append(out, x)
		}
	}
	return out
}

// HostPorts returns the sorted host ports of the exceptions in force.
func HostPorts(list []Exception, now time.Time) []int {
	var out []int
	for _, x := range list {
		if x.Kind == KindHost && x.Active(now) && !slices.Contains(out, x.Port) {
			out = append(out, x.Port)
		}
	}
	slices.Sort(out)
	return out
}

// HostAllowed reports whether an exception in force allows host port p.
func HostAllowed(list []Exception, p int, now time.Time) bool {
	return slices.Contains(HostPorts(list, now), p)
}

// NameEntries are the allowlist entries the exceptions in force add at
// level allowlist: names (never addresses) with their port.
func NameEntries(list []Exception, now time.Time) EntryList {
	var out EntryList
	for _, x := range list {
		if x.Kind != KindAddr || !x.Active(now) {
			continue
		}
		if _, err := netip.ParseAddr(x.Host); err == nil {
			continue
		}
		e := Entry{Pattern: x.Host, Port: x.Port}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// NameEntriesEnd is the earliest end of an exception in force that
// NameEntries counts (zero when none ends).
func NameEntriesEnd(list []Exception, now time.Time) time.Time {
	var end time.Time
	for _, x := range list {
		if x.Kind != KindAddr || !x.Active(now) || x.Until == nil {
			continue
		}
		if _, err := netip.ParseAddr(x.Host); err == nil {
			continue
		}
		if end.IsZero() || x.Until.Before(end) {
			end = *x.Until
		}
	}
	return end
}

// elementTimeout renders the timeout of a set element for the time left:
// whole seconds, rounded up; "" for forever.
func elementTimeout(x *Exception, now time.Time) (string, bool) {
	if x.Until == nil {
		return "", true
	}
	left := x.Until.Sub(now)
	if left <= 0 {
		return "", false
	}
	return " timeout " + strconv.FormatInt(int64((left+time.Second-1)/time.Second), 10) + "s", true
}

// ExceptionScript renders the nft script that makes the exception sets of
// a box of level internet match list at now: both sets are flushed and the
// elements of the exceptions in force are added with the time they have
// left, in one transaction, so a change never leaves a valid exception out
// for a moment. lan says whether the box's rules have the LAN set (rules
// from before C13 do not; a lan exception then fails).
func ExceptionScript(list []Exception, now time.Time, lan bool) (string, error) {
	type elem struct {
		key     string
		forever bool
		until   time.Time
	}
	merge := func(m map[string]*elem, order *[]string, key string, x *Exception) {
		e := m[key]
		if e == nil {
			e = &elem{key: key}
			m[key] = e
			*order = append(*order, key)
		}
		if x.Until == nil {
			e.forever = true
		} else if x.Until.After(e.until) {
			e.until = *x.Until
		}
	}
	allow, lans := map[string]*elem{}, map[string]*elem{}
	var allowOrder, lanOrder []string
	for i := range list {
		x := &list[i]
		if !x.Active(now) {
			continue
		}
		switch x.Kind {
		case KindAddr:
			for _, a := range x.Addrs {
				merge(allow, &allowOrder, a.String()+" . "+strconv.Itoa(x.Port), x)
			}
		case KindLAN:
			if !lan {
				return "", errors.New("the box's network rules predate LAN exceptions; restart it ('devc down', 'devc up') to use them")
			}
			for _, p := range LANRanges {
				merge(lans, &lanOrder, p.String(), x)
			}
		}
	}
	render := func(m map[string]*elem, order []string) string {
		var parts []string
		for _, k := range order {
			e := m[k]
			s := e.key
			if !e.forever {
				fake := Exception{Until: &e.until}
				t, ok := elementTimeout(&fake, now)
				if !ok {
					continue
				}
				s += t
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ", ")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "flush set inet %s %s\n", Table, AllowSet)
	if lan {
		fmt.Fprintf(&b, "flush set inet %s %s\n", Table, LANSet)
	}
	if s := render(allow, allowOrder); s != "" {
		fmt.Fprintf(&b, "add element inet %s %s { %s }\n", Table, AllowSet, s)
	}
	if s := render(lans, lanOrder); s != "" {
		fmt.Fprintf(&b, "add element inet %s %s { %s }\n", Table, LANSet, s)
	}
	return b.String(), nil
}
