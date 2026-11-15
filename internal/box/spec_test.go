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
		Env: "base", MemoryGroup: "hello", Network: "internet",
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
	if args[len(args)-4] != "localhost/devc-env/base:latest" || !slices.Equal(args[len(args)-3:], []string{"/opt/devc/bin/devc", "box", "init"}) {
		t.Errorf("tail: %q", args[len(args)-4:])
	}
	hashLabel := args[len(args)-5]
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
