package netpolicy

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
)

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]Target{
		"lan":               {Kind: KindLAN},
		"host:5432":         {Kind: KindHost, Port: 5432},
		"192.168.1.10:8080": {Kind: KindAddr, Host: "192.168.1.10", Port: 8080},
		"NAS.local:443":     {Kind: KindAddr, Host: "nas.local", Port: 443},
	} {
		got, err := ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("ParseTarget(%q) = %+v, %v", in, got, err)
		}
		if err == nil && got.String() != strings.ToLower(in) {
			t.Errorf("%+v renders as %q", got, got.String())
		}
	}
	for _, in := range []string{"", "lan:1", "host", "host:", "host:0", "x:70000", "127.0.0.1:80", "0.0.0.0:80", "224.0.0.1:80", "255.255.255.255:1",
		"[::1]:80", "fd00::1:80", "a b:80", "a.org:80:1", "2130706433:80", "nas.local"} {
		if got, err := ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) = %+v, want an error", in, got)
		}
	}
}

func TestParseFor(t *testing.T) {
	if d, f, err := ParseFor(""); d != 2*time.Hour || f || err != nil {
		t.Errorf("default: %v %v %v", d, f, err)
	}
	if d, f, err := ParseFor("forever"); d != 0 || !f || err != nil {
		t.Errorf("forever: %v %v %v", d, f, err)
	}
	if d, _, err := ParseFor("1500ms"); d != 2*time.Second || err != nil {
		t.Errorf("rounding: %v %v", d, err)
	}
	for _, s := range []string{"0s", "-1h", "31d", "721h", "soon", "1"} {
		if _, _, err := ParseFor(s); err == nil {
			t.Errorf("ParseFor(%q) accepted", s)
		}
	}
}

func until(t time.Time) *time.Time { return &t }

func TestExceptionStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boxes", "r", "p", "net-exceptions.json")
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	list := []Exception{
		{ID: "x000001", Kind: KindAddr, Host: "192.168.1.10", Port: 22, Addrs: []netip.Addr{netip.MustParseAddr("192.168.1.10")}, Created: now, Until: until(now.Add(time.Hour)), Origin: "cli"},
		{ID: "x000002", Kind: KindLAN, Created: now, Until: until(now.Add(time.Minute)), Origin: "request r0123456789abcdefghij"},
		{ID: "x000003", Kind: KindHost, Port: 5432, Created: now, Origin: "cli"},
		{ID: "x000004", Kind: KindAddr, Host: "pkg.example", Port: 443, Created: now.Add(-2 * time.Hour), Until: until(now.Add(-time.Hour)), Origin: "cli"},
	}
	if got, err := ReadExceptions(path); err != nil || got != nil {
		t.Fatalf("missing file: %v %v", got, err)
	}
	if err := WriteExceptions(path, list, now); err != nil {
		t.Fatal(err)
	}
	got, err := ReadExceptions(path)
	if err != nil || len(got) != 3 {
		t.Fatalf("read %d, %v (the expired one is dropped)", len(got), err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if !slices.Equal(HostPorts(got, now), []int{5432}) || !HostAllowed(got, 5432, now) || HostAllowed(got, 5433, now) {
		t.Error("host ports")
	}
	if len(ActiveExceptions(got, now.Add(2*time.Minute))) != 2 {
		t.Error("the lan exception should have expired after two minutes")
	}
	// Only names count at level allowlist.
	named := []Exception{{ID: "x000005", Kind: KindAddr, Host: "pkg.example", Port: 8443, Created: now, Origin: "cli"}}
	if e := NameEntries(append(got, named...), now); len(e) != 1 || !e.Allows("pkg.example", 8443) || e.Allows("pkg.example", 443) {
		t.Errorf("name entries %v", e)
	}
	// Corrupt or hostile records are refused whole.
	for _, bad := range []string{
		`{"v":2,"exceptions":[]}`,
		`{"v":1,"exceptions":[],"x":1}`,
		`{"v":1,"exceptions":[{"id":"x1","kind":"lan","created":"2026-01-01T00:00:00Z","origin":"cli"}]}`,
		`{"v":1,"exceptions":[{"id":"x000001","kind":"lan","port":1,"created":"2026-01-01T00:00:00Z","origin":"cli"}]}`,
		`{"v":1,"exceptions":[{"id":"x000001","kind":"addr","host":"10.0.0.1","port":1,"addrs":["10.0.0.2"],"created":"2026-01-01T00:00:00Z","origin":"cli"}]}`,
		`{"v":1,"exceptions":[{"id":"x000001","kind":"host","port":1,"created":"2026-01-01T00:00:00Z","origin":"box"}]}`,
		`{"v":1,"exceptions":[{"id":"x000001","kind":"host","port":1,"created":"2026-01-01T00:00:00Z","origin":"cli"},{"id":"x000001","kind":"host","port":2,"created":"2026-01-01T00:00:00Z","origin":"cli"}]}`,
		`{"v":1,"exceptions":[]}]`, `{"v":1,"exceptions":[]}}`, // nothing after the record (finding G4-24)
	} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadExceptions(path); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	// A symlink is not followed.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "x.json")
	_ = WriteExceptions(other, list[:1], now)
	_ = os.Symlink(other, path)
	if _, err := ReadExceptions(path); err == nil {
		t.Error("read through a symlink")
	}
	// Nothing left in force: the file goes.
	if err := WriteExceptions(other, list[3:], now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Error("an empty record was kept")
	}
}

func TestExceptionScript(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	a := netip.MustParseAddr
	list := []Exception{
		{ID: "x000001", Kind: KindAddr, Host: "nas.local", Port: 22, Addrs: []netip.Addr{a("192.168.1.10"), a("192.168.1.11")}, Created: now, Until: until(now.Add(90 * time.Second)), Origin: "cli"},
		// The same element twice: the longer time wins.
		{ID: "x000002", Kind: KindAddr, Host: "192.168.1.10", Port: 22, Addrs: []netip.Addr{a("192.168.1.10")}, Created: now, Until: until(now.Add(10*time.Minute + 500*time.Millisecond)), Origin: "cli"},
		{ID: "x000003", Kind: KindLAN, Created: now, Origin: "cli"},
		{ID: "x000004", Kind: KindHost, Port: 5432, Created: now, Origin: "cli"},
		{ID: "x000005", Kind: KindAddr, Host: "192.168.1.12", Port: 80, Addrs: []netip.Addr{a("192.168.1.12")}, Created: now.Add(-time.Hour), Until: until(now.Add(-time.Second)), Origin: "cli"},
	}
	got, err := ExceptionScript(list, now, true)
	if err != nil {
		t.Fatal(err)
	}
	want := `flush set inet devc allow4
flush set inet devc lan4
add element inet devc allow4 { 192.168.1.10 . 22 timeout 601s, 192.168.1.11 . 22 timeout 90s }
add element inet devc lan4 { 10.0.0.0/8, 100.64.0.0/10, 172.16.0.0/12, 192.168.0.0/16 }
`
	if got != want {
		t.Errorf("script:\n%s\nwant:\n%s", got, want)
	}
	// Nothing in force: both sets are emptied.
	if got, _ := ExceptionScript(list[4:], now, true); got != "flush set inet devc allow4\nflush set inet devc lan4\n" {
		t.Errorf("empty script:\n%s", got)
	}
	// Rules from before the LAN set: address exceptions still work, a lan
	// exception does not.
	if got, err := ExceptionScript(list[:2], now, false); err != nil || strings.Contains(got, "lan4") {
		t.Errorf("old rules: %q %v", got, err)
	}
	if _, err := ExceptionScript(list, now, false); err == nil {
		t.Error("a lan exception without the LAN set was accepted")
	}

	// Applying: the rules are verified, then the script goes to nft in the
	// box's namespace.
	eng := engine.NewFake()
	listing := "table inet devc {\n\tset allow4 {\n\t}\n\tset lan4 {\n\t}\n\tchain output {\n\t\tip daddr 192.0.2.1 tcp dport 9 reject with tcp reset\n\t}\n}\n"
	eng.UnshareFunc = func(args []string, _ string) (execx.Result, error) {
		if slices.Contains(args, "list") {
			return execx.Result{Stdout: []byte(listing)}, nil
		}
		return execx.Result{}, nil
	}
	in := &Installer{Engine: eng, LookPath: func(n string) (string, error) { return "/usr/sbin/" + n, nil }}
	if err := in.ApplyExceptions(context.Background(), "/run/user/1000/netns/x", list, now); err != nil {
		t.Fatal(err)
	}
	if n := len(eng.UnshareInputs); n == 0 || eng.UnshareInputs[n-1] != want {
		t.Errorf("nft input: %q", eng.UnshareInputs)
	}
	listing = "table inet devc {\n}\n"
	if err := in.ApplyExceptions(context.Background(), "/run/user/1000/netns/x", list, now); err == nil {
		t.Error("applied to a namespace without devc's rules")
	}
}
