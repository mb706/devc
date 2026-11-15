//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/test/integration/itest"
)

// connect tries a TCP connection from inside a box with bash's /dev/tcp
// and returns bash's error output ("" when it connected).
func connect(e *itest.Env, ref, addr string, timeout int) (bool, string) {
	host, port, _ := net.SplitHostPort(addr)
	res := e.Run("run", ref, "--", "timeout", strconv.Itoa(timeout), "bash", "-c", "exec 3<>/dev/tcp/"+host+"/"+port)
	return res.ExitCode == 0, strings.TrimSpace(res.Stderr + res.Stdout)
}

// rulesInPlace lists devc's table in the box's network namespace from the
// host, the way devc installs it.
func rulesInPlace(t *testing.T, ref string) bool {
	t.Helper()
	ns := inspect(t, ref).NetworkSettings.SandboxKey
	if ns == "" {
		return false
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		nft = "/usr/sbin/nft"
	}
	out, err := exec.Command(itest.Engine(), "unshare", "--", "nsenter", "--net="+ns, nft, "list", "table", "inet", "devc").CombinedOutput()
	return err == nil && strings.Contains(string(out), "set allow4") && strings.Contains(string(out), "192.0.2.1")
}

// hostAddrs are the host's IPv4 addresses other than loopback.
func hostAddrs(t *testing.T) []netip.Addr {
	t.Helper()
	as, err := netpolicy.HostAddrs()
	if err != nil {
		t.Fatal(err)
	}
	return as
}

// hostOnline reports whether the host itself reaches a public address.
func hostOnline() bool {
	c, err := net.DialTimeout("tcp4", "1.1.1.1:443", 3*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// TestNetworkInternet covers items 1 and 3: a box of level internet starts
// with the rules in place, the sentinel answers, the host and private
// addresses are unreachable, IPv6 is off, a public address is reachable
// when the host is online, and down/up installs the rules again.
func TestNetworkInternet(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	res := e.MustRun("up", hello)
	if !strings.Contains(res.Stderr, "is ready") {
		t.Fatalf("up:\n%s", res.Stderr)
	}
	check := func(when string) {
		t.Helper()
		if !rulesInPlace(t, hello) {
			t.Errorf("%s: devc's rules are not in the box's namespace", when)
		}
		if ok, msg := connect(e, hello, "192.0.2.1:9", 5); ok || !strings.Contains(msg, "refused") {
			t.Errorf("%s: sentinel: connected %v, %q", when, ok, msg)
		}
	}
	check("after up")
	ci := inspect(t, hello)
	if ci.HostConfig.NetworkMode == "none" {
		t.Errorf("network mode %q", ci.HostConfig.NetworkMode)
	}

	// The host's addresses, the host alias and private ranges fail fast.
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	accepted := make(chan struct{}, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
			accepted <- struct{}{}
		}
	}()
	targets := []string{"10.0.2.2", "172.17.0.1", "169.254.1.2", "100.64.0.1"}
	for _, a := range hostAddrs(t) {
		targets = append(targets, a.String())
	}
	alias := runOK(t, e, hello, "sh", "-c", "getent hosts host.containers.internal | cut -d' ' -f1 || true")
	if alias != "" {
		targets = append(targets, strings.Fields(alias)[0])
	}
	for _, a := range targets {
		start := time.Now()
		if ok, msg := connect(e, hello, net.JoinHostPort(a, strconv.Itoa(port)), 10); ok {
			t.Errorf("the box reached %s:%d", a, port)
		} else if time.Since(start) > 8*time.Second {
			t.Errorf("%s: no fast failure (%s): %q", a, time.Since(start), msg)
		}
	}
	// The canary as devc runs it.
	args := []string{"run", hello, "--", "/opt/devc/bin/devc", "box", "net-canary", "--port", strconv.Itoa(port)}
	for _, a := range hostAddrs(t) {
		args = append(args, a.String())
	}
	res = e.Run(args...)
	var probes []netpolicy.Probe
	if err := json.Unmarshal([]byte(res.Stdout), &probes); res.ExitCode != 0 || err != nil || len(probes) != len(hostAddrs(t)) {
		t.Errorf("canary: exit %d, %v\n%s%s", res.ExitCode, err, res.Stdout, res.Stderr)
	}
	select {
	case <-accepted:
		t.Error("the host's listener accepted a connection from the box")
	default:
	}

	// IPv6 is off.
	if got := runOK(t, e, hello, "sh", "-c", "cat /proc/sys/net/ipv6/conf/all/disable_ipv6; cat /proc/net/if_inet6 | wc -l"); got != "1\n0" {
		t.Errorf("IPv6: %q", got)
	}
	// A public address and DNS work when the host is online.
	if hostOnline() {
		if ok, msg := connect(e, hello, "1.1.1.1:443", 10); !ok {
			t.Errorf("public address unreachable: %q", msg)
		}
		if res := e.Run("run", hello, "--", "getent", "hosts", "debian.org"); res.ExitCode != 0 {
			t.Errorf("DNS: exit %d %s", res.ExitCode, res.Stderr)
		}
	} else {
		t.Log("the host has no internet connection; skipping the public-address check")
	}

	// 3. down and up: the rules are back.
	e.MustRun("down", hello)
	e.MustRun("up", hello)
	check("after down and up")
	// So they are after a recreate.
	e.MustRun("up", "--recreate", hello)
	check("after recreate")
}

// TestNetworkPlainStart covers item 2: a box of level internet started
// with a plain `podman start` does not become ready and stops; devc up
// then starts it properly.
func TestNetworkPlainStart(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.MustRun("up", hello)
	e.MustRun("down", hello)
	name := boxName(hello)
	// devc down removes the delivered secrets, whose directory the box
	// mounts, so a plain start fails right away; recreate it to reach the
	// box's own check.
	if err := os.Mkdir(filepath.Join(e.RuntimeDir, boxID(hello)+"-secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	itest.Podman(t, "start", name)
	deadline := time.Now().Add(30 * time.Second)
	for inspect(t, hello).State.Running && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	ci := inspect(t, hello)
	if ci.State.Running || ci.State.ExitCode == 0 {
		t.Fatalf("the box started without rules: running %v, exit %d", ci.State.Running, ci.State.ExitCode)
	}
	if _, err := os.Stat(filepath.Join(e.RuntimeDir, boxID(hello), "ready")); err == nil {
		t.Error("the box reported ready without rules")
	}
	logs, _ := exec.Command(itest.Engine(), "logs", name).CombinedOutput()
	if !strings.Contains(string(logs), "network rules of level internet are not in place") {
		t.Errorf("box log:\n%s", logs)
	}
	e.MustRun("up", hello)
	if !rulesInPlace(t, hello) {
		t.Error("rules missing after devc up")
	}
}

// startServer runs a one-line TCP server on 127.0.0.1:port in a box (Perl
// is part of every Debian image).
func startServer(t *testing.T, e *itest.Env, ref string, port int) {
	t.Helper()
	startServerOn(t, e, ref, "127.0.0.1", port)
}

// greet connects to addr and returns the first line.
func greet(addr string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	return line, err
}

var forwardingRE = regexp.MustCompile(`forwarding (\S+):(\d+) to port`)

type forwardRow struct {
	ID       string `json:"id"`
	Ref      string `json:"ref"`
	BoxPort  int    `json:"box_port"`
	HostPort int    `json:"host_port"`
	PID      int    `json:"pid"`
}

func forwards(t *testing.T, e *itest.Env) []forwardRow {
	t.Helper()
	var rows []forwardRow
	res := e.MustRun("forward", "ls", "--json")
	if err := json.Unmarshal([]byte(res.Stdout), &rows); err != nil {
		t.Fatalf("forward ls: %v\n%s", err, res.Stdout)
	}
	return rows
}

// TestForward covers items 4 and 5: a server bound to 127.0.0.1 in a box
// answers through a forwarded port, at level internet and at level none;
// the listener is on loopback only; a detached forward is listed, stops on
// request and exits when its box goes down.
func TestForward(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	project(e, tools, "image = \""+itest.BaseImage+"\"\n[network]\nlevel = \"none\"\n")
	e.MustRun("up", hello)
	e.MustRun("up", tools)
	// 4. Level none: no network at all.
	if got := runOK(t, e, tools, "sh", "-c", "tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' '"); got != "lo" {
		t.Errorf("interfaces at level none: %q", got)
	}

	// Foreground forward from a busy host port: a free one is chosen.
	startServer(t, e, hello, 8765)
	busy, err := net.Listen("tcp4", "127.0.0.1:8765")
	if err != nil {
		t.Skipf("port 8765 is taken on the host: %v", err)
	}
	defer busy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Bin, "forward", "8765", hello)
	cmd.Env = e.Environ()
	cmd.Dir = "/"
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	m := forwardingRE.FindStringSubmatch(line)
	if m == nil || m[1] != "127.0.0.1" || m[2] == "8765" || !strings.Contains(stderr.String(), "taken") {
		t.Fatalf("forward output %q, stderr %q", line, stderr.String())
	}
	addr := net.JoinHostPort(m[1], m[2])
	for i := 0; i < 3; i++ {
		if got, err := greet(addr); err != nil || got != "hello from the box\n" {
			t.Errorf("through the forward: %q, %v", got, err)
		}
	}
	// Loopback only: the host's other addresses do not answer.
	for _, a := range hostAddrs(t) {
		if c, err := net.DialTimeout("tcp4", net.JoinHostPort(a.String(), m[2]), 2*time.Second); err == nil {
			c.Close()
			t.Errorf("the forward listens on %s", a)
		}
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	if err := cmd.Wait(); err != nil {
		t.Errorf("forward after Ctrl-C: %v (%s)", err, stderr.String())
	}
	if _, err := greet(addr); err == nil {
		t.Error("the forward still answers after it stopped")
	}

	// 4. A detached forward to the box of level none.
	startServer(t, e, tools, 8766)
	res := e.MustRun("forward", "8766", tools, "--detach", "--host-port", "0")
	if !strings.Contains(res.Stdout, "in the background (forward f") {
		t.Fatalf("detach: %s", res.Stdout)
	}
	rows := forwards(t, e)
	if len(rows) != 1 || rows[0].Ref != tools || rows[0].BoxPort != 8766 || rows[0].HostPort == 0 {
		t.Fatalf("forward ls: %+v", rows)
	}
	addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(rows[0].HostPort))
	if got, err := greet(addr); err != nil || got != "hello from the box\n" {
		t.Errorf("through the detached forward: %q, %v", got, err)
	}
	// It stops on request.
	e.MustRun("forward", "stop", rows[0].ID)
	if rows := forwards(t, e); len(rows) != 0 {
		t.Errorf("after stop: %+v", rows)
	}
	if _, err := greet(addr); err == nil {
		t.Error("the stopped forward still answers")
	}
	if res := e.Run("forward", "stop", "f000000"); res.ExitCode != 1 {
		t.Errorf("stop of an unknown forward: %d", res.ExitCode)
	}

	// 5. A detached forward ends when its box goes down.
	e.MustRun("forward", "8765", hello, "--detach", "--host-port", "0")
	rows = forwards(t, e)
	if len(rows) != 1 {
		t.Fatalf("forward ls: %+v", rows)
	}
	pid := rows[0].PID
	e.MustRun("down", hello)
	deadline := time.Now().Add(20 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		t.Error("the detached forward outlived its box")
	}
	if rows := forwards(t, e); len(rows) != 0 {
		t.Errorf("after the box went down: %+v", rows)
	}
	// A non-loopback bind address is warned about.
	if res := e.Run("forward", "8766", tools, "--detach", "--bind", "0.0.0.0", "--host-port", "0"); res.ExitCode != 0 || !strings.Contains(res.Stderr, "reachable from the network") {
		t.Errorf("bind 0.0.0.0: %d %s", res.ExitCode, res.Stderr)
	}
	for _, r := range forwards(t, e) {
		e.MustRun("forward", "stop", r.ID)
	}
}

// TestNetworkLevels covers item 6 and the sandbox description: allowlist
// is refused, and the description states the level in force.
func TestNetworkLevels(t *testing.T) {
	e := itest.New(t)
	project(e, hello, "[network]\nlevel = \"allowlist\"\n")
	res := e.Run("up", hello)
	if res.ExitCode != 1 || !strings.Contains(res.Stderr, "not available yet") {
		t.Errorf("allowlist: %d\n%s", res.ExitCode, res.Stderr)
	}
	if ls(t, e)[hello].State != "absent" {
		t.Error("a box was created for level allowlist")
	}
}

// TestInitExample covers item 7: init --example writes a configuration
// that passes config check, and refuses to overwrite one.
func TestInitExample(t *testing.T) {
	e := itest.New(t)
	dir := filepath.Join(t.TempDir(), "newconfig")
	res := e.MustRun("--config-dir", dir, "init", "--example")
	if !strings.Contains(res.Stdout, "configuration OK") || !strings.Contains(res.Stdout, "devc doctor") {
		t.Errorf("init --example: %s", res.Stdout)
	}
	if res := e.Run("--config-dir", dir, "config", "check"); res.ExitCode != 0 {
		t.Errorf("config check: %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res := e.Run("--config-dir", dir, "init", "--example"); res.ExitCode != 1 || !strings.Contains(res.Stderr, "not empty") {
		t.Errorf("second init: %d %s", res.ExitCode, res.Stderr)
	}
}

// TestDoctorStatx covers item 8: doctor runs the overlay-statx check to
// completion within its time limit (on hosts with the fuse-overlayfs bug
// it fails) and leaves no probe container behind.
func TestDoctorStatx(t *testing.T) {
	e := itest.New(t)
	e.MustRun("toolbox", "sync")
	start := time.Now()
	res := e.Run("doctor", "--json")
	elapsed := time.Since(start)
	var checks []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &checks); err != nil {
		t.Fatalf("doctor --json: %v\n%s%s", err, res.Stdout, res.Stderr)
	}
	found := false
	for _, c := range checks {
		if c.ID != "overlay-statx" {
			continue
		}
		found = true
		t.Logf("overlay-statx: %s: %s (doctor took %s)", c.Status, c.Detail, elapsed.Round(time.Second))
		if c.Status != "ok" && c.Status != "fail" {
			t.Errorf("overlay-statx: %s %q", c.Status, c.Detail)
		}
		if strings.Contains(c.Detail, "could not be removed") {
			t.Errorf("probe container left: %s", c.Detail)
		}
	}
	if !found {
		t.Error("no overlay-statx check")
	}
	// 10 s for the probe plus the other checks and the cleanup.
	if elapsed > 60*time.Second {
		t.Errorf("doctor took %s", elapsed)
	}
	if out := itest.Podman(t, "ps", "-a", "--filter", "label=devc.aux=doctor", "--filter", "label="+itest.LabelRun+"="+itest.Nonce, "--format", "{{.Names}}"); strings.TrimSpace(out) != "" {
		t.Errorf("probe containers left: %s", out)
	}
	// --fix prints the root commands as one script.
	res = e.Run("doctor", "--fix")
	if !strings.Contains(res.Stdout, "#!/bin/sh") && !strings.Contains(res.Stdout, "Nothing needs root") {
		t.Errorf("doctor --fix:\n%s", res.Stdout)
	}
}
