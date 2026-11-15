package gate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/execx"
)

func TestSocketPath(t *testing.T) {
	dir := "/run/user/1000/devc/gate"
	p, err := SocketPath(dir, "cluster1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != dir || !strings.HasPrefix(filepath.Base(p), "m-") || !strings.HasSuffix(p, ".sock") || len(filepath.Base(p)) != len("m-")+16+len(".sock") {
		t.Errorf("socket path %q", p)
	}
	if q, _ := SocketPath(dir, "cluster2"); q == p {
		t.Error("two hosts share a socket")
	}
	if HostFile(dir, "cluster1") != strings.TrimSuffix(p, ".sock")+".host" {
		t.Errorf("host file %q", HostFile(dir, "cluster1"))
	}
	// A long alias does not make a long path; a long directory does.
	if _, err := SocketPath(dir, strings.Repeat("a", 200)); err != nil {
		t.Errorf("long alias: %v", err)
	}
	if _, err := SocketPath("/"+strings.Repeat("d", 70), "c"); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("long directory: %v", err)
	}
	for _, h := range []string{"", "-oProxyCommand=x", "a b", "u@h", "h;x"} {
		if _, err := SocketPath(dir, h); err == nil {
			t.Errorf("alias %q accepted", h)
		}
	}
}

func TestLoginAndControlCommands(t *testing.T) {
	c := &Client{}
	cmd, err := c.LoginCmd("/r/gate/m-1.sock", "c1", 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-M", "-S", "/r/gate/m-1.sock", "-o", "ControlPersist=43200", "-o", "ServerAliveInterval=30", "c1", "true"}
	if cmd.Name != "ssh" || !slices.Equal(cmd.Args, want) {
		t.Errorf("login: %s %q", cmd.Name, cmd.Args)
	}
	c.Config = "/tmp/ssh config"
	cmd, _ = c.LoginCmd("/r/50%/m.sock", "c1", time.Hour)
	if !slices.Equal(cmd.Args[:4], []string{"-F", "/tmp/ssh config", "-M", "-S"}) || cmd.Args[4] != "/r/50%%/m.sock" {
		t.Errorf("login with -F and %%: %q", cmd.Args)
	}
	for _, ttl := range []time.Duration{0, time.Second, 31 * 24 * time.Hour} {
		if _, err := c.LoginCmd("/s", "c1", ttl); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
	if _, err := c.LoginCmd("/s", "-oProxyCommand=x", time.Hour); err == nil {
		t.Error("an option as host was accepted")
	}
	f := execx.NewFake()
	f.On("ssh", "-S", "/s", "-O", "check", "up")
	f.On("ssh", "-S", "/s", "-O", "check", "down").Exit(255)
	f.On("ssh", "-S", "/s", "-O", "exit", "up")
	c = &Client{Runner: f}
	if !c.Check(context.Background(), "/s", "up") || c.Check(context.Background(), "/s", "down") || c.Check(context.Background(), "/s", "-bad") {
		t.Error("check")
	}
	if err := c.Exit(context.Background(), "/s", "up"); err != nil {
		t.Error(err)
	}
	for _, call := range f.Calls() {
		if !call.Setsid || call.Timeout == 0 || call.Dir != "/" {
			t.Errorf("control command without session, timeout or /: %+v", call)
		}
	}
}

func TestModeOf(t *testing.T) {
	for in, want := range map[string]Mode{
		"": ModeShell, SFTPMarker: ModeSFTP, SFTPMarker + " ": ModeSFTP, " " + SFTPMarker: ModeSFTP,
		"ls": ModeExec, " ": ModeExec, SFTPMarker + " x": ModeExec, "scp -t /x": ModeExec,
	} {
		if got := ModeOf(in); got != want {
			t.Errorf("ModeOf(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestExecArgs: every case of gate-exec's ssh, with the command passed on
// as one argument, unchanged.
func TestExecArgs(t *testing.T) {
	c := &Client{}
	common := []string{"-S", "/g/m.sock", "-o", "ControlMaster=no", "-o", "ProxyCommand=false", "-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "Tunnel=no",
		"-o", "PermitLocalCommand=no", "-o", "RemoteCommand=none", "-o", "LogLevel=ERROR", "-e", "none"}
	cmd := `echo "a b" | wc -w; x='$(id)'`
	for _, c2 := range []struct {
		mode Mode
		cmd  string
		tty  bool
		tail []string
	}{
		{ModeShell, "", true, []string{"-tt", "h1"}},
		{ModeShell, "", false, []string{"-T", "h1"}},
		{ModeSFTP, SFTPMarker, false, []string{"-T", "-s", "h1", "sftp"}},
		{ModeSFTP, SFTPMarker, true, []string{"-T", "-s", "h1", "sftp"}},
		{ModeExec, cmd, false, []string{"-T", "h1", "--", cmd}},
		{ModeExec, cmd, true, []string{"-tt", "h1", "--", cmd}},
		{ModeExec, "-oProxyCommand=evil", false, []string{"-T", "h1", "--", "-oProxyCommand=evil"}},
	} {
		got, err := c.ExecArgs("/g/m.sock", "h1", c2.mode, c2.cmd, c2.tty)
		if err != nil {
			t.Fatal(err)
		}
		if want := append(slices.Clone(common), c2.tail...); !slices.Equal(got, want) {
			t.Errorf("%s %q tty=%v:\n got %q\nwant %q", c2.mode, c2.cmd, c2.tty, got, want)
		}
	}
	c.Config = "/cfg"
	if got, _ := c.ExecArgs("/g/m.sock", "h1", ModeShell, "", false); !slices.Equal(got[:2], []string{"-F", "/cfg"}) {
		t.Errorf("-F: %q", got)
	}
	if _, err := c.ExecArgs("/s", "h1", ModeExec, "", false); err == nil {
		t.Error("empty command accepted")
	}
	if _, err := c.ExecArgs("/s", "-x", ModeShell, "", false); err == nil {
		t.Error("option as host accepted")
	}
}

func TestCheckReadOnly(t *testing.T) {
	allow := []string{"squeue", "ls", "/usr/bin/sinfo"}
	for _, ok := range []string{"squeue", "squeue -u me", "  ls -la /scratch", "ls\t-l", `ls "a b"`, "ls *.out", "/usr/bin/sinfo -N", "ls ~/x", "ls a#b", "ls a,b"} {
		if err := CheckReadOnly(ok, allow); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, c := range []struct{ cmd, msg string }{
		{"rm -rf /", "not allowed"},
		{"sinfo", "not allowed"},
		{"'ls'", "not allowed"},
		{`\ls`, "not allowed"},
		{"lsx", "not allowed"},
		{"PATH=/x ls", "not allowed"},
		{"", "interactive shell"},
		{"   ", "interactive shell"},
		{"ls\r", "shell syntax"},
		{"ls\x00", "shell syntax"},
		{"ls\x7f", "shell syntax"},
		{"ls\x1b[2J", "shell syntax"},
	} {
		if err := CheckReadOnly(c.cmd, allow); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q: %v", c.cmd, err)
		}
	}
	for _, m := range ShellMetacharacters {
		cmd := "ls a" + string(m) + "id"
		if err := CheckReadOnly(cmd, allow); err == nil || !strings.Contains(err.Error(), "shell syntax") {
			t.Errorf("metacharacter %q: %v", m, err)
		}
	}
	if err := CheckReadOnly("ls", nil); err == nil || !strings.Contains(err.Error(), "no command is allowed") {
		t.Errorf("empty allowlist: %v", err)
	}
}

func TestServerConfig(t *testing.T) {
	text, err := ServerConfig{HostKey: "/s/local/gate/host_ed25519", AuthorizedKeys: "/s/local/gate/keys/0123456789ab.pub", ForceCommand: "'/bin/devc' 'gate-exec'"}.Render()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for _, want := range []string{
		`HostKey "/s/local/gate/host_ed25519"`,
		`AuthorizedKeysFile "/s/local/gate/keys/0123456789ab.pub"`,
		"AuthenticationMethods publickey", "PasswordAuthentication no", "KbdInteractiveAuthentication no",
		"UsePAM no", "StrictModes no",
		"AllowTcpForwarding no", "AllowStreamLocalForwarding no", "AllowAgentForwarding no", "X11Forwarding no",
		"PermitTunnel no", "GatewayPorts no", "PermitUserEnvironment no", "DisableForwarding yes", "PermitUserRC no",
		"PermitTTY yes", "Subsystem sftp " + SFTPMarker, "ForceCommand '/bin/devc' 'gate-exec'", "PidFile none",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "AcceptEnv") || strings.HasPrefix(l, "SetEnv") || strings.HasPrefix(l, "Include") {
			t.Errorf("unexpected line %q", l)
		}
	}
	for _, bad := range []ServerConfig{
		{HostKey: "rel/key", AuthorizedKeys: "/k", ForceCommand: "x"},
		{HostKey: "/k", AuthorizedKeys: "/home/%u/k", ForceCommand: "x"},
		{HostKey: `/a"b`, AuthorizedKeys: "/k", ForceCommand: "x"},
		{HostKey: "/k", AuthorizedKeys: "/k#x", ForceCommand: "x"},
		{HostKey: "/k", AuthorizedKeys: "/k", ForceCommand: "x\nAllowTcpForwarding yes"},
		{HostKey: "/k", AuthorizedKeys: "/k", ForceCommand: ""},
	} {
		if _, err := bad.Render(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	// Paths with blanks are quoted, not split.
	text, err = ServerConfig{HostKey: "/my state/k", AuthorizedKeys: "/my state/a", ForceCommand: "x"}.Render()
	if err != nil || !strings.Contains(text, `HostKey "/my state/k"`) {
		t.Errorf("blank in path: %v\n%s", err, text)
	}
}

func TestExecTargetCommand(t *testing.T) {
	tg := ExecTarget{Executable: "/opt/my tools/devc", ConfigDir: "/c", StateDir: "/s", RuntimeDir: "/run/user/1/devc", SSHConfig: "/t/ssh_config", BoxID: "0123456789ab", Host: "c1"}
	got, err := tg.Command()
	if err != nil {
		t.Fatal(err)
	}
	want := `'/opt/my tools/devc' '--config-dir' '/c' '--state-dir' '/s' 'gate-exec' '--box' '0123456789ab' '--host' 'c1' '--runtime-dir' '/run/user/1/devc' '--ssh-config' '/t/ssh_config'`
	if got != want {
		t.Errorf("command:\n got %s\nwant %s", got, want)
	}
	tg.SSHConfig = ""
	if got, _ := tg.Command(); strings.Contains(got, "ssh-config") {
		t.Errorf("without DEVC_SSH_CONFIG: %s", got)
	}
	for _, mod := range []func(*ExecTarget){
		func(x *ExecTarget) { x.ConfigDir = "/it's" },
		func(x *ExecTarget) { x.StateDir = `/a\b` },
		func(x *ExecTarget) { x.StateDir = "/a!b" },
		func(x *ExecTarget) { x.RuntimeDir = "/a\nb" },
		func(x *ExecTarget) { x.RuntimeDir = "relative" },
		func(x *ExecTarget) { x.BoxID = "../x" },
		func(x *ExecTarget) { x.Host = "-oProxyCommand=x" },
	} {
		x := tg
		mod(&x)
		if _, err := x.Command(); err == nil {
			t.Errorf("accepted %+v", x)
		}
	}
}

func TestTruncate(t *testing.T) {
	if Truncate("abc", 5) != "abc" || Truncate("abcdef", 3) != "abc" {
		t.Error("ascii")
	}
	s := "aé€😀" // 1 + 2 + 3 + 4 bytes
	for n := 0; n <= len(s); n++ {
		got := Truncate(s, n)
		if len(got) > n || !strings.HasPrefix(s, got) || strings.ToValidUTF8(got, "?") != got {
			t.Errorf("Truncate(%q, %d) = %q", s, n, got)
		}
	}
}

func TestAppendLog(t *testing.T) {
	p := testPaths(t)
	long := strings.Repeat("x", MaxLoggedCommand+100)
	for i, e := range []Entry{{Host: "c1", Mode: ModeExec, Command: "echo hi", Exit: 0}, {Host: "c1", Mode: ModeExec, Command: long, Exit: 3}} {
		if err := AppendLog(p, "demo", e); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	b, err := os.ReadFile(p.GateLog("demo"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var second Entry
	if len(lines) != 2 || !strings.Contains(lines[0], `"command":"echo hi"`) || json.Unmarshal([]byte(lines[1]), &second) != nil || len(second.Command) != MaxLoggedCommand || second.Exit != 3 {
		t.Errorf("log:\n%.300s", b)
	}
	if fi, _ := os.Stat(p.GateLog("demo")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	// A planted symlink is not followed.
	os.Remove(p.GateLog("demo"))
	target := filepath.Join(t.TempDir(), "elsewhere")
	os.Symlink(target, p.GateLog("demo"))
	if err := AppendLog(p, "demo", Entry{Host: "c1"}); err == nil {
		t.Error("the log followed a symlink")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("symlink target written: %v", err)
	}
}

func TestRsyncShell(t *testing.T) {
	if got := (&Client{}).RsyncShell(""); got != nil {
		t.Errorf("default shell: %q", got)
	}
	if got := (&Client{Config: "/t/ssh config"}).RsyncShell(""); !slices.Equal(got, []string{"ssh", "-F", "/t/ssh config"}) {
		t.Errorf("with a configuration: %q", got)
	}
	got := (&Client{Config: "/t/c"}).RsyncShell("/r/gate/m-%h.sock")
	if !slices.Equal(got, []string{"ssh", "-F", "/t/c", "-S", "/r/gate/m-%%h.sock", "-o", "ControlMaster=no"}) {
		t.Errorf("over a master: %q", got)
	}
}
