package toolbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/mb706/devc/assets"
)

// The seccomp profile of every box is Podman's default profile with two
// changes (spec 6.2):
//
//  1. No new user namespaces. clone and unshare are allowed only while the
//     CLONE_NEWUSER bit of their flags is clear; clone3, whose flags live in
//     memory where seccomp cannot see them, falls to the default action
//     (ENOSYS), so libc falls back to clone.
//  2. No AF_ALG sockets. socket(38, …) fails with EAFNOSUPPORT. The 32-bit
//     multiplexer socketcall passes its arguments in memory, where seccomp
//     cannot see the domain, so it is refused as a whole with EPERM; 32-bit
//     programs must use the direct socket system calls (Linux 4.3 and later
//     have them on i386), where the AF_ALG rule applies. ENOSYS, which would
//     invite a libc to fall back, cannot be used: runc drops rules whose
//     action equals the default, and without an explicit unconditional
//     socketcall rule libseccomp turns every socket rule into a socketcall
//     rule on the x86 sub-architecture (the domain comparison replaced by
//     the call number), where rules with different actions collide
//     ("error adding seccomp rule for syscall socket: file exists"). The
//     explicit rule must precede the socket rules, which then become
//     redundant on x86 and are skipped.
//
// libseccomp cannot AND two comparisons on one argument, and separate allow
// rules are ORed, so a rule "allow socket if domain != 16" cannot simply be
// narrowed. Each such rule is replaced by rules that together allow exactly
// the domains [0, 2^32) minus the excluded ones: a ray "domain < n" where an
// interval starts at 0, otherwise aligned power-of-two blocks matched with
// SCMP_CMP_MASKED_EQ. The kernel truncates the domain to 32 bits while
// seccomp compares all 64, so every allow rule also requires the upper 32
// bits to be zero (the masks cover them), and the AF_ALG refusal matches the
// low 32 bits only. Values with upper bits set match no allow rule.

// Seccomp actions and comparison operators used here.
const (
	actAllow     = "SCMP_ACT_ALLOW"
	actErrno     = "SCMP_ACT_ERRNO"
	opNE         = "SCMP_CMP_NE"
	opLT         = "SCMP_CMP_LT"
	opLE         = "SCMP_CMP_LE"
	opEQ         = "SCMP_CMP_EQ"
	opGE         = "SCMP_CMP_GE"
	opGT         = "SCMP_CMP_GT"
	opMaskedEQ   = "SCMP_CMP_MASKED_EQ"
	cloneNewUser = 0x10000000
	afALG        = 38
	errEPERM     = 1
	errENOSYS    = 38
	errEAFNOSUP  = 97
	// DevcComment prefixes the comment of every rule devc adds.
	DevcComment = "devc: "
)

// Profile is the seccomp profile format of containers/common.
type Profile struct {
	DefaultAction   string    `json:"defaultAction"`
	DefaultErrnoRet *uint     `json:"defaultErrnoRet,omitempty"`
	DefaultErrno    string    `json:"defaultErrno,omitempty"`
	ArchMap         []ArchMap `json:"archMap"`
	Syscalls        []Syscall `json:"syscalls"`
}

// ArchMap lists an architecture and its sub-architectures.
type ArchMap struct {
	Architecture     string   `json:"architecture"`
	SubArchitectures []string `json:"subArchitectures"`
}

// Syscall is one rule.
type Syscall struct {
	Names    []string `json:"names"`
	Action   string   `json:"action"`
	Args     []Arg    `json:"args"`
	Comment  string   `json:"comment"`
	Includes Filter   `json:"includes"`
	Excludes Filter   `json:"excludes"`
	ErrnoRet *uint    `json:"errnoRet,omitempty"`
	Errno    string   `json:"errno,omitempty"`
}

// Arg is one argument comparison. For SCMP_CMP_MASKED_EQ, Value is the mask
// and ValueTwo the expected result.
type Arg struct {
	Index    uint   `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo"`
	Op       string `json:"op"`
}

// Filter restricts a rule to capabilities, architectures or kernels.
type Filter struct {
	Caps      []string `json:"caps,omitempty"`
	Arches    []string `json:"arches,omitempty"`
	MinKernel string   `json:"minKernel,omitempty"`
}

func (f Filter) empty() bool { return len(f.Caps) == 0 && len(f.Arches) == 0 && f.MinKernel == "" }

// Unconditional reports whether the rule applies regardless of capabilities,
// architecture and kernel.
func (s Syscall) Unconditional() bool { return s.Includes.empty() && s.Excludes.empty() }

// ParseProfile decodes a profile strictly: an unknown field in a future base
// profile stops the derivation instead of being dropped silently.
func ParseProfile(data []byte) (*Profile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parsing seccomp profile: %w", err)
	}
	for i := range p.Syscalls {
		if p.Syscalls[i].Args == nil {
			p.Syscalls[i].Args = []Arg{}
		}
	}
	return &p, nil
}

// Marshal renders the profile as indented JSON.
func (p *Profile) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "\t")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func uintp(v uint) *uint { return &v }

// SeccompProfile returns the box profile derived from the embedded base.
func SeccompProfile() ([]byte, error) {
	return DeriveProfile(assets.SeccompBase)
}

// DeriveProfile applies the two changes described at the top of this file to
// a base profile and fails if the base does not have the shape the changes
// rely on.
func DeriveProfile(base []byte) ([]byte, error) {
	p, err := ParseProfile(base)
	if err != nil {
		return nil, err
	}
	if p.DefaultAction != actErrno && p.DefaultAction != "SCMP_ACT_KILL" && p.DefaultAction != "SCMP_ACT_KILL_PROCESS" {
		return nil, fmt.Errorf("seccomp base profile: default action %s does not deny; the derivation needs a deny-by-default profile", p.DefaultAction)
	}
	if err := restrictNamespaces(p); err != nil {
		return nil, err
	}
	if err := restrictSockets(p); err != nil {
		return nil, err
	}
	if err := refuseSocketcall(p); err != nil {
		return nil, err
	}
	return p.Marshal()
}

// removeUnconditional deletes the named calls from every unconditional
// allow rule and reports which were found.
func removeUnconditional(p *Profile, targets []string) map[string]bool {
	removed := map[string]bool{}
	for i := range p.Syscalls {
		s := &p.Syscalls[i]
		if s.Action != actAllow || len(s.Args) > 0 || !s.Unconditional() {
			continue
		}
		s.Names = slices.DeleteFunc(s.Names, func(n string) bool {
			if slices.Contains(targets, n) {
				removed[n] = true
				return true
			}
			return false
		})
	}
	p.Syscalls = slices.DeleteFunc(p.Syscalls, func(s Syscall) bool { return len(s.Names) == 0 })
	return removed
}

// defaultIsENOSYS reports whether unlisted calls fail with ENOSYS.
func defaultIsENOSYS(p *Profile) bool {
	return p.DefaultAction == actErrno && p.DefaultErrnoRet != nil && *p.DefaultErrnoRet == errENOSYS
}

// refuseSocketcall makes socketcall fail with EPERM (see the top of the
// file), with the rule first in the list.
func refuseSocketcall(p *Profile) error {
	removeUnconditional(p, []string{"socketcall"})
	for _, s := range p.Syscalls {
		if s.Action == actAllow && slices.Contains(s.Names, "socketcall") && len(s.Includes.Caps) == 0 {
			return fmt.Errorf("seccomp base profile allows socketcall in a form the derivation does not handle (%q); review internal/toolbox/seccomp.go", s.Comment)
		}
	}
	if p.DefaultAction == actErrno && p.DefaultErrnoRet != nil && *p.DefaultErrnoRet == errEPERM {
		return errors.New("seccomp base profile: the default action is EPERM, so an explicit socketcall rule would be dropped; review internal/toolbox/seccomp.go")
	}
	rule := Syscall{Names: []string{"socketcall"}, Action: actErrno, Args: []Arg{}, ErrnoRet: uintp(errEPERM), Errno: "EPERM",
		Comment: DevcComment + "socketcall hides the socket domain; must precede the socket rules"}
	p.Syscalls = append([]Syscall{rule}, p.Syscalls...)
	return nil
}

func restrictNamespaces(p *Profile) error {
	targets := []string{"clone", "clone3", "unshare"}
	removed := removeUnconditional(p, targets)
	// Anything still allowing these calls without a capability condition
	// would reopen user namespaces; capability-gated variants are left alone
	// (boxes have no capabilities).
	for _, s := range p.Syscalls {
		for _, n := range targets {
			if s.Action == actAllow && slices.Contains(s.Names, n) && len(s.Includes.Caps) == 0 {
				return fmt.Errorf("seccomp base profile allows %s in a form the derivation does not handle (%q); review internal/toolbox/seccomp.go", n, s.Comment)
			}
		}
	}
	for _, n := range []string{"clone", "unshare"} {
		if !removed[n] {
			return fmt.Errorf("seccomp base profile no longer allows %s unconditionally; review internal/toolbox/seccomp.go", n)
		}
	}
	s390 := []string{"s390", "s390x"}
	noUser := func(index uint) []Arg {
		return []Arg{{Index: index, Value: cloneNewUser, ValueTwo: 0, Op: opMaskedEQ}}
	}
	p.Syscalls = append(p.Syscalls,
		Syscall{Names: []string{"clone"}, Action: actAllow, Args: noUser(0),
			Comment: DevcComment + "clone without CLONE_NEWUSER", Excludes: Filter{Arches: s390}},
		Syscall{Names: []string{"clone"}, Action: actAllow, Args: noUser(1),
			Comment: DevcComment + "clone without CLONE_NEWUSER (s390 passes the flags second)", Includes: Filter{Arches: s390}},
		Syscall{Names: []string{"unshare"}, Action: actAllow, Args: noUser(0),
			Comment: DevcComment + "unshare without CLONE_NEWUSER"},
	)
	// clone3 must fail with ENOSYS. When that is the default action the
	// call simply stays unlisted: libseccomp rejects rules whose action
	// equals the default.
	if !defaultIsENOSYS(p) {
		p.Syscalls = append(p.Syscalls, Syscall{Names: []string{"clone3"}, Action: actErrno, Args: []Arg{},
			ErrnoRet: uintp(errENOSYS), Errno: "ENOSYS", Comment: DevcComment + "clone3 hides its flags; libc falls back to clone"})
	}
	return nil
}

// domainArg returns the single comparison on argument 0 of a rule, if any.
func domainArg(s Syscall) (Arg, bool) {
	var found []Arg
	for _, a := range s.Args {
		if a.Index == 0 {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		return Arg{}, false
	}
	return found[0], true
}

func restrictSockets(p *Profile) error {
	// Domains every capability set refuses: unconditional errno rules of the
	// form "domain == x" (AF_VSOCK in the current base).
	denied := map[uint64]bool{}
	for _, s := range p.Syscalls {
		if slices.Contains(s.Names, "socket") && s.Action == actErrno && s.Unconditional() && len(s.Args) == 1 {
			if a := s.Args[0]; a.Index == 0 && a.Op == opEQ {
				denied[a.Value] = true
			}
		}
	}
	var out []Syscall
	replaced := 0
	for _, s := range p.Syscalls {
		a, ok := domainArg(s)
		if !slices.Contains(s.Names, "socket") || s.Action != actAllow || !ok || a.Op != opNE {
			out = append(out, s)
			continue
		}
		if len(s.Names) != 1 {
			// Split the socket part off a shared rule.
			rest := s
			rest.Names = slices.DeleteFunc(slices.Clone(s.Names), func(n string) bool { return n == "socket" })
			out = append(out, rest)
			s.Names = []string{"socket"}
		}
		excluded := map[uint64]bool{a.Value: true, afALG: true}
		for d := range denied {
			excluded[d] = true
		}
		var others []Arg
		for _, x := range s.Args {
			if x.Index != 0 {
				others = append(others, x)
			}
		}
		for _, iv := range allowedIntervals(excluded) {
			for _, cmp := range intervalArgs(iv) {
				r := s
				r.Args = append([]Arg{cmp}, others...)
				r.Comment = fmt.Sprintf("%ssocket domains %d-%d (replaces \"domain != %d\"; no AF_ALG)", DevcComment, iv.lo, iv.hi, a.Value)
				if iv.lo == iv.hi {
					r.Comment = fmt.Sprintf("%ssocket domain %d (replaces \"domain != %d\"; no AF_ALG)", DevcComment, iv.lo, a.Value)
				}
				out = append(out, r)
			}
		}
		replaced++
	}
	if replaced == 0 {
		return errors.New("seccomp base profile has no \"allow socket if domain != x\" rule; review internal/toolbox/seccomp.go")
	}
	out = append(out, Syscall{Names: []string{"socket"}, Action: actErrno,
		Args:     []Arg{{Index: 0, Value: 0xFFFFFFFF, ValueTwo: afALG, Op: opMaskedEQ}},
		ErrnoRet: uintp(errEAFNOSUP), Errno: "EAFNOSUPPORT", Comment: DevcComment + "no AF_ALG sockets"})
	p.Syscalls = out
	// Check the result: no allow rule may match AF_ALG, with or without
	// upper bits set.
	for _, s := range p.Syscalls {
		if !slices.Contains(s.Names, "socket") || s.Action != actAllow {
			continue
		}
		a, ok := domainArg(s)
		if !ok {
			return fmt.Errorf("seccomp: a socket allow rule has no single domain comparison (%q)", s.Comment)
		}
		for _, v := range []uint64{afALG, 1<<32 | afALG, 0xFFFFFFFF00000000 | afALG} {
			if ArgMatches(a, v) {
				return fmt.Errorf("seccomp: socket rule %q still allows domain %#x", s.Comment, v)
			}
		}
	}
	return nil
}

type interval struct{ lo, hi uint64 }

// allowedIntervals returns [0, 2^32) minus the excluded values, as sorted
// closed intervals.
func allowedIntervals(excluded map[uint64]bool) []interval {
	var ex []uint64
	for v := range excluded {
		if v <= 0xFFFFFFFF {
			ex = append(ex, v)
		}
	}
	sort.Slice(ex, func(i, j int) bool { return ex[i] < ex[j] })
	var out []interval
	next := uint64(0)
	for _, v := range ex {
		if v > next {
			out = append(out, interval{next, v - 1})
		}
		next = v + 1
	}
	if next <= 0xFFFFFFFF {
		out = append(out, interval{next, 0xFFFFFFFF})
	}
	return out
}

// intervalArgs expresses one interval of domain values as comparisons, each
// of which becomes its own rule. Every comparison is on all 64 bits, so no
// value with upper bits set matches.
func intervalArgs(iv interval) []Arg {
	if iv.lo == 0 {
		return []Arg{{Index: 0, Value: iv.hi + 1, Op: opLT}}
	}
	var out []Arg
	for lo := iv.lo; lo <= iv.hi; {
		size := uint64(1)
		for lo%(size*2) == 0 && lo+size*2-1 <= iv.hi {
			size *= 2
		}
		if size == 1 {
			out = append(out, Arg{Index: 0, Value: lo, Op: opEQ})
		} else {
			out = append(out, Arg{Index: 0, Value: ^(size - 1), ValueTwo: lo, Op: opMaskedEQ})
		}
		lo += size
	}
	return out
}

// ArgMatches evaluates one comparison against a 64-bit argument value the
// way libseccomp does.
func ArgMatches(a Arg, v uint64) bool {
	switch a.Op {
	case opNE:
		return v != a.Value
	case opLT:
		return v < a.Value
	case opLE:
		return v <= a.Value
	case opEQ:
		return v == a.Value
	case opGE:
		return v >= a.Value
	case opGT:
		return v > a.Value
	case opMaskedEQ:
		return v&a.Value == a.ValueTwo
	}
	return false
}
