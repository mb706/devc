package netpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Results of one canary probe.
const (
	ProbeConnected   = "connected"
	ProbeRefused     = "refused"
	ProbeUnreachable = "unreachable"
	ProbeTimeout     = "timeout"
	ProbeError       = "error"
)

// Probe is the outcome of one connection attempt made by `devc box
// net-canary` (or the sentinel check in box init).
type Probe struct {
	Addr   string `json:"addr"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// Classify turns the result of a TCP dial into a probe result.
func Classify(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return ProbeConnected
	case errors.Is(err, syscall.ECONNREFUSED):
		return ProbeRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return ProbeUnreachable
	case errors.As(err, &ne) && ne.Timeout():
		return ProbeTimeout
	}
	return ProbeError
}

// Dialer opens a TCP connection; tests replace it.
type Dialer func(ctx context.Context, addr string) (net.Conn, error)

// TCPDial is the real Dialer (IPv4 only).
func TCPDial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp4", addr)
}

// CheckSentinel connects to the sentinel and succeeds only on an
// immediate "connection refused", which devc's rules produce. A timeout,
// an unreachable destination or a connection means the rules are missing.
func CheckSentinel(ctx context.Context, dial Dialer, timeout time.Duration) error {
	if dial == nil {
		dial = TCPDial
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := netip.AddrPortFrom(SentinelAddr, SentinelPort).String()
	c, err := dial(ctx, addr)
	if c != nil {
		c.Close()
	}
	if r := Classify(err); r != ProbeRefused {
		detail := r
		if err != nil {
			detail += ": " + err.Error()
		}
		return fmt.Errorf("the network rules of level internet are not in place (sentinel %s: %s); start the box with 'devc up', never with a plain 'podman start'", addr, detail)
	}
	return nil
}

// RunCanary tries to connect to port on every address, concurrently, and
// reports each outcome in the order given.
func RunCanary(ctx context.Context, dial Dialer, addrs []netip.Addr, port int, timeout time.Duration) []Probe {
	if dial == nil {
		dial = TCPDial
	}
	out := make([]Probe, len(addrs))
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			addr := netip.AddrPortFrom(a, uint16(port)).String()
			c, err := dial(cctx, addr)
			if c != nil {
				c.Close()
			}
			p := Probe{Addr: addr, Result: Classify(err)}
			if err != nil {
				p.Detail = err.Error()
			}
			out[i] = p
		}()
	}
	wg.Wait()
	return out
}

// Canary is the host's side of the outside check: a TCP listener on all
// IPv4 addresses of the host that counts the connections it accepts while
// the box tries to reach the host.
type Canary struct {
	ln       net.Listener
	accepted atomic.Int64
	done     chan struct{}
}

// OpenCanary starts the listener on a free port.
func OpenCanary() (*Canary, error) {
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("opening the canary listener: %w", err)
	}
	c := &Canary{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.accepted.Add(1)
			conn.Close()
		}
	}()
	return c, nil
}

// Port is the listener's port.
func (c *Canary) Port() int { return c.ln.Addr().(*net.TCPAddr).Port }

// Close stops the listener and returns how many connections it accepted.
func (c *Canary) Close() int {
	c.ln.Close()
	<-c.done
	return int(c.accepted.Load())
}

// CanaryAddrs are the destinations the canary tries: the host's addresses
// and the engine's host alias, without duplicates.
func CanaryAddrs(host, alias []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range append(append([]netip.Addr(nil), host...), alias...) {
		a = a.Unmap()
		if !a.Is4() || a.IsLoopback() || a.IsUnspecified() {
			continue
		}
		dup := false
		for _, o := range out {
			dup = dup || o == a
		}
		if !dup {
			out = append(out, a)
		}
	}
	return out
}

// maxCanaryReport caps the box's report.
const maxCanaryReport = 64 << 10

// EvaluateCanary decides from the box's report and the number of
// connections the host's listener accepted whether the box reached the
// host. Either signal alone fails the check; a report that cannot be
// parsed fails it too, and so does one that leaves out a destination.
func EvaluateCanary(report []byte, want []netip.Addr, port, accepted int) error {
	if accepted > 0 {
		return fmt.Errorf("the box reached the host: the canary listener accepted %d connection(s)", accepted)
	}
	if len(report) > maxCanaryReport {
		return errors.New("the canary report is too large")
	}
	var probes []Probe
	dec := json.NewDecoder(bytes.NewReader(report))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&probes); err != nil {
		return fmt.Errorf("the canary report cannot be parsed: %w", err)
	}
	seen := map[string]bool{}
	var reached []string
	for _, p := range probes {
		seen[p.Addr] = true
		if p.Result == ProbeConnected {
			reached = append(reached, p.Addr)
		}
	}
	if len(reached) > 0 {
		return fmt.Errorf("the box reached %s", strings.Join(reached, ", "))
	}
	for _, a := range want {
		if addr := netip.AddrPortFrom(a, uint16(port)).String(); !seen[addr] {
			return fmt.Errorf("the canary report leaves out %s", addr)
		}
	}
	return nil
}
