// This file relays connections into a box's network namespace for `devc
// forward` and the daemon's router: the box's namespaces are pinned once,
// and each connection gets a dial process of the host's devc binary in
// them.

package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
)

// ErrBoxGone ends a forward whose box stopped, was removed or restarted.
var ErrBoxGone = errors.New("the box is no longer running")

// Forwarder relays connections accepted on a host listener to a TCP port
// on the box's loopback interface. For each connection it runs
//
//	nsenter --user=/proc/self/fd/3 --net=/proc/self/fd/4 --preserve-credentials <devc> box dial 127.0.0.1:<port>
//
// with the connection as standard input and output and the box's pinned
// user and network namespaces (Pin) as descriptors 3 and 4. The dial
// process is the host's devc binary, running in the box's user and network
// namespaces but in the host's mount namespace, so it needs nothing in the
// image, reaches services bound to the box's loopback, and works at every
// network level. It has the box user's identity and no capabilities.
// Because the namespaces are held open from the start, a connection can
// only ever reach this box's namespaces, never those of a process that
// later reuses its PID (finding B5).
type Forwarder struct {
	Runner execx.Runner
	Engine engine.Engine
	// Nsenter and Exe are absolute paths of nsenter and the devc binary.
	Nsenter string
	Exe     string
	// Container is the box's name; Box its pinned main process.
	Container string
	Box       *Pinned
	// Port is the port inside the box.
	Port int
	// Log receives one line per failed connection (nil discards).
	Log io.Writer
	// Poll is how often the box is checked (default 2 s).
	Poll time.Duration
}

// Descriptors of the pinned namespaces in each dial process.
const (
	dialUserFD = 3
	dialNetFD  = 4
)

// DialArgs are nsenter's arguments for one connection.
func (f *Forwarder) DialArgs() []string {
	return []string{"--user=/proc/self/fd/" + strconv.Itoa(dialUserFD), "--net=/proc/self/fd/" + strconv.Itoa(dialNetFD), "--preserve-credentials",
		f.Exe, "box", "dial", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(f.Port)).String()}
}

func (f *Forwarder) logf(format string, args ...any) {
	if f.Log != nil {
		fmt.Fprintf(f.Log, "devc forward: "+format+"\n", args...)
	}
}

// Serve accepts connections until ctx ends or the box's generation ends
// (it stops, is removed or restarts); it returns ErrBoxGone in the latter
// case. It closes ln.
func (f *Forwarder) Serve(ctx context.Context, ln net.Listener) error {
	if f.Box == nil || f.Box.PID <= 0 || f.Box.UserNS == nil || f.Box.NetNS == nil || f.Port <= 0 || f.Port > 65535 || f.Nsenter == "" || f.Exe == "" {
		ln.Close()
		return errors.New("forward: incomplete settings")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchErr := make(chan error, 1)
	go func() {
		err := f.watch(ctx)
		watchErr <- err
		cancel()
	}()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			break
		}
		// A connection accepted as the box's generation ends is dropped.
		if ctx.Err() != nil {
			conn.Close()
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.relay(ctx, conn)
		}()
	}
	cancel()
	wg.Wait()
	if err := <-watchErr; errors.Is(err, ErrBoxGone) {
		return err
	} else if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// watch returns ErrBoxGone when the box is no longer the running container
// generation the forward started with.
func (f *Forwarder) watch(ctx context.Context) error {
	poll := f.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
		ci, err := f.Engine.Inspect(ctx, f.Container)
		switch {
		case errors.Is(err, engine.ErrNoSuchContainer):
			return ErrBoxGone
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if failures++; failures >= 5 {
				return fmt.Errorf("cannot query the box: %w", err)
			}
			continue
		}
		failures = 0
		if !f.Box.Same(&ci) {
			return ErrBoxGone
		}
	}
}

// Pinned is a running box's main process with its user and network
// namespaces held open.
type Pinned struct {
	PID     int
	Started time.Time
	UserNS  *os.File
	NetNS   *os.File
}

// Same reports whether ci is still the generation that was pinned.
func (p *Pinned) Same(ci *engine.ContainerInfo) bool {
	return ci.State.Running && ci.State.Pid == p.PID && ci.State.Started.Equal(p.Started)
}

// Close releases the namespaces.
func (p *Pinned) Close() {
	if p.UserNS != nil {
		p.UserNS.Close()
	}
	if p.NetNS != nil {
		p.NetNS.Close()
	}
}

// Pin opens the user and network namespaces of a running box's main process
// and proves that they are the box's: the engine reports the same main
// process and start time before and after, and the process with that PID
// has the same start time (fs: the host's /proc) before and after the
// namespaces were opened, so the PID was not reused in between.
func Pin(ctx context.Context, eng engine.Engine, container string, fs ProcFS) (*Pinned, error) {
	gone := fmt.Errorf("%s is not running", container)
	ci, err := eng.Inspect(ctx, container)
	if err != nil {
		return nil, err
	}
	if !ci.State.Running || ci.State.Pid <= 0 {
		return nil, gone
	}
	p := &Pinned{PID: ci.State.Pid, Started: ci.State.Started}
	pid := strconv.Itoa(p.PID)
	before, err := fs.StartTime(pid)
	if err != nil {
		return nil, fmt.Errorf("reading the box's main process (pid %d): %w", p.PID, err)
	}
	open := func(ns string) (*os.File, error) {
		f, err := os.OpenFile(filepath.Join(fs.root(), pid, "ns", ns), os.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("opening the %s namespace of the box's main process (pid %d): %w", ns, p.PID, err)
		}
		return f, nil
	}
	if p.UserNS, err = open("user"); err != nil {
		return nil, err
	}
	if p.NetNS, err = open("net"); err != nil {
		p.Close()
		return nil, err
	}
	after, err := fs.StartTime(pid)
	if err != nil || after != before {
		p.Close()
		return nil, fmt.Errorf("%s stopped or restarted while the forward was set up: try again", container)
	}
	ci2, err := eng.Inspect(ctx, container)
	if err != nil || !p.Same(&ci2) {
		p.Close()
		return nil, fmt.Errorf("%s stopped or restarted while the forward was set up: try again", container)
	}
	return p, nil
}

// relay hands one connection to a dial process in the box. The socket
// itself becomes the process's standard input and output, so bytes flow
// without a copy on the host and half-closes reach the other side.
func (f *Forwarder) relay(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		f.logf("unexpected connection type %T", conn)
		return
	}
	file, err := tc.File()
	if err != nil {
		f.logf("%v", err)
		return
	}
	defer file.Close()
	conn.Close()
	stderr := f.Log
	if stderr == nil {
		stderr = io.Discard
	}
	code, err := f.Runner.Interactive(ctx, execx.Cmd{
		Name: f.Nsenter, Args: f.DialArgs(), Dir: "/",
		// DEVC_BOX keeps the dial process from loading the host's devc
		// configuration; it reads nothing from the box either.
		Env:   []string{"DEVC_BOX=1"},
		Stdin: file, Stdout: file, Stderr: stderr,
		// Descriptors 3 and 4 (dialUserFD, dialNetFD).
		ExtraFiles: []*os.File{f.Box.UserNS, f.Box.NetNS},
	})
	switch {
	case err != nil && ctx.Err() == nil:
		f.logf("connection to port %d in %s: %v", f.Port, f.Container, err)
	case err == nil && code != 0:
		f.logf("connection to port %d in %s ended with status %d", f.Port, f.Container, code)
	}
}

// Dial opens one connection to the forwarded port in the box, for callers
// that relay on the host (the daemon's local-URL router): a socket pair
// whose one end becomes the dial process's standard input and output, as
// relay does with an accepted connection. The process runs until the
// connection is closed or ctx ends.
func (f *Forwarder) Dial(ctx context.Context) (net.Conn, error) {
	if f.Box == nil || f.Box.PID <= 0 || f.Box.UserNS == nil || f.Box.NetNS == nil || f.Port <= 0 || f.Port > 65535 || f.Nsenter == "" || f.Exe == "" {
		return nil, errors.New("dial: incomplete settings")
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	local := os.NewFile(uintptr(fds[0]), "dial-local")
	remote := os.NewFile(uintptr(fds[1]), "dial-remote")
	conn, err := net.FileConn(local)
	local.Close()
	if err != nil {
		remote.Close()
		return nil, fmt.Errorf("dial: %w", err)
	}
	pctx, cancel := context.WithCancel(ctx)
	stderr := f.Log
	if stderr == nil {
		stderr = io.Discard
	}
	go func() {
		defer cancel()
		defer remote.Close()
		code, err := f.Runner.Interactive(pctx, execx.Cmd{
			Name: f.Nsenter, Args: f.DialArgs(), Dir: "/",
			Env:   []string{"DEVC_BOX=1"},
			Stdin: remote, Stdout: remote, Stderr: stderr,
			ExtraFiles: []*os.File{f.Box.UserNS, f.Box.NetNS},
		})
		switch {
		case err != nil && pctx.Err() == nil:
			f.logf("connection to port %d in %s: %v", f.Port, f.Container, err)
		case err == nil && code != 0:
			f.logf("connection to port %d in %s ended with status %d", f.Port, f.Container, code)
		}
	}()
	return &dialConn{Conn: conn, cancel: cancel}, nil
}

// dialConn ends its dial process when it is closed.
type dialConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *dialConn) Close() error {
	err := c.Conn.Close()
	c.cancel()
	return err
}

// CloseWrite half-closes the connection.
func (c *dialConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// Listen opens the host side of a forward on bind (an IP address) and
// port. When the port is taken and was not given explicitly, a free port
// is chosen instead.
func Listen(bind string, port int, explicit bool) (net.Listener, error) {
	ip, err := netip.ParseAddr(bind)
	if err != nil {
		return nil, fmt.Errorf("--bind %q is not an IP address", bind)
	}
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range", port)
	}
	network := "tcp4"
	if ip.Unmap().Is6() {
		network = "tcp6"
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	ln, err := net.Listen(network, addr)
	if err != nil && !explicit && errors.Is(err, syscall.EADDRINUSE) {
		return net.Listen(network, net.JoinHostPort(ip.String(), "0"))
	}
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	return ln, nil
}

// IsLoopback reports whether bind is a loopback address.
func IsLoopback(bind string) bool {
	ip, err := netip.ParseAddr(bind)
	return err == nil && ip.Unmap().IsLoopback()
}
