package gui

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/display"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/paths"
)

var hello = names.ProjectRef{Realm: "demo", Project: "hello"}

const boxID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var gen64 = strings.Repeat("ab", 32)

func TestSidecarArgs(t *testing.T) {
	o := SidecarOptions{Ref: hello, BoxID: boxID, BoxStart: "1234@1700000000000000000", Seccomp: "/s/toolbox/seccomp.json",
		Labels: []string{"label=level:s0:c1,c2"}, Image: Image,
		Limits: config.GlobalGUI{Memory: "2g", Pids: 512, CPUs: 2}, Controllers: []string{"cpu", "memory", "pids"}, Generation: gen64}
	args, err := SidecarArgs(o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--name", "devc-demo-hello_gui",
		"--label", "devc.realm=demo", "--label", "devc.project=hello", "--label", "devc.role=aux", "--label", "devc.aux=desktop",
		"--label", "devc.gui.box=" + boxID, "--label", "devc.gui.box-start=1234@1700000000000000000",
		"--label", "devc.gui.limits=memory=2147483648 pids=512 cpus=2", "--label", "devc.gui.image=" + gen64,
		"--network", "container:" + boxID,
		"--userns=container:" + boxID,
		"--user", "1000:1000",
		"--cap-drop=ALL",
		"--security-opt", "no-new-privileges",
		"--security-opt", "seccomp=/s/toolbox/seccomp.json",
		"--security-opt", "label=level:s0:c1,c2",
		"--memory", "2147483648", "--pids-limit", "512", "--cpus", "2",
		"--read-only", "--read-only-tmpfs=false",
		"--mount", "type=tmpfs,destination=/tmp",
		"--init", "--pull=never", "--http-proxy=false",
		"--env", "DEVC_DESKTOP_NAME=devc demo/hello",
		Image, "/usr/local/bin/devc-desktop", "run",
	}
	if !slices.Equal(args, want) {
		t.Errorf("args:\n%q\nwant\n%q", args, want)
	}
	// Exactly one mount, a tmpfs, and no other kind of volume.
	mounts := 0
	for i, a := range args {
		if a == "--mount" {
			mounts++
			if !strings.HasPrefix(args[i+1], "type=tmpfs,") {
				t.Errorf("mount %q", args[i+1])
			}
		}
		if a == "-v" || a == "--volume" || strings.HasPrefix(a, "--cap-add") || a == "--privileged" {
			t.Errorf("argument %q", a)
		}
	}
	if mounts != 1 {
		t.Errorf("%d mounts", mounts)
	}
	for name, mod := range map[string]func(*SidecarOptions){
		"short box id":   func(o *SidecarOptions) { o.BoxID = "abc" },
		"box name":       func(o *SidecarOptions) { o.BoxID = "devc-demo-hello" },
		"box start":      func(o *SidecarOptions) { o.BoxStart = "x" },
		"seccomp comma":  func(o *SidecarOptions) { o.Seccomp = "/a,b" },
		"seccomp rel":    func(o *SidecarOptions) { o.Seccomp = "seccomp.json" },
		"label option":   func(o *SidecarOptions) { o.Labels = []string{"label=type:spc_t"} },
		"no image":       func(o *SidecarOptions) { o.Image = "" },
		"invalid ref":    func(o *SidecarOptions) { o.Ref = names.ProjectRef{Realm: "Demo", Project: "x"} },
		"label injected": func(o *SidecarOptions) { o.Labels = []string{"label=disable\n--privileged"} },
		"no controllers": func(o *SidecarOptions) { o.Controllers = nil },
		"no limits":      func(o *SidecarOptions) { o.Limits = config.GlobalGUI{} },
		"no generation":  func(o *SidecarOptions) { o.Generation = "" },
		"bad generation": func(o *SidecarOptions) { o.Generation = "abc" },
	} {
		oo := o
		mod(&oo)
		if _, err := SidecarArgs(oo); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if SidecarName(hello) == names.BoxName(hello) || strings.ContainsAny(names.BoxName(hello), "_") {
		t.Error("sidecar name could be a box name")
	}
}

// TestLimitArgs: every limit needs its controller, "none" and -1 switch a
// limit off, and invalid values are refused (finding D2-01).
func TestLimitArgs(t *testing.T) {
	all := []string{"cpu", "memory", "pids"}
	def := config.GlobalGUI{Memory: "2g", Pids: 512, CPUs: 2}
	for _, c := range []struct {
		name  string
		l     config.GlobalGUI
		ctrls []string
		want  []string
		err   string
	}{
		{"defaults", def, all, []string{"--memory", "2147483648", "--pids-limit", "512", "--cpus", "2"}, ""},
		{"fraction", config.GlobalGUI{Memory: "512m", Pids: 64, CPUs: 0.5}, all, []string{"--memory", "536870912", "--pids-limit", "64", "--cpus", "0.5"}, ""},
		{"all off", config.GlobalGUI{Memory: "none", Pids: -1, CPUs: -1}, nil, nil, ""},
		{"memory off", config.GlobalGUI{Memory: "none", Pids: 512, CPUs: 2}, []string{"cpu", "pids"}, []string{"--pids-limit", "512", "--cpus", "2"}, ""},
		{"no memory controller", def, []string{"cpu", "pids"}, nil, "memory are not delegated"},
		{"no pids controller", def, []string{"cpu", "memory"}, nil, "pids are not delegated"},
		{"no cpu controller", def, []string{"memory", "pids"}, nil, "cpu are not delegated"},
		{"nothing delegated", def, nil, nil, `memory = "none", pids = -1 and cpus = -1 under [gui]`},
		{"bad memory", config.GlobalGUI{Memory: "lots", Pids: 1, CPUs: 1}, all, nil, "not a size"},
		{"empty memory", config.GlobalGUI{Memory: "", Pids: 1, CPUs: 1}, all, nil, "not a size"},
		{"zero pids", config.GlobalGUI{Memory: "1g", Pids: 0, CPUs: 1}, all, nil, "not a limit"},
		{"zero cpus", config.GlobalGUI{Memory: "1g", Pids: 1, CPUs: 0}, all, nil, "not a limit"},
	} {
		got, err := LimitArgs(c.l, c.ctrls)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: %q %v, want an error with %q", c.name, got, err, c.err)
		case c.err == "" && (err != nil || !slices.Equal(got, c.want)):
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	// The record of the limits (E2-01): what the arguments apply, "none"
	// for a limit switched off; independent of delegation.
	for l, want := range map[config.GlobalGUI]string{
		def:                                   "memory=2147483648 pids=512 cpus=2",
		{Memory: "512m", Pids: 64, CPUs: 0.5}: "memory=536870912 pids=64 cpus=0.5",
		{Memory: "none", Pids: -1, CPUs: -1}:  "memory=none pids=none cpus=none",
		{Memory: "none", Pids: 512, CPUs: -1}: "memory=none pids=512 cpus=none",
	} {
		if got, err := LimitsSpec(l); err != nil || got != want {
			t.Errorf("LimitsSpec(%+v) = %q %v, want %q", l, got, err, want)
		}
	}
	if _, err := LimitsSpec(config.GlobalGUI{Memory: "lots", Pids: 1, CPUs: 1}); err == nil {
		t.Error("LimitsSpec accepted an invalid memory limit")
	}
}

// seqReader returns 0, 1, 2, … as random bytes.
type seqReader struct{ n byte }

func (r *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.n
		r.n++
	}
	return len(p), nil
}

type constReader byte

func (c constReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c)
	}
	return len(p), nil
}

func TestSecrets(t *testing.T) {
	s, err := NewSecrets(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cookie) != 16 || len(s.Password) != 8 {
		t.Fatalf("secrets %x %q", s.Cookie, s.Password)
	}
	for _, c := range s.Password {
		if !strings.ContainsRune(passwordAlphabet, c) {
			t.Errorf("password character %q", c)
		}
	}
	s2, _ := NewSecrets(nil)
	if bytes.Equal(s.Cookie, s2.Cookie) || s.Password == s2.Password {
		t.Error("two starts drew the same secrets")
	}
	// Deterministic source: cookie first, then the password; bytes at or
	// above 248 (the largest multiple of 62 below 256) are skipped.
	d, err := NewSecrets(&seqReader{})
	if err != nil || !bytes.Equal(d.Cookie, []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}) || d.Password != "QRSTUVWX" {
		t.Errorf("deterministic: %x %q %v", d.Cookie, d.Password, err)
	}
	if _, err := NewSecrets(constReader(250)); err == nil {
		t.Error("a source of rejected bytes did not fail")
	}
	if _, err := NewSecrets(bytes.NewReader([]byte{1, 2})); err == nil {
		t.Error("a short source did not fail")
	}

	arch, err := s.Archive()
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(arch))
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg || h.Mode != 0o600 {
			t.Errorf("%s: type %c mode %o", h.Name, h.Typeflag, h.Mode)
		}
		files[h.Name], _ = io.ReadAll(tr)
	}
	xa, _ := s.XAuthority()
	pw, _ := display.VNCPasswordFile(s.Password)
	if len(files) != 2 || !bytes.Equal(files["xauthority"], xa) || !bytes.Equal(files["vncpasswd"], pw) {
		t.Errorf("archive: %v", files)
	}
	es, err := display.ParseXAuthority(xa)
	if err != nil || len(es) != 1 || es[0].Family != display.FamilyWild || es[0].Number != "1" || !bytes.Equal(es[0].Data, s.Cookie) {
		t.Errorf("xauthority %+v %v", es, err)
	}
}

func TestViewerURL(t *testing.T) {
	v := Viewer{Password: "Ab3dEf9h", Bind: "127.0.0.1", HostPort: 6080}
	if v.URL() != "http://127.0.0.1:6080/vnc.html#autoconnect=1&resize=remote&password=Ab3dEf9h" || v.BaseURL() != "http://127.0.0.1:6080/vnc.html" {
		t.Errorf("%s %s", v.URL(), v.BaseURL())
	}
	v.Bind = "0.0.0.0"
	if !strings.HasPrefix(v.URL(), "http://127.0.0.1:6080/") {
		t.Error(v.URL())
	}
	v.Bind = "::1"
	if !strings.HasPrefix(v.URL(), "http://[::1]:6080/") {
		t.Error(v.URL())
	}
}

// --- the manager against fakes ---

type rigProc struct{}

func (rigProc) StartTime(pid int) (uint64, error) { return uint64(pid) * 100, nil }
func (rigProc) NetNS(int) (uint64, error)         { return 0, fs.ErrPermission }
func (rigProc) BootID() (string, error)           { return "2ca2e1dd-4048-4a52-9349-e4d44c4025dd", nil }

type fakeForwards struct {
	started []string
	stopped []string
	alive   map[string]bool
	err     error
}

func (f *fakeForwards) Start(_ context.Context, ref names.ProjectRef, boxPort int, bind string, hostPort int, explicit bool) (string, int, error) {
	if f.err != nil {
		return "", 0, f.err
	}
	id := fmt.Sprintf("f%06x", len(f.started)+1)
	f.started = append(f.started, fmt.Sprintf("%s %s %d %s %d %v", id, ref, boxPort, bind, hostPort, explicit))
	f.alive[id] = true
	return id, hostPort, nil
}

func (f *fakeForwards) Stop(id string) error {
	f.stopped = append(f.stopped, id)
	delete(f.alive, id)
	return nil
}

func (f *fakeForwards) Alive(id string) bool { return f.alive[id] }

type rig struct {
	t     *testing.T
	base  string
	eng   *engine.Fake
	box   *box.Manager
	gui   *Manager
	fwd   *fakeForwards
	out   *bytes.Buffer
	rules string
	// secrets is what the sidecar received; ready answers the ready check.
	secrets []byte
	ready   func() (execx.Result, error)
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// newRig is a configuration from examples/config (level internet for
// demo/hello) with a fake engine whose box becomes ready and whose
// network rules can be listed.
func newRig(t *testing.T) *rig {
	t.Helper()
	base := t.TempDir()
	r := &rig{t: t, base: base, eng: engine.NewFake(), out: &bytes.Buffer{}, fwd: &fakeForwards{alive: map[string]bool{}}}
	cfg := filepath.Join(base, "config")
	copyTree(t, filepath.Join("..", "..", "examples", "config"), cfg)
	if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("projects_root = \""+filepath.Join(base, "projects")+"\"\ndefault_env = \"base\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{paths.EnvConfigDir: cfg, paths.EnvStateDir: filepath.Join(base, "state"), paths.EnvRuntimeDir: filepath.Join(base, "run")}
	p, err := paths.Resolve(paths.Options{Home: filepath.Join(base, "home"), Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := config.Load(p)
	exe := filepath.Join(base, "devc-binary")
	if err := os.WriteFile(exe, []byte("#!fake devc\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := box.NewManager(c, r.eng, execx.NewFake(), exe, "test", r.out)
	m.HostRoot = filepath.Join(base, "hostroot")
	m.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	m.ReadyTimeout = 2 * time.Second
	m.HostAddrs = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.168.1.5")}, nil }
	m.Net = &netpolicy.Installer{Engine: r.eng, HostAddrs: m.HostAddrs, Proc: rigProc{},
		LookPath: func(n string) (string, error) { return "/usr/sbin/" + n, nil }}
	r.box = m
	r.gui = &Manager{Box: m, Engine: r.eng, Paths: p, Toolbox: m.Toolbox, ImageContext: filepath.Join(base, "cache", "aux", "desktop"),
		Forwards: r.fwd, Out: r.out, Now: func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }}
	m.Dependents = append(m.Dependents, r.gui)

	r.eng.UserData = filepath.Join(base, "userdata")
	r.eng.DefaultNetwork = "slirp4netns"
	r.eng.ResolvConf = "nameserver 10.0.2.3\n"
	r.eng.UnshareFunc = func(args []string, stdin string) (execx.Result, error) {
		switch {
		case strings.HasSuffix(args[0], "/stat"):
			path := args[len(args)-1]
			var n uint64
			_, _ = fmt.Sscanf(path[strings.LastIndexByte(path, '-')+1:], "%d", &n)
			return execx.Result{Stdout: []byte(fmt.Sprintf("%d\n", 4026531000+n))}, nil
		case slices.Contains(args, "-f"):
			r.rules = stdin
			return execx.Result{}, nil
		}
		return execx.Result{Stdout: []byte(r.rules)}, nil
	}
	r.ready = func() (execx.Result, error) { return execx.Result{}, nil }
	r.eng.ExecFunc = func(o engine.ExecOptions) (execx.Result, error) {
		switch {
		case len(o.Cmd) > 4 && o.Cmd[2] == "net-canary":
			var probes []string
			for _, a := range o.Cmd[5:] {
				probes = append(probes, fmt.Sprintf(`{"addr":"%s:%s","result":"unreachable"}`, a, o.Cmd[4]))
			}
			return execx.Result{Stdout: []byte("[" + strings.Join(probes, ",") + "]\n")}, nil
		case slices.Equal(o.Cmd, []string{"/opt/devc/bin/devc", "box", "fix-identity"}):
			rt, _ := p.BoxRuntimeDir(names.BoxID(hello))
			_ = os.WriteFile(filepath.Join(rt, "ready"), nil, 0o644)
		case slices.Equal(o.Cmd, []string{entry, "secrets"}):
			r.secrets, _ = io.ReadAll(o.Stdin)
		case slices.Equal(o.Cmd, []string{entry, "ready"}):
			return r.ready()
		}
		return execx.Result{}, nil
	}
	return r
}

func (r *rig) boxUp() {
	r.t.Helper()
	if err := r.box.Up(context.Background(), hello, box.UpOptions{}); err != nil {
		r.t.Fatalf("up: %v\n%s", err, r.out)
	}
}

func (r *rig) cookiePath() string {
	rt, _ := r.gui.Paths.BoxRuntimeDir(names.BoxID(hello))
	return filepath.Join(rt, XAuthorityName)
}

func (r *rig) envPath() string {
	return filepath.Join(r.gui.Paths.BoxEtcDir(hello), boxside.DisplayEnvName)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestUpDownStatus(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	st, err := r.gui.Status(ctx, hello)
	if err != nil || st.State != "absent" {
		t.Fatalf("status before: %+v %v", st, err)
	}
	// gui up starts the box first.
	v, err := r.gui.Up(ctx, hello, UpOptions{})
	if err != nil {
		t.Fatalf("gui up: %v\n%s", err, r.out)
	}
	boxCI := r.eng.Containers["devc-demo-hello"]
	side := r.eng.Containers["devc-demo-hello_gui"]
	if !boxCI.State.Running || !side.State.Running {
		t.Fatalf("box %+v sidecar %+v", boxCI.State, side.State)
	}
	if len(r.eng.Builds) != 2 || r.eng.Builds[1].Tag != Image || r.eng.Builds[1].Labels["devc.aux"] != "desktop" {
		t.Errorf("builds: %+v", r.eng.Builds)
	}
	args := r.eng.CreateArgs["devc-demo-hello_gui"]
	if !slices.Contains(args, "container:"+boxCI.ID) || !slices.Contains(args, "--userns=container:"+boxCI.ID) ||
		!slices.Contains(args, "devc.gui.box-start="+StartID(&boxCI)) || !slices.Contains(args, "seccomp="+r.box.Toolbox.SeccompPath()) {
		t.Errorf("create args %q", args)
	}
	// The default limits of [gui] (finding D2-01).
	if hc := side.HostConfig; hc.Memory != 2<<30 || hc.PidsLimit != 512 || hc.NanoCpus != 2e9 {
		t.Errorf("sidecar limits %+v", hc)
	}
	// The secrets went in on standard input, as root of nothing: as the
	// box's user, never in arguments or the environment.
	var execs []engine.ExecOptions
	for _, e := range r.eng.Execs {
		if e.Container == "devc-demo-hello_gui" {
			execs = append(execs, e)
		}
	}
	if len(execs) != 2 || execs[0].User != "1000:1000" || !execs[0].Interactive || len(execs[0].Env) != 0 || execs[1].Cmd[1] != "ready" {
		t.Errorf("sidecar execs %+v", execs)
	}
	if len(r.secrets) == 0 || strings.Contains(strings.Join(args, " "), v.Password) {
		t.Error("secrets not delivered on stdin, or the password is in the arguments")
	}
	tr := tar.NewReader(bytes.NewReader(r.secrets))
	h, err := tr.Next()
	if err != nil || h.Name != "xauthority" {
		t.Fatalf("secrets archive: %v %v", h, err)
	}
	xa, _ := io.ReadAll(tr)
	// The box got the same cookie, 0600, and the display file.
	fi, err := os.Lstat(r.cookiePath())
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cookie file %v %v", fi, err)
	}
	if b, _ := os.ReadFile(r.cookiePath()); !bytes.Equal(b, xa) {
		t.Error("box cookie differs from the display's")
	}
	if b, _ := os.ReadFile(r.envPath()); string(b) != "DISPLAY=127.0.0.1:1\nXAUTHORITY=/run/devc/display.xauth\n" {
		t.Errorf("display.env %q", b)
	}
	if len(r.fwd.started) != 1 || r.fwd.started[0] != "f000001 demo/hello 6080 127.0.0.1 6080 false" || v.Forward != "f000001" || v.HostPort != 6080 {
		t.Errorf("forwards %v, viewer %+v", r.fwd.started, v)
	}
	if !strings.HasSuffix(v.URL(), "#autoconnect=1&resize=remote&password="+v.Password) || len(v.Password) != 8 {
		t.Errorf("url %s", v.URL())
	}
	// The record keeps the password for the host only.
	rp, _ := r.gui.recordPath(hello)
	if fi, err := os.Stat(rp); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("record %v %v", fi, err)
	}
	st, err = r.gui.Status(ctx, hello)
	if err != nil || st.State != "running" || st.Viewer != "http://127.0.0.1:6080/vnc.html" || st.Display != Display || st.Forward != "f000001" {
		t.Errorf("status running: %+v %v", st, err)
	}

	// A second up keeps the sidecar and prints the same address.
	r.eng.Log = nil
	v2, err := r.gui.Up(ctx, hello, UpOptions{})
	if err != nil || !v2.Reused || v2.Password != v.Password || v2.Forward != v.Forward || len(r.eng.Log) != 0 {
		t.Errorf("second up: %+v %v %v", v2, err, r.eng.Log)
	}
	// Its forward ended: a new one, same sidecar.
	delete(r.fwd.alive, v.Forward)
	v3, err := r.gui.Up(ctx, hello, UpOptions{})
	if err != nil || !v3.Reused || v3.Forward != "f000002" || v3.Password != v.Password {
		t.Errorf("up after the forward ended: %+v %v", v3, err)
	}
	// --restart draws new secrets.
	v4, err := r.gui.Up(ctx, hello, UpOptions{Restart: true})
	if err != nil || v4.Reused || v4.Password == v.Password || !slices.Contains(r.fwd.stopped, "f000002") {
		t.Errorf("restart: %+v %v %v", v4, err, r.fwd.stopped)
	}

	// devc down removes the sidecar before it stops the box.
	r.eng.Log = nil
	if err := r.box.Down(ctx, hello); err != nil {
		t.Fatal(err)
	}
	iRm, iStop := slices.Index(r.eng.Log, "rm devc-demo-hello_gui"), slices.Index(r.eng.Log, "stop devc-demo-hello")
	if iRm < 0 || iStop < 0 || iRm > iStop {
		t.Errorf("down order: %v", r.eng.Log)
	}
	if exists(r.cookiePath()) || exists(r.envPath()) || exists(rp) || r.fwd.alive[v4.Forward] {
		t.Error("files or the forward survived down")
	}
	if st, _ := r.gui.Status(ctx, hello); st.State != "absent" {
		t.Errorf("status after down: %+v", st)
	}
}

func TestRemoveAndStartRemoveSidecar(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if _, err := r.gui.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	// rm: the sidecar goes first, or the engine would refuse.
	r.eng.Log = nil
	if err := r.box.Remove(ctx, hello, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.eng.Log, []string{"rm devc-demo-hello_gui", "rm devc-demo-hello"}) {
		t.Errorf("rm order: %v", r.eng.Log)
	}

	// A sidecar left from an earlier start (the box stopped outside devc)
	// is stale, and the next start removes it before the box starts.
	if _, err := r.gui.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.eng.Stop(ctx, "devc-demo-hello", 0); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.gui.Status(ctx, hello); st.State != "stale" || !strings.Contains(st.Reason, "not running") {
		t.Errorf("status: %+v", st)
	}
	r.eng.Log = nil
	r.boxUp()
	if len(r.eng.Log) < 2 || r.eng.Log[0] != "rm devc-demo-hello_gui" {
		t.Errorf("start: %v", r.eng.Log)
	}
	if exists(r.envPath()) {
		t.Error("display.env survived a new start")
	}
	// A sidecar of an earlier start of a running box is stale too.
	if _, err := r.gui.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	ci := r.eng.Containers["devc-demo-hello_gui"]
	ci.Labels[LabelBoxStart] = "1@1"
	r.eng.Put("devc-demo-hello_gui", ci)
	if st, _ := r.gui.Status(ctx, hello); st.State != "stale" || !strings.Contains(st.Reason, "restarted") {
		t.Errorf("status: %+v", st)
	}
	// gui up replaces it rather than reusing it.
	v, err := r.gui.Up(ctx, hello, UpOptions{})
	if err != nil || v.Reused {
		t.Errorf("up over a stale sidecar: %+v %v", v, err)
	}
	// gui down, twice.
	if err := r.gui.Down(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.eng.Containers["devc-demo-hello_gui"]; ok {
		t.Error("sidecar survived gui down")
	}
	if err := r.gui.Down(ctx, hello); err != nil || !strings.Contains(r.out.String(), "no desktop runs") {
		t.Errorf("second down: %v", err)
	}
}

// TestOutdatedSidecar: a running desktop is reused only with the limits
// and the image generation gui up would create now; otherwise gui up
// refuses it without touching it, status calls it outdated, and only
// --restart replaces it (finding E2-01).
func TestOutdatedSidecar(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	v, err := r.gui.Up(ctx, hello, UpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	side := r.eng.Containers["devc-demo-hello_gui"]
	if side.Labels[LabelLimits] != "memory=2147483648 pids=512 cpus=2" || side.Labels[LabelImage] != AuxImage().Hash() {
		t.Fatalf("labels %v", side.Labels)
	}
	refused := func(name string, want ...string) {
		t.Helper()
		r.eng.Log = nil
		started := len(r.fwd.started)
		_, err := r.gui.Up(ctx, hello, UpOptions{})
		if err == nil || !strings.Contains(err.Error(), "'devc gui up --restart demo/hello'") {
			t.Errorf("%s: up %v", name, err)
		} else {
			for _, w := range want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%s: %q does not name %q", name, err, w)
				}
			}
		}
		// Nothing was removed, created, started or forwarded.
		for _, l := range r.eng.Log {
			if strings.HasPrefix(l, "rm ") || strings.HasPrefix(l, "create ") || strings.HasPrefix(l, "start ") || strings.HasPrefix(l, "build ") {
				t.Errorf("%s: %s", name, l)
			}
		}
		if ci := r.eng.Containers["devc-demo-hello_gui"]; !ci.State.Running || len(r.fwd.started) != started {
			t.Errorf("%s: the sidecar was disturbed", name)
		}
		if st, err := r.gui.Status(ctx, hello); err != nil || st.State != "outdated" || !strings.Contains(st.Reason, "--restart") {
			t.Errorf("%s: status %+v %v", name, st, err)
		}
	}

	// A desktop created by an earlier devc: no limits, no records.
	old := side
	old.Labels = map[string]string{}
	for k, val := range side.Labels {
		if k != LabelLimits && k != LabelImage {
			old.Labels[k] = val
		}
	}
	old.HostConfig.Memory, old.HostConfig.PidsLimit, old.HostConfig.NanoCpus = 0, 0, 0
	r.eng.Put("devc-demo-hello_gui", old)
	refused("created without limits", "recorded no resource limits", "an earlier devc")
	// --restart replaces it with one that has its limits and records.
	v2, err := r.gui.Up(ctx, hello, UpOptions{Restart: true})
	if err != nil || v2.Reused || v2.Password == v.Password {
		t.Fatalf("restart: %+v %v", v2, err)
	}
	side = r.eng.Containers["devc-demo-hello_gui"]
	if hc := side.HostConfig; hc.Memory != 2<<30 || hc.PidsLimit != 512 || hc.NanoCpus != 2e9 || side.Labels[LabelImage] != AuxImage().Hash() {
		t.Errorf("after the restart: %+v %v", hc, side.Labels)
	}
	if v3, err := r.gui.Up(ctx, hello, UpOptions{}); err != nil || !v3.Reused || v3.Password != v2.Password {
		t.Errorf("up after the restart: %+v %v", v3, err)
	}

	// The configuration's limits changed since it was created.
	r.box.Config.Global.GUI.Memory = "1g"
	refused("other limits", "memory=2147483648 pids=512 cpus=2", "memory=1073741824 pids=512 cpus=2")
	r.box.Config.Global.GUI.Memory = "2g"
	if v3, err := r.gui.Up(ctx, hello, UpOptions{}); err != nil || !v3.Reused {
		t.Errorf("control, the limits as before: %+v %v", v3, err)
	}
	if st, _ := r.gui.Status(ctx, hello); st.State != "running" {
		t.Errorf("status of a current desktop: %+v", st)
	}

	// Another image generation.
	ci := r.eng.Containers["devc-demo-hello_gui"]
	ci.Labels[LabelImage] = strings.Repeat("0", 64)
	r.eng.Put("devc-demo-hello_gui", ci)
	refused("other image", "image generation 000000000000", AuxImage().Hash()[:12])
	ci.Labels[LabelImage] = AuxImage().Hash()
	r.eng.Put("devc-demo-hello_gui", ci)

	// Without its record devc cannot print the viewer: refused as well.
	rp, _ := r.gui.recordPath(hello)
	if err := os.Remove(rp); err != nil {
		t.Fatal(err)
	}
	r.eng.Log = nil
	if _, err := r.gui.Up(ctx, hello, UpOptions{}); err == nil || !strings.Contains(err.Error(), "no record") || !strings.Contains(err.Error(), "--restart") {
		t.Errorf("up without a record: %v", err)
	}
	if st, _ := r.gui.Status(ctx, hello); st.State != "running" || !strings.Contains(st.Reason, "no record") {
		t.Errorf("status without a record: %+v", st)
	}
	if v4, err := r.gui.Up(ctx, hello, UpOptions{Restart: true}); err != nil || v4.Reused {
		t.Errorf("restart without a record: %+v %v", v4, err)
	}
}

func TestUpFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("not ready", func(t *testing.T) {
		r := newRig(t)
		r.ready = func() (execx.Result, error) {
			res := execx.Result{ExitCode: 1, Stderr: []byte("devc-desktop: a process of the desktop has ended:\nwebsockify: bind: Address already in use\n")}
			return res, &execx.ExitError{Result: res}
		}
		_, err := r.gui.Up(ctx, hello, UpOptions{})
		if err == nil || !strings.Contains(err.Error(), "Address already in use") || !strings.Contains(err.Error(), "podman logs devc-demo-hello_gui") {
			t.Fatalf("up: %v", err)
		}
		// Stopped and kept for its logs; the box has no display files and
		// no forward was started.
		if ci := r.eng.Containers["devc-demo-hello_gui"]; ci.State.Running {
			t.Error("sidecar still running")
		}
		if exists(r.cookiePath()) || exists(r.envPath()) || len(r.fwd.started) != 0 {
			t.Error("left-overs after a failed start")
		}
		if st, _ := r.gui.Status(ctx, hello); st.State != "stopped" {
			t.Errorf("status %+v", st)
		}
		// gui up again replaces it.
		r.ready = func() (execx.Result, error) { return execx.Result{}, nil }
		if _, err := r.gui.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("limits cannot be applied", func(t *testing.T) {
		r := newRig(t)
		// The box needs only memory and pids; the desktop's CPU limit
		// cannot be applied.
		r.eng.InfoVal.CgroupControllers = []string{"memory", "pids"}
		r.boxUp()
		_, err := r.gui.Up(ctx, hello, UpOptions{})
		if err == nil || !strings.Contains(err.Error(), "cpu are not delegated") {
			t.Fatalf("up: %v", err)
		}
		if _, ok := r.eng.Containers["devc-demo-hello_gui"]; ok || len(r.fwd.started) != 0 {
			t.Error("a sidecar was created or forwarded without its limits")
		}
		// Switched off explicitly, it starts without that limit.
		r.box.Config.Global.GUI.CPUs = config.NoCPULimit
		if _, err := r.gui.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		hc := r.eng.Containers["devc-demo-hello_gui"].HostConfig
		if hc.Memory != 2<<30 || hc.PidsLimit != 512 || hc.NanoCpus != 0 {
			t.Errorf("limits %+v", hc)
		}
	})
	t.Run("box disturbed", func(t *testing.T) {
		r := newRig(t)
		r.boxUp()
		// The rules vanish while the sidecar starts: the check after the
		// join fails and the sidecar is stopped.
		r.ready = func() (execx.Result, error) {
			r.rules = ""
			return execx.Result{}, nil
		}
		_, err := r.gui.Up(ctx, hello, UpOptions{})
		if err == nil || !strings.Contains(err.Error(), "after the desktop joined") {
			t.Fatalf("up: %v", err)
		}
		if ci := r.eng.Containers["devc-demo-hello_gui"]; ci.State.Running || exists(r.cookiePath()) {
			t.Error("sidecar kept running or the cookie was handed out")
		}
	})
	t.Run("box restarted meanwhile", func(t *testing.T) {
		r := newRig(t)
		r.boxUp()
		r.ready = func() (execx.Result, error) {
			ci := r.eng.Containers["devc-demo-hello"]
			ci.State.Pid++
			r.eng.Put("devc-demo-hello", ci)
			return execx.Result{}, nil
		}
		if _, err := r.gui.Up(ctx, hello, UpOptions{}); err == nil || !strings.Contains(err.Error(), "restarted meanwhile") {
			t.Fatalf("up: %v", err)
		}
	})
	t.Run("forward fails", func(t *testing.T) {
		r := newRig(t)
		r.fwd.err = errors.New("port 6080 is taken")
		if _, err := r.gui.Up(ctx, hello, UpOptions{HostPort: 6080, Explicit: true}); err == nil || !strings.Contains(err.Error(), "taken") {
			t.Fatalf("up: %v", err)
		}
		if exists(r.cookiePath()) || r.eng.Containers["devc-demo-hello_gui"].State.Running {
			t.Error("left-overs")
		}
	})
	t.Run("foreign container", func(t *testing.T) {
		r := newRig(t)
		r.boxUp()
		r.eng.Put("devc-demo-hello_gui", engine.ContainerInfo{Name: "devc-demo-hello_gui", Labels: map[string]string{}})
		if _, err := r.gui.Up(ctx, hello, UpOptions{}); err == nil || !strings.Contains(err.Error(), "did not create it") {
			t.Fatalf("up: %v", err)
		}
		if err := r.gui.Down(ctx, hello); err == nil {
			t.Error("gui down touched a foreign container")
		}
		// devc down leaves it alone with a warning and stops the box.
		if err := r.box.Down(ctx, hello); err != nil || !strings.Contains(r.out.String(), "warning") {
			t.Errorf("down: %v\n%s", err, r.out)
		}
		if _, ok := r.eng.Containers["devc-demo-hello_gui"]; !ok {
			t.Error("foreign container removed")
		}
	})
	t.Run("planted directory", func(t *testing.T) {
		r := newRig(t)
		if _, err := r.gui.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		// The box replaces its cookie by a directory with content: down
		// still stops the box, with a warning; gui down reports it.
		if err := os.Remove(r.cookiePath()); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(r.cookiePath(), "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := r.gui.Down(ctx, hello); err == nil {
			t.Error("gui down did not report the directory")
		}
		if _, err := r.gui.Up(ctx, hello, UpOptions{}); err == nil {
			t.Error("gui up wrote over a directory")
		}
		if err := r.box.Down(ctx, hello); err != nil || !strings.Contains(r.out.String(), "warning") {
			t.Errorf("down: %v\n%s", err, r.out)
		}
		if ci := r.eng.Containers["devc-demo-hello"]; ci.State.Running {
			t.Error("the box still runs")
		}
	})
	t.Run("planted cookie link", func(t *testing.T) {
		r := newRig(t)
		r.boxUp()
		// The box plants a symlink where the cookie goes, aimed at a host
		// file: the host replaces the link and never writes through it.
		target := filepath.Join(r.base, "host-file")
		if err := os.WriteFile(target, []byte("precious"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, r.cookiePath()); err != nil {
			t.Fatal(err)
		}
		if _, err := r.gui.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(target); string(b) != "precious" {
			t.Errorf("host file changed: %q", b)
		}
		if fi, err := os.Lstat(r.cookiePath()); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("cookie %v %v", fi, err)
		}
		// Removal unlinks whatever is there without following it.
		if err := os.Remove(r.cookiePath()); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, r.cookiePath()); err != nil {
			t.Fatal(err)
		}
		if err := r.gui.Down(ctx, hello); err != nil {
			t.Fatal(err)
		}
		if exists(r.cookiePath()) || !exists(target) {
			t.Error("link not removed, or its target was")
		}
	})
}
