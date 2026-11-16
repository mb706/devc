//go:build integration

package integration

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/display"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/test/integration/itest"
)

var (
	desktopOnce sync.Once
	desktopErr  error
	desktopSkip string
)

// ensureDesktopImage builds localhost/devc-aux/desktop once per run; the
// image is kept between runs (itest.KeptImages). Without network access
// for the first build the test is skipped, not failed.
func ensureDesktopImage(t *testing.T, e *itest.Env) {
	t.Helper()
	desktopOnce.Do(func() {
		old := e.Timeout
		e.Timeout = 30 * time.Minute
		defer func() { e.Timeout = old }()
		res := e.Run("gui", "build")
		if res.ExitCode == 0 {
			return
		}
		if !reachable("deb.debian.org:80") || !reachable("registry-1.docker.io:443") {
			desktopSkip = "the desktop image localhost/devc-aux/desktop is not built and its build cannot reach the network " +
				"(deb.debian.org, registry-1.docker.io); build it once with 'devc gui build' while online"
			return
		}
		desktopErr = fmt.Errorf("devc gui build: exit %d\n%s", res.ExitCode, res.Stderr)
	})
	if desktopSkip != "" {
		t.Skip(desktopSkip)
	}
	if desktopErr != nil {
		t.Fatal(desktopErr)
	}
}

func reachable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

var guiURLRE = regexp.MustCompile(`desktop of \S+: (http://127\.0\.0\.1:(\d+)/vnc\.html)#autoconnect=1&resize=remote&password=([A-Za-z0-9]{8})\n`)

type viewer struct {
	base     string // http://127.0.0.1:<port>/vnc.html
	addr     string
	password string
}

func guiUp(t *testing.T, e *itest.Env, ref string, args ...string) viewer {
	t.Helper()
	res := e.MustRun(append([]string{"gui", "up", ref}, args...)...)
	m := guiURLRE.FindStringSubmatch(res.Stdout)
	if m == nil || !strings.Contains(res.Stdout, "DISPLAY=127.0.0.1:1") {
		t.Fatalf("gui up output:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	return viewer{base: m[1], addr: "127.0.0.1:" + m[2], password: m[3]}
}

type guiStatusRow struct {
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Viewer  string `json:"viewer"`
	Display string `json:"display"`
}

func guiStatus(t *testing.T, e *itest.Env, ref string) guiStatusRow {
	t.Helper()
	var st guiStatusRow
	res := e.MustRun("gui", "status", ref, "--json")
	if err := json.Unmarshal([]byte(res.Stdout), &st); err != nil {
		t.Fatalf("gui status: %v\n%s", err, res.Stdout)
	}
	return st
}

func sidecarName(ref string) string { return boxName(ref) + "_gui" }

func containerExists(name string) bool {
	return exec.Command(itest.Engine(), "container", "exists", name).Run() == nil
}

// viewerAnswers fetches the noVNC page through the forwarded port.
func viewerAnswers(v viewer) error {
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(v.base)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 || !strings.Contains(string(b), "noVNC") {
		return fmt.Errorf("status %d, body %.200q", resp.StatusCode, b)
	}
	return nil
}

// rfbAuth runs the VNC handshake up to the security result: the types the
// server offers, and the result of VNC authentication with password (0
// is success).
func rfbAuth(addr, password string) ([]byte, uint32, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, 0, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	ver := make([]byte, 12)
	if _, err := io.ReadFull(c, ver); err != nil || !strings.HasPrefix(string(ver), "RFB 003.") {
		return nil, 0, fmt.Errorf("protocol version %q: %v", ver, err)
	}
	if _, err := c.Write([]byte("RFB 003.008\n")); err != nil {
		return nil, 0, err
	}
	var n [1]byte
	if _, err := io.ReadFull(c, n[:]); err != nil || n[0] == 0 {
		return nil, 0, fmt.Errorf("security types: %d %v", n[0], err)
	}
	types := make([]byte, n[0])
	if _, err := io.ReadFull(c, types); err != nil {
		return nil, 0, err
	}
	if !strings.ContainsRune(string(types), 2) {
		return types, 0, nil
	}
	if _, err := c.Write([]byte{2}); err != nil {
		return types, 0, err
	}
	ch := make([]byte, 16)
	if _, err := io.ReadFull(c, ch); err != nil {
		return types, 0, err
	}
	resp, err := display.VNCResponse(ch, password)
	if err != nil {
		return types, 0, err
	}
	if _, err := c.Write(resp); err != nil {
		return types, 0, err
	}
	var res [4]byte
	if _, err := io.ReadFull(c, res[:]); err != nil {
		return types, 0, err
	}
	return types, binary.BigEndian.Uint32(res[:]), nil
}

// wsUpgrade asks the viewer's server for its WebSocket (the path noVNC
// uses) and returns the status line.
func wsUpgrade(addr string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "GET /websockify HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: binary\r\n\r\n", addr)
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), err
}

func sidecarExec(t *testing.T, ref string, argv ...string) (string, error) {
	t.Helper()
	out, err := exec.Command(itest.Engine(), append([]string{"exec", sidecarName(ref)}, argv...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestGUI covers C19's items 1 to 5 on a box of level internet.
func TestGUI(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	ensureDesktopImage(t, e)
	if st := guiStatus(t, e, hello); st.State != "absent" {
		t.Errorf("status before: %+v", st)
	}
	// gui up starts the box too.
	v := guiUp(t, e, hello)
	if st := guiStatus(t, e, hello); st.State != "running" || st.Viewer != v.base || st.Display != "127.0.0.1:1" {
		t.Errorf("status: %+v (viewer %s)", st, v.base)
	}

	// 1. The viewer answers through the forwarded port, its WebSocket too;
	// the VNC port offers only VNC authentication and wants the password.
	if err := viewerAnswers(v); err != nil {
		t.Errorf("viewer: %v", err)
	}
	if line, err := wsUpgrade(v.addr); err != nil || !strings.HasPrefix(line, "HTTP/1.1 101") {
		t.Errorf("websocket: %q %v", line, err)
	}
	e.MustRun("forward", "5901", hello, "--detach", "--host-port", "0")
	vncAddr := ""
	for _, r := range forwards(t, e) {
		if r.BoxPort == 5901 {
			vncAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(r.HostPort))
		}
	}
	if types, res, err := rfbAuth(vncAddr, v.password); err != nil || string(types) != "\x02" || res != 0 {
		t.Errorf("VNC with the password: types %v result %d %v", types, res, err)
	}
	if types, res, err := rfbAuth(vncAddr, "wrongpw1"); err != nil || string(types) != "\x02" || res == 0 {
		t.Errorf("VNC with a wrong password: types %v result %d %v", types, res, err)
	}

	// 2. A box process with the cookie gets the display; without, not.
	if got := runOK(t, e, hello, "/opt/devc/bin/devc", "box", "xcheck"); !strings.HasPrefix(got, "accepted: display 127.0.0.1:1 is The X.Org Foundation") ||
		!strings.Contains(got, "screen 1600x900") {
		t.Errorf("xcheck: %q", got)
	}
	res := e.Run("run", hello, "--", "/opt/devc/bin/devc", "box", "xcheck", "--no-auth")
	if res.ExitCode != 1 || !strings.Contains(res.Stdout, "refused: Authorization required") {
		t.Errorf("xcheck without the cookie: %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := runOK(t, e, hello, "stat", "-c", "%a %U", "/run/devc/display.xauth"); got != "600 dev" {
		t.Errorf("cookie file: %q", got)
	}
	if got := runOK(t, e, hello, "cat", "/etc/devc/display.env"); got != "DISPLAY=127.0.0.1:1\nXAUTHORITY=/run/devc/display.xauth" {
		t.Errorf("display.env: %q", got)
	}
	// X listens on loopback only: from the box's other address it does
	// not answer (the abstract socket is off too).
	if ok, _ := connect(e, hello, "127.0.0.1:6001", 5); !ok {
		t.Error("X does not listen on 127.0.0.1:6001")
	}
	addrs := runOK(t, e, hello, "sh", "-c", "hostname -I")
	for _, a := range strings.Fields(addrs) {
		for _, p := range []string{"6001", "5901", "6080"} {
			if ok, _ := connect(e, hello, net.JoinHostPort(a, p), 3); ok {
				t.Errorf("the desktop listens on %s:%s", a, p)
			}
		}
	}
	if got := runOK(t, e, hello, "sh", "-c", "grep -c '@/tmp/.X11-unix' /proc/net/unix || true"); got != "0" {
		t.Errorf("abstract X sockets in the box's namespace: %s", got)
	}

	// 3. No capabilities, no new privileges, no project, nothing mounted
	// but a tmpfs, a read-only root, the box's user.
	ci, err := engine.ParseInspect([]byte(itest.Podman(t, "container", "inspect", "--format", "json", sidecarName(hello))))
	if err != nil {
		t.Fatal(err)
	}
	if len(ci.EffectiveCaps) != 0 || len(ci.BoundingCaps) != 0 || ci.HostConfig.Privileged {
		t.Errorf("capabilities %v %v", ci.EffectiveCaps, ci.BoundingCaps)
	}
	for _, m := range ci.Mounts {
		if m.Type != "tmpfs" {
			t.Errorf("mount %+v", m)
		}
	}
	if box := inspect(t, hello); ci.HostConfig.NetworkMode != "container:"+box.ID {
		t.Errorf("network mode %q", ci.HostConfig.NetworkMode)
	}
	status, err := sidecarExec(t, hello, "cat", "/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CapEff:\t0000000000000000", "CapBnd:\t0000000000000000", "CapPrm:\t0000000000000000", "NoNewPrivs:\t1", "Seccomp:\t2"} {
		if !strings.Contains(status, want) {
			t.Errorf("sidecar lacks %q:\n%s", want, status)
		}
	}
	if out, err := sidecarExec(t, hello, "id", "-u"); err != nil || out != "1000" {
		t.Errorf("uid %q %v", out, err)
	}
	for _, p := range []string{"/w", "/home/dev", "/opt/devc", "/etc/devc", "/run/devc"} {
		if out, err := sidecarExec(t, hello, "test", "-e", p); err == nil {
			t.Errorf("%s exists in the sidecar %s", p, out)
		}
	}
	if _, err := sidecarExec(t, hello, "touch", "/usr/local/bin/x"); err == nil {
		t.Error("the sidecar's root is writable")
	}

	// The desktop has limits of its own (the [gui] defaults), in force in
	// its cgroup (finding D2-01).
	if hc := ci.HostConfig; hc.Memory != 2<<30 || hc.PidsLimit != 512 || hc.NanoCpus != 2e9 {
		t.Errorf("sidecar limits %+v", hc)
	}
	if out, err := sidecarExec(t, hello, "cat", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/pids.max", "/sys/fs/cgroup/cpu.max"); err != nil ||
		out != "2147483648\n512\n200000 100000" {
		t.Errorf("sidecar cgroup limits %q %v", out, err)
	}
	// The relays fork a bounded number of processes for the box's
	// connections: 24 viewer and 66 X connections held open in the box give
	// 16 websockify handlers and 64 socat children; the rest wait. Each
	// viewer connection sends the start of a request and stops, which keeps
	// its handler waiting (an idle connection would end after websockify's
	// 3 s handshake timeout, and a WebSocket would reach Xvnc, which refuses
	// a sixth unauthenticated connection from 127.0.0.1).
	relays := func() (web, x int) {
		out, _ := sidecarExec(t, hello, "sh", "-c", "for p in /proc/[0-9]*; do cat $p/comm 2>/dev/null; done")
		for _, l := range strings.Split(out, "\n") {
			switch l {
			case "python3":
				web++
			case "socat":
				x++
			}
		}
		return web, x
	}
	// A handler of an earlier request may still be ending; it counts
	// towards the bound like any other.
	web0, x0 := relays()
	if web0 < 1 || web0 > 3 || x0 != 1 {
		t.Errorf("relay processes before: websockify %d, socat %d", web0, x0)
	}
	hold := exec.Command(itest.Engine(), "exec", boxName(hello), "bash", "-c",
		`req=$'GET /vnc.html HTTP/1.1\r\nHost: 127.0.0.1:6080\r\n'; `+
			`for i in $(seq 24); do exec {fd}<>/dev/tcp/127.0.0.1/6080 || exit 1; printf '%s' "$req" >&$fd; done; `+
			`for i in $(seq 66); do exec {fd}<>/dev/tcp/127.0.0.1/6001 || exit 1; done; echo opened; sleep 15`)
	var holdOut strings.Builder
	hold.Stdout, hold.Stderr = &holdOut, &holdOut
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	var web, x int
	pollFor(20*time.Second, func() bool { web, x = relays(); return web >= 17 && x >= 65 })
	time.Sleep(time.Second)
	web, x = relays()
	if err := hold.Wait(); err != nil || !strings.Contains(holdOut.String(), "opened") {
		t.Errorf("holding connections in the box: %v %s", err, holdOut.String())
	}
	if web != 17 || x != 65 {
		t.Errorf("relay processes with the connections held: websockify %d (want 1+16), socat %d (want 1+64)", web, x)
	}
	if !pollFor(20*time.Second, func() bool { web, x = relays(); return web <= web0 && x == x0 }) {
		t.Errorf("relay processes after the connections closed: websockify %d, socat %d", web, x)
	}
	if err := viewerAnswers(v); err != nil {
		t.Errorf("viewer after the connections: %v", err)
	}

	// Joining disturbed neither the readiness nor the rules (C07r).
	if res := e.MustRun("up", hello); !strings.Contains(res.Stderr+res.Stdout, "already running") {
		t.Errorf("up after gui up: %s%s", res.Stdout, res.Stderr)
	}
	if !rulesInPlace(t, hello) {
		t.Error("the rules are gone")
	}
	if ok, msg := connect(e, hello, "192.0.2.1:9", 5); ok || !strings.Contains(msg, "refused") {
		t.Errorf("sentinel after gui up: %v %s", ok, msg)
	}
	// gui up again keeps the desktop and prints the same address.
	if v2 := guiUp(t, e, hello); v2 != v {
		t.Errorf("second gui up: %+v, first %+v", v2, v)
	}

	// 4. down removes the sidecar first; status before and after.
	e.MustRun("down", hello)
	if containerExists(sidecarName(hello)) {
		t.Error("the sidecar survived down")
	}
	if st := guiStatus(t, e, hello); st.State != "absent" {
		t.Errorf("status after down: %+v", st)
	}
	if !pollFor(10*time.Second, func() bool { return viewerAnswers(v) != nil }) {
		t.Error("the viewer still answers after down")
	}

	// 5. devc browser forwards a fake debugging endpoint in the box.
	e.MustRun("up", hello)
	script := `use IO::Socket::INET; my $s = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => 9222, Listen => 5, ReuseAddr => 1) or die "listen: $!";` +
		` while (my $c = $s->accept) { while (my $h = <$c>) { last if $h =~ /^\r?\n$/ }` +
		` my $b = '{"Browser":"FakeChrome/1.0","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/x"}';` +
		` print $c "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " . length($b) . "\r\nConnection: close\r\n\r\n$b"; close $c }`
	e.MustRun("run", hello, "--", "setsid", "-f", "perl", "-e", script)
	if !pollFor(10*time.Second, func() bool { ok, _ := connect(e, hello, "127.0.0.1:9222", 2); return ok }) {
		t.Fatal("the fake debugging endpoint did not come up")
	}
	res = e.MustRun("browser", hello, "--detach", "--host-port", "0")
	m := regexp.MustCompile(`DevTools endpoint: (http://127\.0\.0\.1:\d+/json/version)`).FindStringSubmatch(res.Stdout)
	if m == nil || !strings.Contains(res.Stdout, "chrome://inspect") || strings.Contains(res.Stderr, "no browser answers") {
		t.Fatalf("browser:\n%s\n%s", res.Stdout, res.Stderr)
	}
	resp, err := http.Get(m[1])
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "FakeChrome/1.0") {
		t.Errorf("/json/version: %s", body)
	}
	// Without a browser in the box, devc says what to start.
	res = e.MustRun("browser", hello, "--detach", "--host-port", "0", "--port", "9333")
	if !strings.Contains(res.Stderr, "--remote-debugging-port=9333 --remote-debugging-address=127.0.0.1 --no-sandbox") {
		t.Errorf("hint: %s", res.Stderr)
	}
	for _, r := range forwards(t, e) {
		e.MustRun("forward", "stop", r.ID)
	}
}

// TestGUILevels: the desktop starts at levels open, none and allowlist
// (internet is TestGUI's), shares the box's restrictions and leaves the
// box's start alone; rm removes it, and a stale one goes at the next start.
func TestGUILevels(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	ensureDesktopImage(t, e)
	for _, level := range []string{"open", "none", "allowlist"} {
		t.Run(level, func(t *testing.T) {
			project(e, hello, "[network]\nlevel = \""+level+"\"\n")
			e.MustRun("up", hello)
			ifaces := runOK(t, e, hello, "sh", "-c", "tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' ' | sort | tr '\\n' ' '")
			v := guiUp(t, e, hello)
			if err := viewerAnswers(v); err != nil {
				t.Errorf("viewer: %v", err)
			}
			if got := runOK(t, e, hello, "/opt/devc/bin/devc", "box", "xcheck"); !strings.HasPrefix(got, "accepted:") {
				t.Errorf("xcheck: %q", got)
			}
			if res := e.Run("run", hello, "--", "/opt/devc/bin/devc", "box", "xcheck", "--no-auth"); res.ExitCode != 1 {
				t.Errorf("xcheck without the cookie: %d %s", res.ExitCode, res.Stdout)
			}
			// The same namespace: the sidecar sees the box's interfaces.
			side, err := sidecarExec(t, hello, "sh", "-c", "tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' ' | sort | tr '\\n' ' '")
			if err != nil || side != strings.TrimSpace(ifaces) {
				t.Errorf("interfaces: box %q, sidecar %q (%v)", ifaces, side, err)
			}
			if level != "open" && strings.TrimSpace(ifaces) != "lo" {
				t.Errorf("interfaces at level %s: %q", level, ifaces)
			}
			if res := e.MustRun("up", hello); !strings.Contains(res.Stderr+res.Stdout, "already running") {
				t.Errorf("up after gui up: %s%s", res.Stdout, res.Stderr)
			}
			switch level {
			case "open":
				// rm removes the sidecar first.
				e.MustRun("rm", hello)
				if containerExists(sidecarName(hello)) {
					t.Error("the sidecar survived rm")
				}
			case "none":
				// The box stopped outside devc: the sidecar is stale, and
				// the next start removes it.
				itest.Podman(t, "stop", "--time", "2", boxName(hello))
				if st := guiStatus(t, e, hello); st.State != "stale" {
					t.Errorf("status after the box stopped: %+v", st)
				}
				e.MustRun("up", hello)
				if containerExists(sidecarName(hello)) {
					t.Error("the stale sidecar survived the start")
				}
				if res := e.Run("run", hello, "--", "test", "-e", "/etc/devc/display.env"); res.ExitCode == 0 {
					t.Error("display.env survived the start")
				}
				e.MustRun("rm", hello)
			default:
				e.MustRun("gui", "down", hello)
				if containerExists(sidecarName(hello)) {
					t.Error("the sidecar survived gui down")
				}
				if st := guiStatus(t, e, hello); st.State != "absent" {
					t.Errorf("status after gui down: %+v", st)
				}
				e.MustRun("rm", hello)
			}
		})
	}
}
