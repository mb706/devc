package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
)

// ErrBoxGone ends a forward whose box stopped, was removed or restarted.
var ErrBoxGone = errors.New("the box is no longer running")

// Forwarder relays connections accepted on a host listener to a TCP port
// on the box's loopback interface. For each connection it runs
//
//	nsenter -t <box PID> -U -n --preserve-credentials <devc> box dial 127.0.0.1:<port>
//
// with the connection as standard input and output. The dial process is
// the host's devc binary, running in the box's user and network
// namespaces but in the host's mount namespace, so it needs nothing in
// the image, reaches services bound to the box's loopback, and works at
// every network level. It has the box user's identity and no
// capabilities.
type Forwarder struct {
	Runner execx.Runner
	Engine engine.Engine
	// Nsenter and Exe are absolute paths of nsenter and the devc binary.
	Nsenter string
	Exe     string
	// Container is the box's name, PID its main process on the host when
	// the forward started.
	Container string
	PID       int
	// Port is the port inside the box.
	Port int
	// Log receives one line per failed connection (nil discards).
	Log io.Writer
	// Poll is how often the box is checked (default 2 s).
	Poll time.Duration
}

// DialArgs are nsenter's arguments for one connection.
func (f *Forwarder) DialArgs() []string {
	return []string{"-t", strconv.Itoa(f.PID), "-U", "-n", "--preserve-credentials",
		f.Exe, "box", "dial", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(f.Port)).String()}
}

func (f *Forwarder) logf(format string, args ...any) {
	if f.Log != nil {
		fmt.Fprintf(f.Log, "devc forward: "+format+"\n", args...)
	}
}

// Serve accepts connections until ctx ends or the box goes away; it
// returns ErrBoxGone in the latter case. It closes ln.
func (f *Forwarder) Serve(ctx context.Context, ln net.Listener) error {
	if f.PID <= 0 || f.Port <= 0 || f.Port > 65535 || f.Nsenter == "" || f.Exe == "" {
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
// the forward started with.
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
		if !ci.State.Running || ci.State.Pid != f.PID {
			return ErrBoxGone
		}
	}
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
	})
	switch {
	case err != nil && ctx.Err() == nil:
		f.logf("connection to port %d in %s: %v", f.Port, f.Container, err)
	case err == nil && code != 0:
		f.logf("connection to port %d in %s ended with status %d", f.Port, f.Container, code)
	}
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
