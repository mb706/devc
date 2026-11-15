package hub

// The development mode of the hub (work order C12 section 5): `devc hub
// serve --dev-no-auth` serves the web app and the API without certificates
// or a login, backed by machines that exist only in this process
// (devmachine.go) with sessions in every state. It is for working on the
// web app and nothing else: it starts only with DEVC_DEV=1, listens only on
// a loopback address over plain HTTP, keeps the Host check (against DNS
// rebinding), and keeps its database in a temporary directory that is
// removed when it stops. No real machine can connect to it.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"
)

// DevEnv must be 1 for the development mode to start.
const DevEnv = "DEVC_DEV"

// devIdentity is the device every request of the development mode comes
// from.
var devIdentity = Identity{Role: RoleDevice, Name: "dev", Fingerprint: "dev"}

// newDev prepares a server in the development mode.
func newDev(o Options) (*Server, error) {
	if os.Getenv(DevEnv) != "1" {
		return nil, fmt.Errorf("--dev-no-auth serves without any authentication and is only for developing the web app: set %s=1 to use it", DevEnv)
	}
	host, _, err := net.SplitHostPort(o.Listen)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", o.Listen, err)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("--dev-no-auth listens only on a loopback address (127.0.0.1:<port>), not %q", o.Listen)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	dir, err := os.MkdirTemp("", "devc-hub-dev-")
	if err != nil {
		return nil, err
	}
	o.Dir, o.BehindProxy = dir, true
	l := Layout{Dir: dir}
	st, err := OpenStore(l.DB())
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	now := time.Now().UTC()
	login := Login{ID: TokenID(newStreamID()), Fingerprint: devIdentity.Fingerprint, Device: devIdentity.Name, Created: now, LastUsed: now}
	if err := st.AddLogin(login); err != nil {
		st.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	s := &Server{o: o, l: l, st: st, log: o.Log, instance: newStreamID()[:16], host: strings.ToLower(host), now: time.Now,
		cfg: &Config{Terminal: TerminalConfig{Control: true}}, devLogin: login}
	s.initState()
	s.buildMuxes()
	s.notifier = newNotifier(s, nil)
	o.Log.Warn("development mode: no certificates, no login, invented machines; never expose this", "listen", o.Listen)
	return s, nil
}

// devListen opens the development mode's listener and learns its port.
func (s *Server) devListen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.o.Listen)
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	s.port = port
	s.o.PublicName = net.JoinHostPort(s.host, port)
	s.publicURL = s.origin()
	return ln, nil
}

// devCleanup removes the development mode's temporary directory.
func (s *Server) devCleanup() {
	if !s.o.DevNoAuth {
		return
	}
	if !strings.Contains(s.o.Dir, "devc-hub-dev-") {
		s.log.Error("not removing an unexpected development directory", "dir", s.o.Dir)
		return
	}
	if err := os.RemoveAll(s.o.Dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("removing the development directory", "dir", s.o.Dir, "err", err)
	}
}
