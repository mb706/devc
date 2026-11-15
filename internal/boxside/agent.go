package boxside

// The box agent (work order C08 section 3): part of `devc box init`, started
// after the readiness marker. It listens on /run/devc/agent.sock for the
// host's daemon and on the abstract socket @devc-agent for clients inside
// the box, runs yamux over the host connection, relays each in-box client
// on a new stream and serves the streams the host opens.
//
// The direction is fixed: the box listens, the host connects. Abstract
// sockets are scoped to the network namespace, so only processes of this
// box reach @devc-agent. A connection on agent.sock is accepted as the
// host's only when its peer lies outside the box's PID namespace (the
// kernel then reports PID 0), so no box process can stand in for the host.
// Everything here only logs; nothing can stop box init.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/proto"
)

// Agent sockets and log.
const (
	AgentSocketName = "agent.sock"
	AgentSocket     = RunDir + "/" + AgentSocketName
	AgentLog        = RunDir + "/agent.log"
	// AgentAbstract is the agent's socket for in-box clients ("@" is the
	// abstract namespace).
	AgentAbstract = "@devc-agent"
)

// maxAgentLog bounds agent.log; it starts over when full.
const maxAgentLog = 1 << 20

// AgentOptions configure RunAgent.
type AgentOptions struct {
	// Socket is the path the host connects to; Local the in-box socket.
	Socket string
	Local  string
	Log    io.Writer
	// Hello describes the box for box.hello; Sessions lists its live
	// sessions for box.sessions.
	Hello    func() (proto.HelloReply, error)
	Sessions func() ([]Live, error)
	// Input carries out the daemon's input to session panes (nil: refused).
	Input *Input
	// AcceptHost decides whether a connection on Socket is the host's
	// (default: its peer is outside the box's PID namespace).
	AcceptHost func(*unix.Ucred) error
	// MaxClients bounds concurrent in-box clients (default 32).
	MaxClients int
	// Proxy is the loopback address on which the agent relays connections
	// to the host's allowlist proxy (level allowlist; empty: none).
	Proxy string
	// MaxRelays bounds relayed TCP connections at once
	// (DefaultMaxRelays).
	MaxRelays int
	// Retry is the pause after a listener failed (default 5 s).
	Retry time.Duration
}

// DefaultAgentOptions are the agent's settings in a box.
func DefaultAgentOptions(log io.Writer, hello func() (proto.HelloReply, error), sessions func() ([]Live, error)) AgentOptions {
	return AgentOptions{Socket: AgentSocket, Local: AgentAbstract, Log: log, Hello: hello, Sessions: sessions}
}

// Agent is a running agent.
type Agent struct {
	o  AgentOptions
	mu sync.Mutex
	// host is the current host session (nil without one).
	host *yamux.Session
	// relays bounds the relayed TCP connections; limiter the relay streams
	// per window (netrelay.go).
	relays  chan struct{}
	limiter *relayLimiter
	// listeners are the host-exception ports (box.listen).
	lmu       sync.Mutex
	listeners map[int]net.Listener
}

// HostPeer accepts a peer that the kernel reports with PID 0: a process
// outside this PID namespace, which in a box means the host.
func HostPeer(cred *unix.Ucred) error {
	if cred.Pid != 0 {
		return fmt.Errorf("the peer is process %d of this box, not the host", cred.Pid)
	}
	return nil
}

// RunAgent runs the agent until ctx ends. It never returns an error and
// never panics: failures are logged and retried.
func RunAgent(ctx context.Context, o AgentOptions) {
	if o.MaxClients <= 0 {
		o.MaxClients = 32
	}
	if o.Retry <= 0 {
		o.Retry = 5 * time.Second
	}
	if o.AcceptHost == nil {
		o.AcceptHost = HostPeer
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.MaxRelays <= 0 {
		o.MaxRelays = DefaultMaxRelays
	}
	a := &Agent{o: o, relays: make(chan struct{}, o.MaxRelays), limiter: &relayLimiter{now: time.Now, max: relayOpens},
		listeners: map[int]net.Listener{}}
	var wg sync.WaitGroup
	wg.Add(2)
	a.goSafe("host listener", func() { defer wg.Done(); a.hostLoop(ctx) })
	a.goSafe("local listener", func() { defer wg.Done(); a.localLoop(ctx) })
	if o.Proxy != "" {
		wg.Add(1)
		a.goSafe("proxy listener", func() { defer wg.Done(); a.proxyLoop(ctx) })
	}
	wg.Wait()
	a.closeListeners()
	a.setHost(nil)
}

func (a *Agent) logf(format string, args ...any) {
	fmt.Fprintf(a.o.Log, "%s devc agent: %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// goSafe runs fn in a goroutine that logs a panic instead of ending the
// process (box init).
func (a *Agent) goSafe(what string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.logf("%s: panic: %v", what, r)
			}
		}()
		fn()
	}()
}

func (a *Agent) sleep(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(a.o.Retry):
		return true
	}
}

// Connected reports whether a host is connected.
func (a *Agent) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.host != nil && !a.host.IsClosed()
}

func (a *Agent) current() *yamux.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.host != nil && a.host.IsClosed() {
		a.host = nil
	}
	return a.host
}

// setHost makes s the host session; the previous one is closed.
func (a *Agent) setHost(s *yamux.Session) {
	a.mu.Lock()
	old := a.host
	a.host = s
	a.mu.Unlock()
	if old != nil && old != s {
		_ = old.Close()
	}
}

func (a *Agent) clearHost(s *yamux.Session) {
	a.mu.Lock()
	if a.host == s {
		a.host = nil
	}
	a.mu.Unlock()
}

func (a *Agent) yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.AcceptBacklog = 64
	cfg.StreamOpenTimeout = 15 * time.Second
	// A relayed client half-closes its stream when it has sent everything
	// (git push closes its side after the pack); the host may still be
	// working for a long time (receive-pack's checks and hooks), so a
	// half-closed stream is not torn down early.
	cfg.StreamCloseTimeout = 35 * time.Minute
	cfg.LogOutput = a.o.Log
	return cfg
}

// hostLoop listens on the host socket, re-creating it after failures.
func (a *Agent) hostLoop(ctx context.Context) {
	for ctx.Err() == nil {
		ln, err := listenSocket(a.o.Socket)
		if err != nil {
			a.logf("listening on %s: %v", a.o.Socket, err)
			if !a.sleep(ctx) {
				return
			}
			continue
		}
		stop := context.AfterFunc(ctx, func() { ln.Close() })
		a.acceptHost(ctx, ln)
		stop()
		ln.Close()
		if !a.sleep(ctx) {
			return
		}
	}
}

// listenSocket listens on path after removing what is there: the runtime
// directory belongs to the box, so an old socket (or anything planted)
// is in the way only of the box itself.
func listenSocket(path string) (*net.UnixListener, error) {
	if err := syscall.Unlink(path); err != nil && !errors.Is(err, syscall.ENOENT) {
		return nil, fmt.Errorf("removing the old socket: %w", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(false)
	return ln, nil
}

func (a *Agent) acceptHost(ctx context.Context, ln *net.UnixListener) {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() == nil {
				a.logf("accepting on %s: %v", a.o.Socket, err)
			}
			return
		}
		cred, err := peerCred(c)
		if err == nil {
			err = a.o.AcceptHost(cred)
		}
		if err != nil {
			a.logf("refused a connection on %s: %v", a.o.Socket, err)
			c.Close()
			continue
		}
		s, err := yamux.Server(c, a.yamuxConfig())
		if err != nil {
			a.logf("starting the channel: %v", err)
			c.Close()
			continue
		}
		a.logf("host connected")
		a.setHost(s)
		a.goSafe("host session", func() { a.serveHost(ctx, s) })
	}
}

// peerCred returns the credentials of a Unix socket's peer.
func peerCred(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, cerr
}

// serveHost serves the streams the host opens until its session ends.
func (a *Agent) serveHost(ctx context.Context, s *yamux.Session) {
	defer a.clearHost(s)
	defer s.Close()
	handlers := map[string]proto.Handler{
		proto.MethodBoxHello:    a.handleHello,
		proto.MethodBoxSessions: a.handleSessions,
		proto.MethodBoxClients:  a.inputHandler(proto.MethodBoxClients),
		proto.MethodBoxPaste:    a.inputHandler(proto.MethodBoxPaste),
		proto.MethodBoxKeys:     a.inputHandler(proto.MethodBoxKeys),
		proto.MethodBoxListen:   a.handleListen(ctx),
	}
	for {
		st, err := s.AcceptStreamWithContext(ctx)
		if err != nil {
			if ctx.Err() == nil {
				a.logf("host disconnected: %v", err)
			}
			return
		}
		conn := proto.NewConn(st, proto.Options{Handlers: handlers, Context: ctx, IdleTimeout: time.Hour})
		_ = conn
	}
}

func (a *Agent) handleHello(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var h proto.Hello
	if err := proto.DecodeParams(params, &h); err != nil {
		return nil, err
	}
	if a.o.Hello == nil {
		return nil, proto.Errorf(proto.CodeInternal, "the box description is not available")
	}
	r, err := a.o.Hello()
	if err != nil {
		a.logf("box.hello: %v", err)
		return nil, proto.Errorf(proto.CodeInternal, "the box description is not available")
	}
	if h.Protocol != proto.ChannelVersion {
		a.logf("the host speaks channel protocol %d, this helper %d", h.Protocol, proto.ChannelVersion)
	}
	r.Protocol = proto.ChannelVersion
	return r, nil
}

func (a *Agent) handleSessions(context.Context, *proto.Conn, json.RawMessage) (any, error) {
	if a.o.Sessions == nil {
		return nil, proto.Errorf(proto.CodeInternal, "session listing is not available")
	}
	live, err := a.o.Sessions()
	if err != nil {
		a.logf("box.sessions: %v", err)
		return nil, proto.Errorf(proto.CodeInternal, "listing the sessions failed")
	}
	if live == nil {
		live = []Live{}
	}
	b, err := json.Marshal(live)
	if err != nil {
		return nil, err
	}
	return proto.LiveReply{Sessions: b}, nil
}

// inputHandler serves one of the input methods (work order C09).
func (a *Agent) inputHandler(method string) proto.Handler {
	return func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
		var p proto.BoxInputParams
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		in := a.o.Input
		if in == nil {
			return nil, proto.Errorf(proto.CodeRefused, "input is not available in this box")
		}
		var res any = struct{}{}
		var err error
		switch method {
		case proto.MethodBoxClients:
			res, err = in.Clients(p.Session)
		case proto.MethodBoxPaste:
			err = in.Paste(p.Session, p.Text, p.Submit)
		case proto.MethodBoxKeys:
			err = in.Keys(p.Session, p.Keys)
		}
		switch {
		case errors.Is(err, ErrNoPane):
			return nil, proto.Errorf(proto.CodeNotFound, "%s", ErrNoPane.Error())
		case err != nil:
			a.logf("%s: %v", method, err)
			return nil, proto.Errorf(proto.CodeInternal, "%s", proto.Printable(proto.Truncate(err.Error(), 300)))
		}
		return res, nil
	}
}

// localLoop listens for in-box clients.
func (a *Agent) localLoop(ctx context.Context) {
	sem := make(chan struct{}, a.o.MaxClients)
	for ctx.Err() == nil {
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: a.o.Local, Net: "unix"})
		if err != nil {
			a.logf("listening on %s: %v", a.o.Local, err)
			if !a.sleep(ctx) {
				return
			}
			continue
		}
		stop := context.AfterFunc(ctx, func() { ln.Close() })
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				if ctx.Err() == nil {
					a.logf("accepting on %s: %v", a.o.Local, err)
				}
				break
			}
			select {
			case sem <- struct{}{}:
				a.goSafe("client", func() {
					defer func() { <-sem }()
					a.relay(c)
				})
			default:
				c.Close() // too many clients at once
			}
		}
		stop()
		ln.Close()
		if !a.sleep(ctx) {
			return
		}
	}
}

// relay forwards one in-box client to the host on a new stream. Without a
// host, a request gets an immediate "host unavailable" error.
func (a *Agent) relay(c *net.UnixConn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReaderSize(c, 64<<10)
	line, err := proto.ReadLine(r, proto.MaxMessage)
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	var st *yamux.Stream
	if s := a.current(); s != nil {
		st, err = s.OpenStream()
	}
	if st == nil || err != nil {
		if m, kind, perr := proto.ParseMessage(line); perr == nil && kind == proto.KindRequest {
			b, _ := json.Marshal(&proto.RPCMessage{JSONRPC: "2.0", ID: m.ID, Error: &proto.Error{Code: proto.CodeHostUnavailable, Message: "host unavailable"}})
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, _ = c.Write(append(b, '\n'))
		}
		return
	}
	defer st.Close()
	if _, err := st.Write(append(line, '\n')); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(st, r)
		_ = st.Close() // the client is done writing
	}()
	_, _ = io.Copy(c, st)
	c.Close()
	<-done
}

// cappedLog appends to a file of the box's runtime directory and starts it
// over when it would exceed max bytes. A planted symlink, FIFO or other
// non-regular file makes it discard instead.
type cappedLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	n    int64
	max  int64
	bad  bool
}

// OpenAgentLog returns the writer for agent.log below run.
func OpenAgentLog(run string) io.Writer {
	return &cappedLog{path: filepath.Join(run, filepath.Base(AgentLog)), max: maxAgentLog}
}

func (l *cappedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bad {
		return len(p), nil
	}
	if l.f == nil {
		f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0o600)
		if err != nil {
			l.bad = true
			return len(p), nil
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			f.Close()
			l.bad = true
			return len(p), nil
		}
		l.f, l.n = f, fi.Size()
	}
	if l.n+int64(len(p)) > l.max {
		_ = l.f.Truncate(0)
		l.n = 0
	}
	n, _ := l.f.Write(p)
	l.n += int64(n)
	return len(p), nil
}
