package boxside

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/proto"
)

// fakeRelay is a RelayDialer whose host echoes in upper case.
type fakeRelay struct {
	available bool
	err       error
	service   string
	session   string
	got       chan string
}

type pipeConn struct{ net.Conn }

func (p pipeConn) CloseWrite() error { return p.Conn.(*net.UnixConn).CloseWrite() }

func (f *fakeRelay) Available() bool { return f.available }

func (f *fakeRelay) Open(service, session string) (RelayConn, error) {
	f.service, f.session = service, session
	if f.err != nil {
		return nil, f.err
	}
	a, b, err := unixPair()
	if err != nil {
		return nil, err
	}
	go func() {
		// The "host": read everything the client sends, answer, close.
		data, _ := io.ReadAll(b)
		f.got <- string(data)
		b.Write([]byte("PACK " + strings.ToUpper(string(data))))
		b.Close()
	}()
	return pipeConn{a}, nil
}

func unixPair() (*net.UnixConn, *net.UnixConn, error) {
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@devc-relay-test-" + randHex(), Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	defer ln.Close()
	ch := make(chan *net.UnixConn, 1)
	go func() {
		c, _ := ln.AcceptUnix()
		ch <- c
	}()
	a, err := net.DialUnix("unix", nil, ln.Addr().(*net.UnixAddr))
	if err != nil {
		return nil, nil, err
	}
	return a, <-ch, nil
}

func randHex() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func TestRemoteHelperConnect(t *testing.T) {
	git := func(string, []string, io.Reader, ...string) ([]byte, error) {
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/remotes/origin/main\n"), nil
	}
	t.Setenv("DEVC_SESSION", "s0000000000aaaaaaaaaa")
	// Connected: connect is offered and the stream relayed both ways.
	f := &fakeRelay{available: true, got: make(chan string, 1)}
	var out, errb bytes.Buffer
	in := "capabilities\nconnect git-receive-pack\n0000want\n"
	if code := RemoteHelperMain(nil, strings.NewReader(in), &out, &errb, git, f); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := <-f.got; got != "0000want\n" {
		t.Errorf("the host got %q", got)
	}
	if out.String() != "connect\nfetch\npush\n\n\nPACK 0000WANT\n" || f.service != "receive-pack" || f.session != "s0000000000aaaaaaaaaa" {
		t.Errorf("stdout %q, service %q, session %q", out.String(), f.service, f.session)
	}
	// The host went away in between: fall back to the stub.
	f = &fakeRelay{available: true, err: proto.Errorf(proto.CodeHostUnavailable, "host unavailable")}
	out.Reset()
	in = "capabilities\nconnect git-upload-pack\nlist\n\n"
	if code := RemoteHelperMain(nil, strings.NewReader(in), &out, &errb, git, f); code != 0 {
		t.Fatalf("fallback exit %d", code)
	}
	if out.String() != "connect\nfetch\npush\n\nfallback\naaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\n\n" {
		t.Errorf("fallback: %q", out.String())
	}
	// The agent itself cannot be reached any more: the same.
	f = &fakeRelay{available: true, err: errors.New("dial unix @devc-agent: connection refused")}
	out.Reset()
	if code := RemoteHelperMain(nil, strings.NewReader("connect git-upload-pack\n"), &out, &errb, git, f); code != 0 || out.String() != "fallback\n" {
		t.Errorf("dial failure: %d %q", code, out.String())
	}
	// The host refuses: git gets the message and a failure.
	f = &fakeRelay{available: true, err: proto.Errorf(proto.CodeRefused, "r/p has no relay yet\x1b[2J")}
	out.Reset()
	errb.Reset()
	if code := RemoteHelperMain(nil, strings.NewReader("connect git-upload-pack\n"), &out, &errb, git, f); code != 1 ||
		!strings.Contains(errb.String(), "has no relay yet") || strings.Contains(errb.String(), "\x1b") {
		t.Errorf("refusal: %d %q", code, errb.String())
	}
	// Other services are not offered.
	errb.Reset()
	if code := RemoteHelperMain(nil, strings.NewReader("connect git-upload-archive\n"), &out, &errb, git, f); code != 1 {
		t.Errorf("upload-archive: %d", code)
	}
	// Not connected: the stub, without connect.
	f = &fakeRelay{available: false}
	out.Reset()
	if code := RemoteHelperMain(nil, strings.NewReader("capabilities\n\n"), &out, &errb, git, f); code != 0 || out.String() != "fetch\npush\n\n" {
		t.Errorf("stub: %d %q", code, out.String())
	}
	if f.service != "" {
		t.Error("the stub opened a stream")
	}
	// An invalid session ID is not passed on.
	t.Setenv("DEVC_SESSION", "../../x")
	f = &fakeRelay{available: true, got: make(chan string, 1)}
	RemoteHelperMain(nil, strings.NewReader("connect git-upload-pack\n"), &out, &errb, git, f)
	<-f.got
	if f.session != "" {
		t.Errorf("session %q", f.session)
	}
}

func TestDaemonSecrets(t *testing.T) {
	l := fakeLaunch()
	l.Delivery = DeliveryDaemon
	root := launchRoot(t, l)
	// Files in the secrets directory are ignored in daemon mode.
	os.WriteFile(filepath.Join(root, SecretsDir, "fake-token"), []byte("from-file"), 0o400)
	asked := []string{}
	fetch := func(name string) (string, error) {
		asked = append(asked, name)
		switch name {
		case "fake-token":
			return "tok", nil
		case "GRANT":
			return "", proto.Errorf(proto.CodeNotFound, "the secret GRANT is not set")
		}
		return "", proto.Errorf(proto.CodeRefused, "not granted")
	}
	var errb bytes.Buffer
	e := LaunchEnv{Root: root, Environ: []string{"PATH=/usr/bin"}, Stderr: &errb, NewSession: func() string { return "s0000000000aaaaaaaaaa" }, FetchSecret: fetch}
	p, err := PrepareLaunch(&l, nil, e)
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(p.Env)
	if m["FAKE_TOKEN"] != "tok" || m["GRANT"] != "" || strings.Join(asked, ",") != "fake-token,GRANT" || !strings.Contains(errb.String(), "GRANT is not set") {
		t.Errorf("env %v asked %v stderr %q", m, asked, errb.String())
	}
	// Daemon unreachable: start without, say how to fix it, ask once.
	asked, errb = nil, bytes.Buffer{}
	e.Stderr = &errb
	e.FetchSecret = func(name string) (string, error) {
		asked = append(asked, name)
		return "", proto.Errorf(proto.CodeHostUnavailable, "host unavailable")
	}
	p, err = PrepareLaunch(&l, nil, e)
	if err != nil {
		t.Fatal(err)
	}
	if m := envMap(p.Env); m["FAKE_TOKEN"] != "" || len(asked) != 1 || !strings.Contains(errb.String(), "not reachable") || !strings.Contains(errb.String(), "secrets.delivery") {
		t.Errorf("unreachable: env %v asked %v stderr %q", m, asked, errb.String())
	}
	if err := (&Launch{Schema: LaunchSchema, ID: "x", Bin: "x", Exe: "/x", Home: LaunchHome{Dir: "/home/dev/.x"}, Delivery: "carrier-pigeon"}).Validate(); err == nil {
		t.Error("accepted an unknown delivery")
	}
}
