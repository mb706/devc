package toolbox

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mb706/devc/assets"
)

// evaluate returns the actions of all rules matching a syscall with the
// given argument values for a box with caps on arch. Argument comparisons
// within one rule are ANDed.
func evaluate(p *Profile, name string, args map[uint]uint64, caps []string, arch string) []string {
	var out []string
	for _, s := range p.Syscalls {
		if !slices.Contains(s.Names, name) || !applies(s, caps, arch) {
			continue
		}
		match := true
		for _, a := range s.Args {
			if !ArgMatches(a, args[a.Index]) {
				match = false
			}
		}
		if match {
			act := s.Action
			if s.ErrnoRet != nil {
				act += "/" + s.Errno
			}
			out = append(out, act)
		}
	}
	return out
}

func applies(s Syscall, caps []string, arch string) bool {
	for _, c := range s.Includes.Caps {
		if !slices.Contains(caps, c) {
			return false
		}
	}
	if len(s.Includes.Arches) > 0 && !slices.Contains(s.Includes.Arches, arch) {
		return false
	}
	for _, c := range s.Excludes.Caps {
		if slices.Contains(caps, c) {
			return false
		}
	}
	return !slices.Contains(s.Excludes.Arches, arch)
}

func derived(t *testing.T) (*Profile, *Profile) {
	t.Helper()
	out, err := DeriveProfile(assets.SeccompBase)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseProfile(out)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseProfile(assets.SeccompBase)
	if err != nil {
		t.Fatal(err)
	}
	return b, d
}

// The decision for one call: the single action of all matching rules, or
// "default" when none matches. Overlapping rules with different actions are
// a test failure: their outcome would depend on libseccomp internals.
func decide(t *testing.T, p *Profile, name string, args map[uint]uint64, caps []string, arch string) string {
	t.Helper()
	acts := evaluate(p, name, args, caps, arch)
	if len(acts) == 0 {
		return "default"
	}
	for _, a := range acts[1:] {
		if a != acts[0] {
			t.Fatalf("%s %v caps=%v arch=%s: conflicting rules %v", name, args, caps, arch, acts)
		}
	}
	return acts[0]
}

func TestSeccompNamespaces(t *testing.T) {
	_, d := derived(t)
	for _, arch := range []string{"amd64", "arm64", "s390x"} {
		flagArg := uint(0)
		if arch == "s390x" {
			flagArg = 1
		}
		for _, caps := range [][]string{nil, {"CAP_SYS_ADMIN"}} {
			for _, flags := range []uint64{0, 0x11 /* SIGCHLD */, 0x01200011 /* fork-like */, 0x10000000, 0x10000011, 0x7E020000 | 0x10000000} {
				want := actAllow
				if flags&cloneNewUser != 0 {
					want = "default"
				}
				if got := decide(t, d, "clone", map[uint]uint64{flagArg: flags}, caps, arch); got != want {
					t.Errorf("clone(%#x) on %s caps=%v: %s, want %s", flags, arch, caps, got, want)
				}
				if got := decide(t, d, "unshare", map[uint]uint64{0: flags}, caps, arch); got != want {
					t.Errorf("unshare(%#x) on %s: %s, want %s", flags, arch, got, want)
				}
			}
			if got := decide(t, d, "clone3", nil, caps, arch); got != "default" {
				t.Errorf("clone3 on %s: %s, want the default action", arch, got)
			}
		}
	}
	if d.DefaultAction != actErrno || d.DefaultErrnoRet == nil || *d.DefaultErrnoRet != errENOSYS {
		t.Errorf("default action %s/%v: clone3 and refused namespaces rely on ENOSYS", d.DefaultAction, d.DefaultErrnoRet)
	}
}

func TestSeccompSockets(t *testing.T) {
	b, d := derived(t)
	values := []uint64{}
	for v := uint64(0); v <= 70; v++ {
		values = append(values, v)
	}
	values = append(values, 127, 128, 255, 256, 1000, 1<<20, 1<<31-1, 1<<31, 0xFFFFFFFE, 0xFFFFFFFF,
		1<<32, 1<<32|afALG, 1<<32|2, 0xFFFFFFFF00000000|afALG, 0xFFFFFFFFFFFFFFFF)
	for _, caps := range [][]string{nil, {"CAP_AUDIT_WRITE"}} {
		for _, v := range values {
			for _, proto := range []uint64{0, 9} {
				args := map[uint]uint64{0: v, 2: proto}
				got := decide(t, d, "socket", args, caps, "amd64")
				var want string
				switch {
				case v&0xFFFFFFFF == afALG:
					want = actErrno + "/EAFNOSUPPORT"
				case v > 0xFFFFFFFF:
					want = "default"
				case v == 40:
					want = actErrno + "/EPERM"
				case v == 16 && proto == 9 && len(caps) == 0:
					want = actErrno + "/EINVAL"
				default:
					want = actAllow
				}
				if got != want {
					t.Errorf("socket(%#x, proto %d) caps=%v: %s, want %s", v, proto, caps, got, want)
				}
				// Outside AF_ALG, AF_VSOCK and values with upper bits set,
				// the base decision must be unchanged.
				if v <= 0xFFFFFFFF && v != afALG && v != 40 {
					if bw := evaluate(b, "socket", args, caps, "amd64"); len(bw) == 0 || !slices.Contains(bw, got) {
						t.Errorf("socket(%d, proto %d) caps=%v: base %v, derived %s", v, proto, caps, bw, got)
					}
				}
			}
		}
	}
}

// TestSeccompNothingElseChanged undoes the documented changes and expects
// the base profile back.
func TestSeccompNothingElseChanged(t *testing.T) {
	b, d := derived(t)
	if b.DefaultAction != d.DefaultAction || !reflect.DeepEqual(b.DefaultErrnoRet, d.DefaultErrnoRet) ||
		b.DefaultErrno != d.DefaultErrno || !reflect.DeepEqual(b.ArchMap, d.ArchMap) {
		t.Error("top-level fields changed")
	}
	var dKept []Syscall
	for _, s := range d.Syscalls {
		if !strings.HasPrefix(s.Comment, DevcComment) {
			dKept = append(dKept, s)
		}
	}
	var bKept []Syscall
	for _, s := range b.Syscalls {
		if a, ok := domainArg(s); ok && slices.Equal(s.Names, []string{"socket"}) && s.Action == actAllow && a.Op == opNE {
			continue // replaced by range rules
		}
		if s.Action == actAllow && len(s.Args) == 0 && s.Unconditional() {
			s.Names = slices.DeleteFunc(slices.Clone(s.Names), func(n string) bool {
				return n == "clone" || n == "clone3" || n == "unshare"
			})
		}
		bKept = append(bKept, s)
	}
	if !reflect.DeepEqual(bKept, dKept) {
		t.Errorf("the derivation changed more than documented (%d vs %d rules)", len(bKept), len(dKept))
		for i := range min(len(bKept), len(dKept)) {
			if !reflect.DeepEqual(bKept[i], dKept[i]) {
				t.Errorf("first difference at %d:\nbase    %+v\nderived %+v", i, bKept[i], dKept[i])
				break
			}
		}
	}
	added := map[string]int{}
	for _, s := range d.Syscalls {
		if strings.HasPrefix(s.Comment, DevcComment) {
			added[strings.Join(s.Names, ",")]++
		}
	}
	if added["clone"] != 2 || added["unshare"] != 1 || added["socket"] < 3 || len(added) != 3 {
		t.Errorf("added rules: %v", added)
	}
}

func TestSeccompRejectsUnexpectedBase(t *testing.T) {
	for name, base := range map[string]string{
		"unknown field":  `{"defaultAction":"SCMP_ACT_ERRNO","bogus":1,"syscalls":[]}`,
		"allow default":  `{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[]}`,
		"no clone":       `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read"],"action":"SCMP_ACT_ALLOW"}]}`,
		"no socket rule": `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["clone","unshare","socket"],"action":"SCMP_ACT_ALLOW"}]}`,
		"arch-only clone": `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["clone","unshare"],"action":"SCMP_ACT_ALLOW"},` +
			`{"names":["clone"],"action":"SCMP_ACT_ALLOW","includes":{"arches":["arm"]}},` +
			`{"names":["socket"],"action":"SCMP_ACT_ALLOW","args":[{"index":0,"value":40,"op":"SCMP_CMP_NE"}]}]}`,
	} {
		if _, err := DeriveProfile([]byte(base)); err == nil {
			t.Errorf("%s: derivation succeeded", name)
		}
	}
}

func TestIntervalArgs(t *testing.T) {
	for _, iv := range []interval{{0, 15}, {17, 37}, {39, 39}, {41, 0xFFFFFFFF}, {1, 1 << 20}} {
		cmps := intervalArgs(iv)
		check := func(v uint64) {
			n := 0
			for _, c := range cmps {
				if ArgMatches(c, v) {
					n++
				}
			}
			in := v >= iv.lo && v <= iv.hi
			if (in && n != 1) || (!in && n != 0) {
				t.Errorf("%+v: value %#x matched %d comparisons", iv, v, n)
			}
		}
		for _, v := range []uint64{0, 1, iv.lo - 1, iv.lo, iv.lo + 1, (iv.lo + iv.hi) / 2, iv.hi - 1, iv.hi, iv.hi + 1, 1 << 32, 1<<32 | iv.lo, ^uint64(0)} {
			check(v)
		}
		if len(cmps) > 40 {
			t.Errorf("%+v: %d comparisons", iv, len(cmps))
		}
	}
}
