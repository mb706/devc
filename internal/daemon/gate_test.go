package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// TestGateStreams: host.gate streams start the gate's sshd for granted
// hosts only, with that box's key and forced command, and the stream as
// sshd's standard input and output.
func TestGateStreams(t *testing.T) {
	r := newRig(t)
	proj := filepath.Join(r.p.ConfigDir(), "realms", "demo", "projects", "hello", "project.toml")
	if err := os.WriteFile(proj, []byte("harnesses = [\"claude\"]\n[grants]\nssh = [\"c1\", \"c2:ro\"]\n[grants.ssh_allow]\nc2 = [\"ls\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var confs []string
	var conf string
	r.runner.On("/fake/sshd").Do(func(c execx.Cmd) {
		// The "sshd": check the configuration, answer in upper case.
		conf = c.Args[len(c.Args)-1]
		b, _ := os.ReadFile(conf)
		confs = append(confs, string(b))
		if !slices.Equal(c.Args[:3], []string{"-i", "-e", "-f"}) || !c.Setsid {
			c.Stdout.Write([]byte("BAD INVOCATION"))
			return
		}
		// The fake runner has drained standard input already.
		stdins := r.runner.Stdins()
		in := stdins[len(stdins)-1]
		c.Stdout.Write([]byte("SSH " + strings.ToUpper(in)))
		c.Stderr.Write([]byte("Accepted publickey for test\n"))
	})
	r.boxUp(true)
	o := r.options()
	o.SSHD, o.SSHConfig = "/fake/sshd", "/t/ssh_config"
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	r.d, r.done = d, make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-r.done })
	r.agent(nil)
	r.waitChannel(r.client(nil), proto.ChannelConnected)

	connect := func(host, input string) (int, string, string) {
		var out, errb bytes.Buffer
		code := boxside.GateConnect(r.local, host, strings.NewReader(input), &out, &errb)
		return code, out.String(), errb.String()
	}
	for _, c := range []struct{ host, msg string }{
		{"c9", "c9 is not granted to demo/hello"},
		{"-oProxyCommand=x", "not a valid SSH host alias"},
		{"c1", "restart it"}, // no keys yet
	} {
		if code, out, errb := connect(c.host, "hello"); code != 255 || out != "" || !strings.Contains(errb, c.msg) || strings.Count(errb, "\n") != 1 {
			t.Errorf("%s: exit %d out %q err %q", c.host, code, out, errb)
		}
	}
	if len(confs) != 0 {
		t.Fatalf("sshd ran for a refused stream: %v", confs)
	}
	// With the host key and the box's key, granted hosts reach sshd.
	pub := "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte("key")) + " x\n"
	id := names.BoxID(hello)
	os.MkdirAll(r.p.GateKeysDir(), 0o700)
	os.WriteFile(r.p.GateHostKey(), []byte("PRIVATE"), 0o600)
	os.WriteFile(r.p.GateHostKey()+".pub", []byte(pub), 0o644)
	os.WriteFile(r.p.GateBoxKey(id), []byte("restrict,pty "+pub), 0o600)
	for _, host := range []string{"c1", "c2"} {
		code, out, errb := connect(host, "client bytes")
		if code != 0 || out != "SSH CLIENT BYTES" || errb != "" {
			t.Errorf("%s: exit %d out %q err %q", host, code, out, errb)
		}
	}
	if len(confs) != 2 {
		t.Fatalf("sshd runs: %d", len(confs))
	}
	rt, _ := r.p.RuntimeDir()
	for i, host := range []string{"c1", "c2"} {
		text := confs[i]
		for _, want := range []string{
			`AuthorizedKeysFile "` + r.p.GateBoxKey(id) + `"`,
			`HostKey "` + r.p.GateHostKey() + `"`,
			"ForceCommand '/fake/devc' '--config-dir' '" + r.p.ConfigDir() + "' '--state-dir' '" + r.p.StateDir() + "' 'gate-exec' '--box' '" + id + "' '--host' '" + host + "' '--runtime-dir' '" + rt + "' '--ssh-config' '/t/ssh_config'",
			"AllowTcpForwarding no", "DisableForwarding yes",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: configuration lacks %q:\n%s", host, want, text)
			}
		}
	}
	// The configuration file goes once sshd has ended.
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Errorf("sshd configuration left behind: %v", err)
	}
	// A grant removed from the configuration closes the gate at once (the
	// daemon reloads a changed configuration before answering).
	os.WriteFile(proj, []byte("harnesses = [\"claude\"]\n"), 0o644)
	var info proto.DaemonInfo
	if err := r.client(nil).Call(context.Background(), proto.MethodDaemonInfo, nil, &info); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := connect("c1", "x"); code != 255 || !strings.Contains(errb, "not granted") {
		t.Errorf("after the grant was removed: %d %q", code, errb)
	}
}
