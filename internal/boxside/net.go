package boxside

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/netpolicy"
)

// Dial connects to addr (an IP address and port) and relays standard input
// to it and its answers to standard output until both directions end. It
// is what the host's forwarder runs in the box's user and network
// namespaces (`devc box dial`), with the forwarded connection's socket as
// stdin and stdout; it runs in the host's mount namespace and reads no
// file.
func Dial(ctx context.Context, addr string, stdin, stdout *os.File) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || ap.Port() == 0 {
		return fmt.Errorf("dial: %q is not an IP address and port", addr)
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := d.DialContext(dctx, "tcp", ap.String())
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	tc := conn.(*net.TCPConn)
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(tc, stdin)
		_ = tc.CloseWrite()
		done <- err
	}()
	go func() {
		_, err := io.Copy(stdout, tc)
		shutdownWrite(stdout)
		done <- err
	}()
	var errs []error
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// shutdownWrite ends the write side of f: a socket is shut down for
// writing (the peer sees the end of the stream while it can still send),
// anything else is closed.
func shutdownWrite(f *os.File) {
	rc, err := f.SyscallConn()
	if err == nil {
		var serr error
		if cerr := rc.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_WR) }); cerr == nil && serr == nil {
			return
		}
	}
	_ = f.Close()
}

// NetCanary is `devc box net-canary`: it tries to connect to port on each
// address and writes the outcomes as JSON. It reports whether any
// connection succeeded. The host runs it after starting a box of level
// internet, against a listener of its own; the host also counts what its
// listener accepted, so a box cannot pass by lying.
func NetCanary(ctx context.Context, addrs []string, port int, timeout time.Duration, dial netpolicy.Dialer, out io.Writer) (bool, error) {
	if port < 1 || port > 65535 {
		return false, fmt.Errorf("net-canary: bad port %d", port)
	}
	var as []netip.Addr
	for _, s := range addrs {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Unmap().Is4() {
			return false, fmt.Errorf("net-canary: %q is not an IPv4 address", s)
		}
		as = append(as, a.Unmap())
	}
	probes := netpolicy.RunCanary(ctx, dial, as, port, timeout)
	if probes == nil {
		probes = []netpolicy.Probe{}
	}
	b, err := json.Marshal(probes)
	if err != nil {
		return false, err
	}
	if _, err := out.Write(append(b, '\n')); err != nil {
		return false, err
	}
	for _, p := range probes {
		if p.Result == netpolicy.ProbeConnected {
			return true, nil
		}
	}
	return false, nil
}

// BindingFile is where box init finds the host's network binding record.
const BindingFile = "/etc/devc/" + netpolicy.BindingFile

// NetworkCheck returns the check box init runs before it reports ready, for
// level internet (or when box.json cannot be read, so the level is
// unknown): the binding record the host wrote into the read-only
// /etc/devc must name this box's network namespace, PID 1 and boot, which
// only a start through devc gives (finding B1); then the sentinel must
// answer "connection refused", which devc's rules do. Other levels need no
// check. binding is the record's path and proc the box's /proc.
func NetworkCheck(info *Info, dial netpolicy.Dialer, binding string, proc netpolicy.ProcFS) func(context.Context) error {
	if info != nil && info.Network != netpolicy.Internet {
		return nil
	}
	return func(ctx context.Context) error {
		if err := netpolicy.CheckBinding(binding, proc); err != nil {
			return err
		}
		return netpolicy.CheckSentinel(ctx, dial, 5*time.Second)
	}
}
