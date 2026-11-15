package gate

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/secrets"
)

// testPaths are short private roots (the runtime directory holds sockets).
func testPaths(t *testing.T) *paths.Paths {
	t.Helper()
	base, err := os.MkdirTemp("", "gt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(base, "c"), StateDir: filepath.Join(base, "s"),
		RuntimeDir: filepath.Join(base, "r"), Home: base, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// testConfig is examples/config with the given files replaced.
func testConfig(t *testing.T, files map[string]string) *config.Config {
	t.Helper()
	p := testPaths(t)
	src := filepath.Join("..", "..", "examples", "config")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		dst := filepath.Join(p.ConfigDir(), rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(p.ConfigDir(), rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := config.Load(p)
	return c
}

const helloTOML = "harnesses = [\"claude\"]\n[grants]\nssh = [\"c1\", \"c2:ro\"]\n[grants.ssh_allow]\nc2 = [\"ls\", \"squeue\"]\n"

var hello = names.ProjectRef{Realm: "demo", Project: "hello"}

type execRig struct {
	x       *Exec
	f       *execx.Fake
	env     map[string]string
	entries []Entry
	realms  []string
	stderr  bytes.Buffer
}

func newExecRig(t *testing.T, c *config.Config, host string) *execRig {
	t.Helper()
	r := &execRig{f: execx.NewFake(), env: map[string]string{}}
	rt, err := c.Paths.GateRuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	// The box was started with the current grants (finding C1-02).
	if res, err := c.Resolved(hello); err == nil {
		if _, err := box.ReadCapabilities(c.Paths, hello); err != nil {
			if err := box.WriteCapabilities(c.Paths, hello, box.CapabilitiesOf(res, "cid", time.Now())); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.x = &Exec{Config: c, Client: &Client{Runner: r.f}, BoxID: names.BoxID(hello), Host: host, RuntimeDir: rt,
		Getenv: func(k string) string { return r.env[k] }, Stdin: strings.NewReader("input"), Stdout: &bytes.Buffer{}, Stderr: &r.stderr,
		Log: func(realm string, e Entry) error {
			r.realms = append(r.realms, realm)
			r.entries = append(r.entries, e)
			return nil
		}}
	return r
}

func (r *execRig) sock(t *testing.T) string {
	s, err := SocketPath(r.x.RuntimeDir, r.x.Host)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// master makes the master's check answer (or not).
func (r *execRig) master(t *testing.T, alive bool) {
	resp := r.f.On("ssh", "-S", r.sock(t), "-O", "check", r.x.Host)
	if !alive {
		resp.Exit(255)
	}
}

// execCalls are the calls of the multiplexing ssh (not the checks).
func (r *execRig) execCalls() []execx.Cmd {
	var out []execx.Cmd
	for _, c := range r.f.Calls() {
		if !slices.Contains(c.Args, "-O") {
			out = append(out, c)
		}
	}
	return out
}

func TestExecFullGrant(t *testing.T) {
	c := testConfig(t, map[string]string{"realms/demo/projects/hello/project.toml": helloTOML})
	cmd := `echo "a b" | wc -w`
	for _, tc := range []struct {
		name     string
		original string
		tty      bool
		mode     Mode
		tail     []string
	}{
		{"exec", cmd, false, ModeExec, []string{"-T", "c1", "--", cmd}},
		{"exec tty", cmd, true, ModeExec, []string{"-tt", "c1", "--", cmd}},
		{"shell", "", true, ModeShell, []string{"-tt", "c1"}},
		{"sftp", SFTPMarker + " ", false, ModeSFTP, []string{"-T", "-s", "c1", "sftp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newExecRig(t, c, "c1")
			r.env["SSH_ORIGINAL_COMMAND"] = tc.original
			if tc.tty {
				r.env["SSH_TTY"] = "/dev/pts/3"
			}
			r.master(t, true)
			r.f.On("ssh", "-S", r.sock(t), "-o", "ControlMaster=no").Exit(7)
			if code := r.x.Run(context.Background()); code != 7 {
				t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
			}
			calls := r.execCalls()
			if len(calls) != 1 || !slices.Equal(calls[0].Args[len(calls[0].Args)-len(tc.tail):], tc.tail) {
				t.Fatalf("ssh calls: %+v", calls)
			}
			if calls[0].Dir != "/" {
				t.Errorf("ssh ran in %q", calls[0].Dir)
			}
			if len(r.entries) != 1 || r.realms[0] != "demo" {
				t.Fatalf("log: %+v", r.entries)
			}
			e := r.entries[0]
			wantCmd := ""
			if tc.mode == ModeExec {
				wantCmd = cmd
			}
			if e.Box != "devc-demo-hello" || e.Project != "demo/hello" || e.Host != "c1" || e.Mode != tc.mode || e.TTY != tc.tty || e.Command != wantCmd || e.Exit != 7 || e.Refused != "" {
				t.Errorf("entry %+v", e)
			}
		})
	}
}

func TestExecRefusals(t *testing.T) {
	c := testConfig(t, map[string]string{"realms/demo/projects/hello/project.toml": helloTOML})
	for _, tc := range []struct {
		name, host, original string
		alive                bool
		code                 int
		refused, msg         string
	}{
		{"no master", "c1", "echo hi", false, 255, "no master", "ask the user to run 'devc ssh login c1'"},
		{"not granted", "c9", "echo hi", true, 1, "not granted", "c9 is not granted to demo/hello"},
		{"ro disallowed", "c2", "rm -rf x", true, 1, "read-only", `"rm" is not allowed`},
		{"ro metachar", "c2", "ls; rm x", true, 1, "read-only", "shell syntax"},
		{"ro subst", "c2", "ls $(rm x)", true, 1, "read-only", "shell syntax"},
		{"ro shell", "c2", "", true, 1, "read-only: shell", "interactive shell is not allowed"},
		{"ro sftp", "c2", SFTPMarker, true, 1, "read-only: sftp", "sftp, scp and rsync are not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newExecRig(t, c, tc.host)
			r.env["SSH_ORIGINAL_COMMAND"] = tc.original
			r.master(t, tc.alive)
			r.f.On("ssh", "-S", r.sock(t), "-o", "ControlMaster=no")
			if code := r.x.Run(context.Background()); code != tc.code {
				t.Errorf("exit %d, want %d", code, tc.code)
			}
			if len(r.execCalls()) != 0 {
				t.Errorf("ssh ran: %+v", r.execCalls())
			}
			if !strings.Contains(r.stderr.String(), tc.msg) || strings.Count(r.stderr.String(), "\n") != 1 {
				t.Errorf("stderr %q, want one line with %q", r.stderr.String(), tc.msg)
			}
			if len(r.entries) != 1 || !strings.HasPrefix(r.entries[0].Refused, tc.refused) || r.entries[0].Exit != tc.code || r.entries[0].Host != tc.host {
				t.Errorf("log: %+v", r.entries)
			}
		})
	}
	// An allowed command on a read-only host runs.
	r := newExecRig(t, c, "c2")
	r.env["SSH_ORIGINAL_COMMAND"] = "squeue -u me"
	r.master(t, true)
	r.f.On("ssh", "-S", r.sock(t), "-o", "ControlMaster=no")
	if code := r.x.Run(context.Background()); code != 0 || len(r.execCalls()) != 1 || !slices.Contains(r.execCalls()[0].Args, "squeue -u me") {
		t.Errorf("allowed read-only command: exit %d calls %+v stderr %q", code, r.execCalls(), r.stderr.String())
	}
	// A box ID that no project has: refused, nothing logged.
	r = newExecRig(t, c, "c1")
	r.x.BoxID = "000000000000"
	r.env["SSH_ORIGINAL_COMMAND"] = "echo"
	if code := r.x.Run(context.Background()); code != 1 || len(r.entries) != 0 || len(r.f.Calls()) != 0 {
		t.Errorf("unknown box: exit %d entries %v calls %v", code, r.entries, r.f.Calls())
	}
	// A failed ssh run counts as 255.
	r = newExecRig(t, c, "c1")
	r.env["SSH_ORIGINAL_COMMAND"] = "echo"
	r.master(t, true)
	r.f.On("ssh", "-S", r.sock(t), "-o", "ControlMaster=no").NotFound()
	if code := r.x.Run(context.Background()); code != 255 || r.entries[0].Exit != 255 {
		t.Errorf("ssh missing: exit %d %+v", code, r.entries)
	}
}

func TestExecConfigurationProblems(t *testing.T) {
	c := testConfig(t, map[string]string{"realms/demo/projects/hello/project.toml": "harnesses = [\"claude\"]\n[grants]\nssh = [\"c1\", \"-bad\"]\n"})
	if len(c.ProblemsFor(hello)) == 0 {
		t.Fatal("the fixture has no problem")
	}
	r := newExecRig(t, c, "c1")
	r.env["SSH_ORIGINAL_COMMAND"] = "echo"
	r.master(t, true)
	if code := r.x.Run(context.Background()); code != 1 || len(r.f.Calls()) != 0 || !strings.Contains(r.stderr.String(), "config check") {
		t.Errorf("exit %d calls %v stderr %q", code, r.f.Calls(), r.stderr.String())
	}
}

// fakeKeygen makes ssh-keygen write a recognisable key pair.
func fakeKeygen(f *execx.Fake, n *int) {
	f.On("ssh-keygen").Do(func(c execx.Cmd) {
		*n++
		file := c.Args[len(c.Args)-1]
		os.WriteFile(file, []byte("PRIVATE-"+filepath.Base(filepath.Dir(file))), 0o600)
		os.WriteFile(file+".pub", []byte("ssh-ed25519 QUFBQUFBQUE= "+c.Args[len(c.Args)-3]+"\n"), 0o644)
	})
}

func TestBoxHooks(t *testing.T) {
	c := testConfig(t, map[string]string{"realms/demo/projects/hello/project.toml": helloTOML})
	p := c.Paths
	f := execx.NewFake()
	var keygens int
	fakeKeygen(f, &keygens)
	h := &BoxHooks{Paths: p, Runner: f, Login: func() (string, error) { return "alice", nil }, AllowPersistent: true}
	r, err := c.Resolved(hello)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The configuration mount: only for a box with a grant.
	in := &box.Inputs{Resolved: r, Paths: p}
	s := &box.Spec{}
	if err := h.Contribute(in, s); err != nil {
		t.Fatal(err)
	}
	m := s.Mounts
	if len(m) != 1 || m[0].Target != paths.InBoxSSHConfigD || m[0].Source != filepath.Join(p.BoxEtcDir(hello), "ssh_config") || !m[0].ReadOnly || !m[0].File {
		t.Errorf("mounts %+v", m)
	}

	// Prepare: host key once, configuration and known hosts.
	for i := 0; i < 2; i++ {
		if err := h.Prepare(ctx, in, nil); err != nil {
			t.Fatal(err)
		}
	}
	if keygens != 1 {
		t.Errorf("ssh-keygen ran %d times for the host key", keygens)
	}
	if fi, err := os.Stat(p.GateHostKey()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("host key: %v %v", fi, err)
	}
	cfg, _ := os.ReadFile(filepath.Join(p.BoxEtcDir(hello), "ssh_config"))
	for _, want := range []string{"Host c1\n", "Host c2\n", "User alice", "ProxyCommand /opt/devc/bin/devc box gate-connect c1", "ProxyCommand /opt/devc/bin/devc box gate-connect c2",
		"IdentityFile /run/devc-secrets/gate_key", "IdentitiesOnly yes", "HostKeyAlias devc-gate", "UserKnownHostsFile /etc/devc/gate_known_hosts",
		"StrictHostKeyChecking yes", "ControlMaster no", "ForwardAgent no"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("ssh_config lacks %q:\n%s", want, cfg)
		}
	}
	if kh, _ := os.ReadFile(filepath.Join(p.BoxEtcDir(hello), "gate_known_hosts")); string(kh) != "devc-gate ssh-ed25519 QUFBQUFBQUE=\n" {
		t.Errorf("known hosts %q", kh)
	}

	// The box's key: private for the secrets, public authorized with
	// restrict,pty; generated on the runtime directory, not the state.
	sec, err := h.BoxSecrets(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := p.GateRuntimeDir()
	last := f.Calls()[len(f.Calls())-1]
	if !strings.HasPrefix(last.Args[len(last.Args)-1], rt+"/") {
		t.Errorf("box key generated in %s", last.Args[len(last.Args)-1])
	}
	if len(sec) != 1 || !strings.HasPrefix(string(sec["gate_key"]), "PRIVATE-") {
		t.Errorf("secrets %q", sec)
	}
	pub, err := os.ReadFile(p.GateBoxKey(r.BoxID))
	if err != nil || string(pub) != "restrict,pty ssh-ed25519 QUFBQUFBQUE= devc-box-"+r.BoxID+"\n" {
		t.Errorf("authorized key %q %v", pub, err)
	}
	if fi, _ := os.Stat(p.GateBoxKey(r.BoxID)); fi.Mode().Perm() != 0o600 {
		t.Errorf("authorized key mode %v", fi.Mode())
	}
	if left, _ := os.ReadDir(rt); len(left) != 0 {
		t.Errorf("left in the runtime directory: %v", left)
	}
	// A second start gets a new key.
	sec2, _ := h.BoxSecrets(ctx, r)
	if string(sec2["gate_key"]) == string(sec["gate_key"]) {
		t.Error("the key was reused")
	}
	if err := h.Cleanup(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.GateBoxKey(r.BoxID)); !os.IsNotExist(err) {
		t.Errorf("key after cleanup: %v", err)
	}

	// Without a grant: no mount, no files, no key.
	h.BoxSecrets(ctx, r)
	r.Grants.SSH = nil
	s = &box.Spec{}
	if err := h.Contribute(in, s); err != nil || len(s.Mounts) != 0 {
		t.Errorf("mounts without a grant: %+v %v", s.Mounts, err)
	}
	if err := h.Prepare(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ssh_config", "gate_known_hosts"} {
		if _, err := os.Stat(filepath.Join(p.BoxEtcDir(hello), f)); !os.IsNotExist(err) {
			t.Errorf("%s without a grant: %v", f, err)
		}
	}
	if sec, err := h.BoxSecrets(ctx, r); err != nil || len(sec) != 0 {
		t.Errorf("secrets without a grant: %v %v", sec, err)
	}
	if _, err := os.Stat(p.GateBoxKey(r.BoxID)); !os.IsNotExist(err) {
		t.Errorf("key without a grant: %v", err)
	}
	if _, err := ClientConfig([]string{"c1"}, "bad name"); err == nil {
		t.Error("a login with a blank was accepted")
	}
	if _, err := ClientConfig([]string{"-oProxyCommand=x"}, "alice"); err == nil {
		t.Error("an option as host was accepted")
	}
}

func TestServerPrepare(t *testing.T) {
	c := testConfig(t, map[string]string{"realms/demo/projects/hello/project.toml": helloTOML})
	p := c.Paths
	rt, _ := p.RuntimeDir()
	s := &Server{Paths: p, SSHD: "/usr/sbin/sshd", Exec: ExecTarget{Executable: "/bin/devc", ConfigDir: p.ConfigDir(), StateDir: p.StateDir(), RuntimeDir: rt}}
	id := names.BoxID(hello)
	if _, err := s.Prepare(id, "c1"); err == nil {
		t.Fatal("prepared without keys")
	}
	f := execx.NewFake()
	var n int
	fakeKeygen(f, &n)
	k := &Keys{Paths: p, Runner: f}
	if _, err := k.EnsureHostKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(id, "c1"); err != ErrNoBoxKey {
		t.Fatalf("without the box key: %v", err)
	}
	// On a runtime directory that is not memory-backed, no key is made
	// without the opt-in, and ssh-keygen does not even run (C1-12).
	gateRT, _ := p.GateRuntimeDir()
	os.MkdirAll(gateRT, 0o700)
	if secrets.CheckMemoryFS(gateRT) != nil {
		before := n
		if _, err := k.NewBoxKey(context.Background(), id); err == nil || !strings.Contains(err.Error(), secrets.EnvAllowPersistent) {
			t.Errorf("a box key on persistent storage: %v", err)
		}
		if n != before {
			t.Error("ssh-keygen ran before the check")
		}
	} else {
		t.Log("the test's runtime directory is memory-backed: the refusal is not exercised")
	}
	k.AllowPersistent = true
	if _, err := k.NewBoxKey(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	conf, err := s.Prepare(id, "c1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(conf)
	text := string(b)
	if !strings.Contains(text, `AuthorizedKeysFile "`+p.GateBoxKey(id)+`"`) || !strings.Contains(text, "'--box' '"+id+"' '--host' 'c1'") || !strings.Contains(text, `HostKey "`+p.GateHostKey()+`"`) {
		t.Errorf("configuration:\n%s", text)
	}
	if fi, _ := os.Stat(conf); fi.Mode().Perm() != 0o600 || filepath.Dir(conf) != filepath.Join(rt, "gate") {
		t.Errorf("configuration file %s %v", conf, fi.Mode())
	}
	cmd := s.Command(conf)
	if cmd.Name != "/usr/sbin/sshd" || !slices.Equal(cmd.Args, []string{"-i", "-e", "-f", conf}) || !cmd.Setsid {
		t.Errorf("sshd command %+v", cmd)
	}
	if _, err := s.Prepare(id, "-oX"); err == nil {
		t.Error("an option as host was accepted")
	}
}

// TestExecStartedWith: gate-exec allows what the box was started with and
// the configuration still grants (finding C1-02).
func TestExecStartedWith(t *testing.T) {
	c := testConfig(t, map[string]string{"realms/demo/projects/hello/project.toml": helloTOML})
	run := func(host, original string) (int, string) {
		r := newExecRig(t, c, host)
		r.env["SSH_ORIGINAL_COMMAND"] = original
		r.master(t, true)
		r.f.On("ssh", "-S", r.sock(t), "-o", "ControlMaster=no").Exit(0)
		code := r.x.Run(context.Background())
		return code, r.stderr.String()
	}
	// Started with c1 read-only (allowing ls and cat) and without c2.
	box.WriteCapabilities(c.Paths, hello, box.Capabilities{V: 1, ContainerID: "cid", SSH: []string{"c1:ro"}, SSHAllow: map[string][]string{"c1": {"ls", "cat"}}})
	if code, errs := run("c2", "ls"); code != 1 || !strings.Contains(errs, "after its box started") {
		t.Errorf("a host granted after the start: %d %q", code, errs)
	}
	if code, errs := run("c1", "echo hi"); code != 1 || !strings.Contains(errs, "not allowed") {
		t.Errorf("full now, read-only at the start: %d %q", code, errs)
	}
	if code, _ := run("c1", "cat f"); code != 0 {
		t.Errorf("an allowed command: %d", code)
	}
	// Without a record nothing goes through.
	os.Remove(c.Paths.BoxCapabilitiesFile(hello))
	r := newExecRig(t, c, "c1")
	os.Remove(c.Paths.BoxCapabilitiesFile(hello))
	r.env["SSH_ORIGINAL_COMMAND"] = "ls"
	r.master(t, true)
	if code := r.x.Run(context.Background()); code != 1 || !strings.Contains(r.stderr.String(), "restart it") {
		t.Errorf("without a record: %d %q", code, r.stderr.String())
	}
}
