//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mb706/devc/internal/gate"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// sshTools finds the OpenSSH programs the gate tests need, or skips: an
// sshd stands in for the remote machine and serves as the gate.
func sshTools(t *testing.T) string {
	t.Helper()
	for _, n := range []string{"ssh", "ssh-keygen", "scp", "sftp"} {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s is not installed on this host; the SSH gate tests need an OpenSSH client and server", n)
		}
	}
	for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd", "/sbin/sshd"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	if p, err := exec.LookPath("sshd"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	t.Skip("no sshd binary on this host: the SSH gate tests need one to stand in for a remote machine and to serve as the gate")
	return ""
}

// remote stands in for a remote machine: an unprivileged sshd on a free
// loopback port with its own host key and an authorized key of the test,
// reached through a temporary ssh configuration (DEVC_SSH_CONFIG) under
// two aliases. Commands "on the remote" run on this host as the user.
type remote struct {
	dir    string
	files  string // where transfers go
	port   int
	alias  string // granted to the test boxes
	other  string // the same machine under a second alias
	config string
	// marker is in the environment of every process the test starts, so
	// cleanup can find whatever is left.
	marker string
	log    *bytes.Buffer
	mu     sync.Mutex
}

func sshKeygen(t *testing.T, file, comment string) {
	t.Helper()
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", file).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func readPub(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b))
	return f[0] + " " + f[1]
}

// killMarked ends every process of this user that carries marker in its
// environment, and fails the test if there was one: tests must not leave
// daemons, sshd processes or masters behind.
func killMarked(t *testing.T, marker string) {
	t.Helper()
	find := func() []int {
		var pids []int
		des, _ := os.ReadDir("/proc")
		for _, de := range des {
			pid, err := strconv.Atoi(de.Name())
			if err != nil || pid == os.Getpid() {
				continue
			}
			env, err := os.ReadFile(filepath.Join("/proc", de.Name(), "environ"))
			if err != nil {
				continue
			}
			for _, kv := range bytes.Split(env, []byte{0}) {
				if string(kv) == marker {
					pids = append(pids, pid)
					break
				}
			}
		}
		return pids
	}
	// Processes that are ending get a moment.
	var left []int
	for i := 0; i < 20; i++ {
		if left = find(); len(left) == 0 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	for _, pid := range left {
		cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		t.Errorf("process %d was left running: %s", pid, bytes.ReplaceAll(cmd, []byte{0}, []byte{' '}))
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func startRemote(t *testing.T, sshd string) *remote {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	r := &remote{dir: t.TempDir(), alias: "itgate" + itest.Nonce, other: "itgateo" + itest.Nonce,
		marker: "DEVC_IT_GATE=" + hex.EncodeToString(b), log: &bytes.Buffer{}}
	t.Cleanup(func() { killMarked(t, r.marker) })
	r.files = filepath.Join(r.dir, "files")
	if err := os.Mkdir(r.files, 0o700); err != nil {
		t.Fatal(err)
	}
	sshKeygen(t, filepath.Join(r.dir, "host"), "itest-remote-host")
	sshKeygen(t, filepath.Join(r.dir, "user"), "itest-remote-user")
	writeFile(t, filepath.Join(r.dir, "authorized"), readPub(t, filepath.Join(r.dir, "user.pub"))+"\n")
	r.port = freePort(t)
	conf := filepath.Join(r.dir, "sshd_config")
	writeFile(t, conf, strings.Join([]string{
		"Port " + strconv.Itoa(r.port),
		"ListenAddress 127.0.0.1",
		"HostKey " + filepath.Join(r.dir, "host"),
		"AuthorizedKeysFile " + filepath.Join(r.dir, "authorized"),
		"AuthenticationMethods publickey",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"UsePAM no",
		"StrictModes no",
		"PidFile none",
		// Forwarding is allowed here, so that only the gate refuses it.
		"AllowTcpForwarding yes",
		"AllowAgentForwarding yes",
		"PrintMotd no",
		"Subsystem sftp internal-sftp",
		"LogLevel ERROR",
	}, "\n")+"\n")
	cmd := exec.Command(sshd, "-D", "-e", "-f", conf)
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), r.marker)
	cmd.Stdout, cmd.Stderr = r, r
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("remote sshd log:\n%s", r.output())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(r.port))
		if err == nil {
			c.Close()
			break
		}
		select {
		case <-done:
			t.Fatalf("the remote sshd exited:\n%s", r.output())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the remote sshd does not listen:\n%s", r.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
	writeFile(t, filepath.Join(r.dir, "known_hosts"), "itest-remote "+readPub(t, filepath.Join(r.dir, "host.pub"))+"\n")
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	var cfg strings.Builder
	for _, a := range []string{r.alias, r.other} {
		fmt.Fprintf(&cfg, "Host %s\n", a)
		for _, kv := range [][2]string{
			{"HostName", "127.0.0.1"}, {"Port", strconv.Itoa(r.port)}, {"User", me.Username},
			{"IdentityFile", filepath.Join(r.dir, "user")}, {"IdentitiesOnly", "yes"}, {"IdentityAgent", "none"},
			{"HostKeyAlias", "itest-remote"}, {"UserKnownHostsFile", filepath.Join(r.dir, "known_hosts")},
			{"GlobalKnownHostsFile", "/dev/null"}, {"StrictHostKeyChecking", "yes"}, {"BatchMode", "yes"},
		} {
			fmt.Fprintf(&cfg, "    %s %s\n", kv[0], kv[1])
		}
	}
	r.config = filepath.Join(r.dir, "ssh_config")
	writeFile(t, r.config, cfg.String())
	return r
}

func (r *remote) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.log.Write(p)
}

func (r *remote) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.log.String()
}

// direct runs a command over the user's direct SSH to the remote, for
// comparison with the gate.
func (r *remote) direct(t *testing.T, command string) (string, int) {
	t.Helper()
	cmd := exec.Command("ssh", "-F", r.config, r.alias, command)
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), r.marker)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

type gateLogEntry struct {
	Box     string `json:"box"`
	Project string `json:"project"`
	Host    string `json:"host"`
	Mode    string `json:"mode"`
	TTY     bool   `json:"tty"`
	Command string `json:"command"`
	Exit    int    `json:"exit"`
	Refused string `json:"refused"`
}

func readGateLog(t *testing.T, e *itest.Env) []gateLogEntry {
	t.Helper()
	f, err := os.Open(filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "gate-log.jsonl"))
	if err != nil {
		t.Fatalf("the gate log: %v", err)
	}
	defer f.Close()
	var out []gateLogEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var g gateLogEntry
		if err := json.Unmarshal(sc.Bytes(), &g); err != nil {
			t.Fatalf("gate log line %q: %v", sc.Text(), err)
		}
		out = append(out, g)
	}
	return out
}

// TestSSHGate covers work order C10b section 5, integration items 1-11.
func TestSSHGate(t *testing.T) {
	sshd := sshTools(t)
	rem := startRemote(t, sshd)
	e := itest.NewNoDaemon(t)
	e.ExtraEnv = append(e.ExtraEnv, gate.EnvSSHConfig+"="+rem.config, rem.marker)
	itest.EnsureBaseImage(t, e)
	e.Timeout = 5 * time.Minute
	ref := itest.RealmA + "/gatep"
	refRO := itest.RealmA + "/gatero"
	refB := itest.RealmA + "/gateb"
	project(e, ref, fmt.Sprintf("harnesses = []\n[grants]\nssh = [%q]\n", rem.alias))
	project(e, refRO, fmt.Sprintf("harnesses = []\n[grants]\nssh = [%q]\n[grants.ssh_allow]\n%s = [\"echo\", \"ls\"]\n", rem.alias+":ro", rem.alias))
	project(e, refB, fmt.Sprintf("harnesses = []\n[grants]\nssh = [%q]\n", rem.other))
	for _, r := range []string{ref, refRO, refB} {
		e.MustRun("up", r)
	}
	rt := filepath.Join(e.RuntimeDir, "gate")
	sock, err := gate.SocketPath(rt, rem.alias)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The master must not outlive the test (killMarked checks).
		cmd := exec.Command("ssh", "-F", rem.config, "-S", sock, "-O", "exit", rem.alias)
		cmd.Dir = "/"
		_ = cmd.Run()
	})

	// The box's SSH client reads the generated configuration both ways.
	if got := boxSh(t, e, ref, "head -1 ~/.ssh/config; test -r /etc/ssh/ssh_config.d/50-devc.conf && echo mounted; ls /run/devc-secrets"); got != "Include /etc/devc/ssh_config\nmounted\ngate_key" {
		t.Errorf("box SSH configuration: %q", got)
	}
	if keys, _ := filepath.Glob(filepath.Join(e.StateDir, "local", "gate", "keys", "*.pub")); len(keys) != 3 {
		t.Errorf("authorized keys: %v", keys)
	}

	// 1. Login with key authentication establishes the master.
	res := e.Run("ssh", "login", rem.alias, "--ttl", "10m")
	if res.ExitCode != 0 {
		t.Fatalf("ssh login: exit %d\n%s%s\nremote:\n%s", res.ExitCode, res.Stdout, res.Stderr, rem.output())
	}
	var rows []struct{ Host, State string }
	if err := json.Unmarshal([]byte(e.MustRun("ssh", "ls", "--json").Stdout), &rows); err != nil || len(rows) != 1 || rows[0].Host != rem.alias || rows[0].State != "alive" {
		t.Fatalf("ssh ls: %v %v", rows, err)
	}
	if out := e.MustRun("ssh", "login", rem.alias).Stdout; !strings.Contains(out, "already logged in") {
		t.Errorf("second login: %q", out)
	}

	d := itest.StartDaemon(t, e, "--housekeeping-interval", "1h")
	for _, r := range []string{ref, refRO, refB} {
		waitBox(t, e, r, proto.ChannelConnected, "")
	}

	// 11. Nothing of the gate directory is visible in the box.
	canary := "canary-" + itest.Nonce
	writeFile(t, filepath.Join(rt, canary), "x")
	script := `grep -c -e '` + filepath.Base(e.RuntimeDir) + `/gate' -e 'local/gate' /proc/self/mountinfo; find / \( -path /proc -o -path /sys -o -path /dev \) -prune -o \( -name 'm-*.sock' -o -name 'm-*.host' -o -name 'sshd-*.conf' -o -name 'host_ed25519*' -o -name '` + canary + `' \) -print 2>/dev/null; ls -A /run/devc`
	// find prints absolute paths; grep a count; ls the bare names.
	if got := boxRun(e, ref, script); strings.Contains(got.Stdout, "\n/") || !strings.HasPrefix(got.Stdout, "0\n") || strings.Contains(got.Stdout, "gate") || !strings.Contains(got.Stdout, "agent.sock") {
		t.Errorf("the gate directory is visible in the box:\n%s", got.Stdout)
	}

	// 2. A command runs with its output and its exit status.
	if got := runOK(t, e, ref, "ssh", rem.alias, "echo", "hello"); got != "hello" {
		t.Fatalf("ssh echo hello: %q\ndaemon:\n%s", got, d.Log())
	}
	if res := e.Run("run", ref, "--", "ssh", rem.alias, "exit 7"); res.ExitCode != 7 {
		t.Errorf("failing command: exit %d %q", res.ExitCode, res.Stderr)
	}

	// 6. Forwarding is refused: -L, -R, -D, -W (what ProxyJump uses) and
	// the agent. The targets are the remote sshd's port on the host's
	// loopback, so a forward that got through would read its banner.
	socksPort := fmt.Sprintf(`\%03o\%03o`, rem.port>>8, rem.port&255)
	fwd := fmt.Sprintf(`
ssh -o ExitOnForwardFailure=yes -L 127.0.0.1:41901:127.0.0.1:%[1]d %[2]s 'sleep 8' & S=$!
sleep 3
echo "L:[$(timeout 6 bash -c 'exec 3<>/dev/tcp/127.0.0.1/41901 && timeout 4 head -c 16 <&3' 2>&1 | tr -d '\0')]"
kill $S 2>/dev/null; wait $S 2>/dev/null
ssh -o ExitOnForwardFailure=yes -R 127.0.0.1:41902:127.0.0.1:%[1]d %[2]s 'sleep 2'; echo "R:$?"
ssh -N -D 127.0.0.1:41903 %[2]s & S=$!
sleep 3
printf 'D:[%%s]\n' "$(timeout 6 bash -c 'exec 3<>/dev/tcp/127.0.0.1/41903 || exit 9; printf "\005\001\000" >&3; sleep 1; printf "\005\001\000\001\177\000\000\001%[3]s" >&3; timeout 4 cat <&3' 2>&1 | od -An -tx1 | tr -d ' \n')"
kill $S 2>/dev/null; wait $S 2>/dev/null
echo "W:[$(timeout 6 ssh -W 127.0.0.1:%[1]d %[2]s </dev/null 2>/dev/null | head -c 16)]"
eval "$(ssh-agent -s)" >/dev/null
ssh -A %[2]s 'echo "agent:[${SSH_AUTH_SOCK-}]"'
ssh-agent -k >/dev/null
`, rem.port, rem.alias, socksPort)
	res = boxRun(e, ref, fwd)
	t.Logf("forwarding attempts in the box:\n%s", res.Stdout)
	// D is printed in hex; 5353482d322e30 is "SSH-2.0".
	if strings.Contains(res.Stdout, "SSH-2.0") || strings.Contains(res.Stdout, "5353482d322e30") || !strings.Contains(res.Stdout, "L:[") || !strings.Contains(res.Stdout, "D:[") || !strings.Contains(res.Stdout, "W:[]") {
		t.Errorf("forwarding through the gate:\n%s\n%s", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "R:255") || !strings.Contains(res.Stderr, "forwarding failed") {
		t.Errorf("remote forwarding: %q %q", res.Stdout, res.Stderr)
	}
	if c, err := net.Dial("tcp", "127.0.0.1:41902"); err == nil {
		c.Close()
		t.Error("a remote forward listens on the host")
	}
	if !strings.Contains(res.Stdout, "agent:[") || strings.Contains(res.Stdout, "/agent.") {
		t.Errorf("agent forwarding: %q", res.Stdout)
	}

	// 3. Quoting and pipes behave as over direct SSH.
	for _, c := range []string{`echo "a b" | wc -w`, `printf '%s|' "$((6*7))" 'x  y' "it's"; echo`, `false || echo or; test -d / && echo and`} {
		want, wantCode := rem.direct(t, c)
		res := e.Run("run", ref, "--", "ssh", rem.alias, c)
		if res.Stdout != want || res.ExitCode != wantCode {
			t.Errorf("%s: through the gate %q (exit %d), direct %q (exit %d)", c, res.Stdout, res.ExitCode, want, wantCode)
		}
	}
	if got := runOK(t, e, ref, "ssh", rem.alias, `echo "a b" | wc -w`); got != "2" {
		t.Errorf("pipe: %q", got)
	}

	// 4. scp, sftp, rsync and git over the gate.
	writeFile(t, filepath.Join(rem.files, "down.txt"), "from the remote\n")
	hostGit(t, "", "init", "-q", "--bare", filepath.Join(rem.files, "repo.git"))
	work := t.TempDir()
	hostGit(t, "", "clone", "-q", filepath.Join(rem.files, "repo.git"), work)
	writeFile(t, filepath.Join(work, "f"), "content\n")
	hostGit(t, work, "add", "f")
	hostGit(t, work, "commit", "-qm", "over the gate")
	hostGit(t, work, "push", "-q", "origin", "HEAD")
	files := rem.files
	xfer := fmt.Sprintf(`set -e
cd /tmp
echo "from the box" > up.txt
scp -q up.txt %[1]s:%[2]s/up-scp.txt
scp -q -O up.txt %[1]s:%[2]s/up-scp-legacy.txt
scp -q %[1]s:%[2]s/down.txt down-scp.txt
printf 'put up.txt %[2]s/up-sftp.txt\nget %[2]s/down.txt down-sftp.txt\n' > batch
sftp -q -b batch %[1]s >/dev/null
cat down-scp.txt down-sftp.txt
git clone -q %[1]s:%[2]s/repo.git clone
git -C clone log --format=%%s
`, rem.alias, files)
	res = boxRun(e, ref, xfer)
	if res.ExitCode != 0 || res.Stdout != "from the remote\nfrom the remote\nover the gate\n" {
		t.Errorf("transfers: exit %d\n%s\n%s\ndaemon:\n%s", res.ExitCode, res.Stdout, res.Stderr, d.Log())
	}
	for _, f := range []string{"up-scp.txt", "up-scp-legacy.txt", "up-sftp.txt"} {
		if b, err := os.ReadFile(filepath.Join(files, f)); err != nil || string(b) != "from the box\n" {
			t.Errorf("%s on the remote: %q %v", f, b, err)
		}
	}
	if boxRun(e, ref, "command -v rsync").ExitCode == 0 {
		res := boxRun(e, ref, fmt.Sprintf("cd /tmp && rsync -e ssh up.txt %[1]s:%[2]s/up-rsync.txt && rsync -e ssh %[1]s:%[2]s/down.txt down-rsync.txt && cat down-rsync.txt", rem.alias, files))
		if res.ExitCode != 0 || res.Stdout != "from the remote\n" {
			t.Errorf("rsync: exit %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
		}
	} else {
		t.Log("rsync is not in the box image; the rsync check is skipped")
	}

	// 5. A terminal on request.
	if got := runOK(t, e, ref, "ssh", "-tt", rem.alias, "tty"); !strings.Contains(got, "/dev/pts/") {
		t.Errorf("ssh -tt tty: %q", got)
	}
	// A shell without a terminal reads its commands from standard input.
	if res := e.RunIn(e.Repo, "echo marker-$((6*7))\nexit 3\n", "run", ref, "--", "ssh", "-T", rem.alias); !strings.Contains(res.Stdout, "marker-42") || res.ExitCode != 3 {
		t.Errorf("shell without a terminal: exit %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}

	// 7. A host that is not granted is refused, and another box's key does
	// not open this box's stream.
	if res := e.Run("run", ref, "--", "devc", "box", "gate-connect", rem.other); res.ExitCode != 255 || !strings.Contains(res.Stderr, "not granted") {
		t.Errorf("host not granted: exit %d %q", res.ExitCode, res.Stderr)
	}
	me, _ := user.Current()
	bKey, err := os.ReadFile(filepath.Join(e.RuntimeDir, boxID(refB)+"-secrets", "gate_key"))
	if err != nil {
		t.Fatal(err)
	}
	realm, name, _ := strings.Cut(ref, "/")
	if err := os.WriteFile(filepath.Join(e.ProjectsRoot, realm, name, "bkey"), bKey, 0o600); err != nil {
		t.Fatal(err)
	}
	withKey := func(key string) itest.Result {
		return e.Run("run", ref, "--", "ssh", "-F", "/dev/null", "-o", "ProxyCommand=/opt/devc/bin/devc box gate-connect "+rem.alias,
			"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-i", key, "-o", "HostKeyAlias=devc-gate",
			"-o", "UserKnownHostsFile=/etc/devc/gate_known_hosts", "-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes",
			me.Username+"@"+rem.alias, "echo", "in")
	}
	if res := withKey("/w/" + ref + "/bkey"); res.ExitCode == 0 || !strings.Contains(res.Stderr, "Permission denied") {
		t.Errorf("another box's key: exit %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res := withKey("/run/devc-secrets/gate_key"); res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "in" {
		t.Errorf("own key: exit %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}

	// 10. The read-only profile.
	if got := runOK(t, e, refRO, "ssh", rem.alias, "echo", "ro-ok"); got != "ro-ok" {
		t.Errorf("allowed command: %q", got)
	}
	for _, c := range []struct {
		argv []string
		msg  string
	}{
		{[]string{"ssh", rem.alias, "id"}, "not allowed"},
		{[]string{"ssh", rem.alias, "echo a; id"}, "shell syntax"},
		{[]string{"ssh", rem.alias, "echo $(id)"}, "shell syntax"},
		{[]string{"ssh", rem.alias, "echo a | cat"}, "shell syntax"},
		{[]string{"ssh", "-tt", rem.alias}, "interactive shell"},
		{[]string{"sh", "-c", "echo ls | sftp -b - " + rem.alias}, ""},
		{[]string{"scp", rem.alias + ":" + files + "/down.txt", "/tmp/x"}, ""},
	} {
		res := e.Run(append([]string{"run", refRO, "--"}, c.argv...)...)
		if res.ExitCode == 0 || !strings.Contains(res.Stderr+res.Stdout, c.msg) {
			t.Errorf("read-only %v: exit %d %q %q", c.argv, res.ExitCode, res.Stdout, res.Stderr)
		}
	}

	// 8. After devc ssh close the box gets one line and status 255.
	if out := e.MustRun("ssh", "close", rem.alias).Stdout; !strings.Contains(out, "closed") {
		t.Errorf("ssh close: %q", out)
	}
	res = e.Run("run", ref, "--", "ssh", rem.alias, "echo", "after")
	if res.ExitCode != 255 || !strings.Contains(res.Stderr, "devc ssh login "+rem.alias) || strings.Contains(res.Stdout, "after") {
		t.Errorf("after close: exit %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if err := json.Unmarshal([]byte(e.MustRun("ssh", "ls", "--json").Stdout), &rows); err != nil || len(rows) != 0 {
		t.Errorf("ssh ls after close: %v %v", rows, err)
	}

	// 9. The gate log has the commands.
	var hello, failed, sftp, noMaster, roRefused, roOK, legacy bool
	for _, g := range readGateLog(t, e) {
		switch {
		case g.Project == ref && g.Command == "echo hello" && g.Exit == 0 && g.Mode == "exec":
			hello = true
		case g.Project == ref && g.Command == "exit 7" && g.Exit == 7:
			failed = true
		case g.Project == ref && g.Mode == "sftp" && g.Exit == 0:
			sftp = true
		case g.Project == ref && strings.HasPrefix(g.Command, "scp -t") && g.Exit == 0:
			legacy = true
		case g.Project == ref && g.Refused == "no master" && g.Exit == 255:
			noMaster = true
		case g.Project == refRO && strings.HasPrefix(g.Refused, "read-only") && g.Host == rem.alias:
			roRefused = true
		case g.Project == refRO && g.Command == "echo ro-ok" && g.Exit == 0:
			roOK = true
		}
	}
	if !hello || !failed || !sftp || !legacy || !noMaster || !roRefused || !roOK {
		t.Errorf("gate log: hello %v, failed %v, sftp %v, scp -t %v, no master %v, read-only refused %v, read-only ok %v", hello, failed, sftp, legacy, noMaster, roRefused, roOK)
	}

	// The box's public key goes when the box stops.
	e.MustRun("down", refB)
	if _, err := os.Stat(filepath.Join(e.StateDir, "local", "gate", "keys", boxID(refB)+".pub")); !os.IsNotExist(err) {
		t.Errorf("the key of a stopped box is still authorized: %v", err)
	}
}
