package boxside

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/display"
)

func writeDisplayEnv(t *testing.T, root, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, EtcDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, DisplayEnvFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadDisplayEnv(t *testing.T) {
	root := t.TempDir()
	if vars, err := ReadDisplayEnv(root); vars != nil || err != nil {
		t.Errorf("absent: %v %v", vars, err)
	}
	writeDisplayEnv(t, root, string(DisplayEnv("127.0.0.1:1", "/run/devc/display.xauth")))
	vars, err := ReadDisplayEnv(root)
	if err != nil || strings.Join(vars, ";") != "DISPLAY=127.0.0.1:1;XAUTHORITY=/run/devc/display.xauth" {
		t.Errorf("vars %v %v", vars, err)
	}
	for _, bad := range []string{
		"",
		"DISPLAY=127.0.0.1:1\n",
		"DISPLAY=127.0.0.1:1\nXAUTHORITY=/run/devc/display.xauth\nLD_PRELOAD=/tmp/x.so\n",
		"DISPLAY=127.0.0.1:1\nDISPLAY=:2\nXAUTHORITY=/x\n",
		"DISPLAY=$(id):1\nXAUTHORITY=/x\n",
		"DISPLAY=127.0.0.1:1\nXAUTHORITY=relative\n",
		"DISPLAY=127.0.0.1:1\nXAUTHORITY=/a b\n",
		"export DISPLAY=127.0.0.1:1\nXAUTHORITY=/x\n",
		strings.Repeat("#", 2000),
	} {
		writeDisplayEnv(t, root, bad)
		if vars, err := ReadDisplayEnv(root); err == nil {
			t.Errorf("accepted %q: %v", bad, vars)
		}
	}
}

func TestWithDisplay(t *testing.T) {
	root := t.TempDir()
	env := []string{"PATH=/bin", "DISPLAY=:9"}
	if got := WithDisplay(root, env); strings.Join(got, ";") != "PATH=/bin;DISPLAY=:9" {
		t.Errorf("without the file: %v", got)
	}
	writeDisplayEnv(t, root, string(DisplayEnv("127.0.0.1:1", "/run/devc/display.xauth")))
	m := envMap(WithDisplay(root, env))
	if m["DISPLAY"] != "127.0.0.1:1" || m["XAUTHORITY"] != "/run/devc/display.xauth" || m["PATH"] != "/bin" {
		t.Errorf("with the file: %v", m)
	}
	// The launcher sets them too, and the manifest's environment still
	// comes last.
	l := fakeLaunch()
	lroot := launchRoot(t, l)
	writeDisplayEnv(t, lroot, string(DisplayEnv("127.0.0.1:1", "/run/devc/display.xauth")))
	var errb bytes.Buffer
	p, err := PrepareLaunch(&l, nil, LaunchEnv{Root: lroot, Environ: []string{"PATH=/usr/bin"}, Stderr: &errb,
		NewSession: func() string { return "s0000000000aaaaaaaaaa" }})
	if err != nil {
		t.Fatal(err)
	}
	if m := envMap(p.Env); m["DISPLAY"] != "127.0.0.1:1" || m["XAUTHORITY"] != "/run/devc/display.xauth" {
		t.Errorf("launch env: %v", m)
	}
	writeDisplayEnv(t, lroot, "DISPLAY=x\n")
	p, err = PrepareLaunch(&l, nil, LaunchEnv{Root: lroot, Environ: []string{"PATH=/usr/bin"}, Stderr: &errb,
		NewSession: func() string { return "s0000000000aaaaaaaaaa" }})
	if err != nil || envMap(p.Env)["DISPLAY"] != "" || !strings.Contains(errb.String(), "ignoring the desktop's display") {
		t.Errorf("bad file: %v %v %q", p, err, errb.String())
	}
}

// xServer accepts connections on 127.0.0.1 and accepts those whose setup
// carries cookie; it records the authorisation it saw.
func xServer(t *testing.T, cookie []byte) (port int, seen chan []byte) {
	t.Helper()
	// A port X can name: 6000 + display number (at most 9999).
	var ln net.Listener
	var err error
	for p := 6100 + int(time.Now().UnixNano()%5000); p < 15999; p += 7 {
		if ln, err = net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(p)); err == nil {
			break
		}
	}
	if ln == nil {
		t.Skipf("no free port for an X display: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	seen = make(chan []byte, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			var head [12]byte
			if _, err := io.ReadFull(c, head[:]); err != nil {
				c.Close()
				continue
			}
			n, d := int(binary.LittleEndian.Uint16(head[6:])), int(binary.LittleEndian.Uint16(head[8:]))
			pad := func(x int) int { return (4 - x%4) % 4 }
			rest := make([]byte, n+pad(n)+d+pad(d))
			_, _ = io.ReadFull(c, rest)
			data := rest[n+pad(n) : n+pad(n)+d]
			seen <- append([]byte(nil), data...)
			if bytes.Equal(data, cookie) && string(rest[:n]) == display.CookieName {
				// A minimal success: no vendor, no formats, no screens.
				body := make([]byte, 32)
				reply := append([]byte{1, 0, 11, 0, 0, 0, 8, 0}, body...)
				_, _ = c.Write(reply)
			} else {
				reason := "Authorization required"
				reply := []byte{0, byte(len(reason)), 11, 0, 0, 0, byte((len(reason) + pad(len(reason))) / 4), 0}
				reply = append(reply, reason...)
				reply = append(reply, make([]byte, pad(len(reason)))...)
				_, _ = c.Write(reply)
			}
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, seen
}

func TestXCheck(t *testing.T) {
	cookie := bytes.Repeat([]byte{0x5a}, 16)
	port, seen := xServer(t, cookie)
	number := port - 6000
	root := t.TempDir()
	xa, _ := display.CookieFile(cookie, number)
	xpath := filepath.Join(root, "run", "devc", "display.xauth")
	_ = os.MkdirAll(filepath.Dir(xpath), 0o755)
	if err := os.WriteFile(xpath, xa, 0o600); err != nil {
		t.Fatal(err)
	}
	disp := "127.0.0.1:" + strconv.Itoa(number)
	// From display.env alone.
	writeDisplayEnv(t, root, string(DisplayEnv(disp, xpath)))
	res, err := XCheck(root, nil, false, 5*time.Second)
	if err != nil || res.Display != disp {
		t.Fatalf("with the cookie: %+v %v", res, err)
	}
	if got := <-seen; !bytes.Equal(got, cookie) {
		t.Errorf("sent %x", got)
	}
	// Without it the display refuses.
	_, err = XCheck(root, nil, true, 5*time.Second)
	var re *display.RefusedError
	if !errors.As(err, &re) || re.Reason != "Authorization required" {
		t.Errorf("without the cookie: %v", err)
	}
	if got := <-seen; len(got) != 0 {
		t.Errorf("sent %x without auth", got)
	}
	// XAUTHORITY in the environment wins over the file.
	other := filepath.Join(root, "other")
	_ = os.WriteFile(other, nil, 0o600)
	if _, err := XCheck(root, []string{"XAUTHORITY=" + other}, false, 5*time.Second); !errors.As(err, &re) {
		t.Errorf("empty authority file: %v", err)
	}
	<-seen
	// No display at all.
	if _, err := XCheck(t.TempDir(), nil, false, time.Second); err == nil || !strings.Contains(err.Error(), "devc gui up") {
		t.Errorf("no display: %v", err)
	}
	if _, err := XCheck(t.TempDir(), []string{"DISPLAY=nonsense"}, false, time.Second); err == nil {
		t.Error("bad DISPLAY accepted")
	}
}
