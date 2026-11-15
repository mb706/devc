package box

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/toolbox"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func testPaths(t *testing.T) *paths.Paths {
	t.Helper()
	env := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1001"}
	p, err := paths.Resolve(paths.Options{Home: "/home/u", Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func baseResolved() *config.Resolved {
	ref := names.ProjectRef{Realm: "demo", Project: "hello"}
	return &config.Resolved{
		Realm: "demo", Project: "hello", BoxName: names.BoxName(ref), BoxID: names.BoxID(ref),
		Env: "base", MemoryGroup: "hello", Network: "open",
		Box:    config.ResolvedBox{Memory: "16g", Pids: 4096},
		Mounts: []config.Mount{},
	}
}

func baseInputs(t *testing.T, r *config.Resolved) Inputs {
	p := testPaths(t)
	rt, _ := p.BoxRuntimeDir(r.BoxID)
	return Inputs{
		Resolved: r, Paths: p, Toolbox: toolbox.New(p.ToolboxDir()),
		Host:        HostFacts{Controllers: []string{"cpu", "memory", "pids"}},
		SELinuxMode: config.SELinuxLabels,
		ImageRef:    "localhost/devc-env/base:latest",
		Image:       engine.ImageInfo{ID: strings.Repeat("ab", 32), Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}},
		Seccomp:     []byte(`{"defaultAction":"SCMP_ACT_ERRNO"}`),
		RuntimeDir:  rt,
	}
}

func golden(t *testing.T, name string, s *Spec) {
	t.Helper()
	var b strings.Builder
	for _, a := range s.Args() {
		b.WriteString(a)
		b.WriteByte('\n')
	}
	for _, w := range s.Warnings {
		b.WriteString("# warning: " + w + "\n")
	}
	file := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run the test with -args -update to create it)", err)
	}
	if string(want) != b.String() {
		t.Errorf("%s differs from the golden file (rerun with -args -update after checking):\n%s", name, b.String())
	}
}

func TestSpecGolden(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		s, err := BuildSpec(baseInputs(t, baseResolved()))
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "basic", s)
	})
	t.Run("selinux-level-none-network-mounts", func(t *testing.T) {
		r := baseResolved()
		r.Network = "none"
		r.SELinuxLevel = "s0:c100,c200"
		r.MemoryGroup = "shared"
		r.Mounts = []config.Mount{{Project: "lib", Mode: "ro"}, {Project: "data", Mode: "rw"}}
		r.Box.Shell = "zsh"
		in := baseInputs(t, r)
		in.Host.SELinux = true
		s, err := BuildSpec(in)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "selinux-none-mounts", s)
	})
	t.Run("selinux-disable-no-cgroups-open", func(t *testing.T) {
		r := baseResolved()
		r.Network = "open"
		r.SELinuxLevel = "s0:c1,c2" // ignored when labelling is disabled
		r.Image, r.Env = "docker.io/library/debian:trixie-slim", ""
		// Without delegated controllers the limits must be switched off
		// explicitly.
		r.Box.Memory, r.Box.Pids = config.NoLimit, config.NoPidsLimit
		in := baseInputs(t, r)
		in.Host = HostFacts{SELinux: true}
		in.SELinuxMode = config.SELinuxDisable
		in.ImageRef = r.Image
		in.Image.Env = nil
		s, err := BuildSpec(in)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "selinux-disable-open", s)
	})
	t.Run("contributed", func(t *testing.T) {
		c := ContributorFunc(func(in *Inputs, s *Spec) error {
			if err := s.AddMount(Mount{Source: "/state/harness/claude/projects", Target: "/home/dev/.claude/projects"}); err != nil {
				return err
			}
			return s.AddEnv("CLAUDE_CONFIG_DIR", "/home/dev/.claude")
		})
		s, err := BuildSpec(baseInputs(t, baseResolved()), c)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "contributed", s)
	})
}

func TestSpecProperties(t *testing.T) {
	in := baseInputs(t, baseResolved())
	s, err := BuildSpec(in)
	if err != nil {
		t.Fatal(err)
	}
	args := s.Args()
	// The fixed hardening is always present.
	for _, want := range []string{"--init", "--userns=keep-id:uid=1000,gid=1000", "--cap-drop=ALL", "no-new-privileges", "--http-proxy=false"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s", want)
		}
	}
	// devc's init is the entrypoint, whatever the image declares.
	if args[len(args)-3] != "localhost/devc-env/base:latest" || !slices.Equal(args[len(args)-2:], []string{"box", "init"}) {
		t.Errorf("tail: %q", args[len(args)-3:])
	}
	if i := slices.Index(args, "--entrypoint"); i < 0 || args[i+1] != "/opt/devc/bin/devc" {
		t.Errorf("entrypoint: %q", args)
	}
	if !slices.Contains(args, "TERMINFO_DIRS=/opt/devc/terminfo:") {
		t.Errorf("no TERMINFO_DIRS: %q", args)
	}
	hashLabel := args[len(args)-4]
	if !strings.HasPrefix(hashLabel, LabelConfigHash+"=") || strings.TrimPrefix(hashLabel, LabelConfigHash+"=") != s.Hash() {
		t.Errorf("hash label %q", hashLabel)
	}
	// Deterministic.
	s2, _ := BuildSpec(baseInputs(t, baseResolved()))
	if s2.Hash() != s.Hash() || !slices.Equal(s2.Args(), args) {
		t.Error("two builds of the same inputs differ")
	}
	// Each input that matters changes the hash.
	changes := map[string]func(*Inputs){
		"image id":  func(i *Inputs) { i.Image.ID = strings.Repeat("cd", 32) },
		"seccomp":   func(i *Inputs) { i.Seccomp = []byte(`{}`) },
		"memory":    func(i *Inputs) { i.Resolved.Box.Memory = "8g" },
		"network":   func(i *Inputs) { i.Resolved.Network = "none" },
		"selinux":   func(i *Inputs) { i.Host.SELinux = true; i.Resolved.SELinuxLevel = "s0:c1,c2" },
		"image ref": func(i *Inputs) { i.ImageRef = "localhost/devc-env/other:latest" },
	}
	for name, change := range changes {
		i := baseInputs(t, baseResolved())
		change(&i)
		s3, err := BuildSpec(i)
		if err != nil {
			t.Fatal(err)
		}
		if s3.Hash() == s.Hash() {
			t.Errorf("%s does not change the hash", name)
		}
	}
	// Box PATH puts devc first and keeps the image's.
	for _, e := range s.Env {
		if e.Name == "PATH" && e.Value != "/opt/devc/shims:/opt/devc/bin:/home/dev/.local/bin:/usr/local/bin:/usr/bin:/bin" {
			t.Errorf("PATH = %s", e.Value)
		}
	}
}

func TestSpecRejects(t *testing.T) {
	s, err := BuildSpec(baseInputs(t, baseResolved()))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []Mount{
		{Source: "/a,b", Target: "/x"},
		{Source: "rel", Target: "/x"},
		{Source: "/a", Target: "/x/../y"},
		{Source: "/a", Target: "/home/dev"}, // duplicate target
	} {
		if err := s.AddMount(m); err == nil {
			t.Errorf("AddMount(%+v) accepted", m)
		}
	}
	for _, kv := range [][2]string{{"1X", "v"}, {"A-B", "v"}, {"A", "x\ny"}} {
		if err := s.AddEnv(kv[0], kv[1]); err == nil {
			t.Errorf("AddEnv(%q) accepted", kv[0])
		}
	}
	for _, k := range []string{LabelConfigHash, LabelRealm, "a=b", "-x"} {
		if err := s.AddLabel(k, "v"); err == nil {
			t.Errorf("AddLabel(%q) accepted", k)
		}
	}
	in := baseInputs(t, baseResolved())
	in.Image.ID = ""
	if _, err := BuildSpec(in); err == nil {
		t.Error("spec without an image")
	}
	in = baseInputs(t, baseResolved())
	in.Resolved.Network = "lan"
	if _, err := BuildSpec(in); err == nil {
		t.Error("unknown network level accepted")
	}
	// A level without enforcement fails closed (finding A5).
	in = baseInputs(t, baseResolved())
	in.Resolved.Network = "allowlist"
	if _, err := BuildSpec(in); err == nil || !strings.Contains(err.Error(), "not available yet") {
		t.Errorf("network allowlist: %v", err)
	}
	// Nested containers are not implemented (finding C2).
	in = baseInputs(t, baseResolved())
	in.Resolved.Capabilities.Nested = true
	if _, err := BuildSpec(in); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Errorf("nested: %v", err)
	}
}

// TestSpecLimits: a configured limit that cannot be applied fails; "none"
// and -1 switch a limit off (finding A7).
func TestSpecLimits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		memory      string
		pids        int
		controllers []string
		want        []string // limit arguments, nil for an error
	}{
		{"both", "8g", 100, []string{"memory", "pids"}, []string{"--pids-limit", "100", "--memory", "8g"}},
		{"no memory controller", "8g", 100, []string{"pids"}, nil},
		{"no pids controller", "8g", 100, []string{"memory"}, nil},
		{"memory off", "none", 100, []string{"pids"}, []string{"--pids-limit", "100"}},
		{"pids off", "8g", -1, []string{"memory"}, []string{"--memory", "8g"}},
		{"both off", "none", -1, nil, []string{}},
	} {
		r := baseResolved()
		r.Box.Memory, r.Box.Pids = tc.memory, tc.pids
		in := baseInputs(t, r)
		in.Host.Controllers = tc.controllers
		s, err := BuildSpec(in)
		switch {
		case tc.want == nil && (err == nil || !strings.Contains(err.Error(), "not delegated")):
			t.Errorf("%s: %v, want an error", tc.name, err)
		case tc.want != nil && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != nil && !slices.Equal(append([]string{}, s.Limits...), tc.want):
			t.Errorf("%s: limits %q, want %q", tc.name, s.Limits, tc.want)
		}
	}
}

// TestSpecDevices: gpu and kvm add their devices (finding C2; not testable
// on this host beyond the arguments).
func TestSpecDevices(t *testing.T) {
	r := baseResolved()
	r.Capabilities.GPU, r.Capabilities.KVM = true, true
	s, err := BuildSpec(baseInputs(t, r))
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(s.Args(), " ")
	for _, want := range []string{"--device nvidia.com/gpu=all", "--device /dev/kvm", "--group-add keep-groups"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
	plain, _ := BuildSpec(baseInputs(t, baseResolved()))
	if strings.Contains(strings.Join(plain.Args(), " "), "--device") || plain.Hash() == s.Hash() {
		t.Error("devices without the capabilities, or no hash change")
	}
}

func TestInBoxPath(t *testing.T) {
	p := testPaths(t)
	ref := names.ProjectRef{Realm: "demo", Project: "hello"}
	for host, want := range map[string]string{
		"/home/u/devc/demo/hello":              "/w/demo/hello",
		"/home/u/devc/demo/hello/main/src/":    "/w/demo/hello/main/src",
		"/home/u/devc/demo/hello2":             "",
		"/home/u/devc/demo":                    "",
		"/elsewhere":                           "",
		"/home/u/devc/demo/hello/../hello/sub": "/w/demo/hello/sub",
	} {
		got, ok := InBoxPath(p, ref, host)
		if got != want || ok != (want != "") {
			t.Errorf("InBoxPath(%s) = %q, %v; want %q", host, got, ok, want)
		}
	}
}
