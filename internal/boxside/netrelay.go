package boxside

// The box agent's network relays (work order C13). At level allowlist the
// box has no network interface besides loopback; the agent listens on
// 127.0.0.1:3128 and relays every connection, unparsed, on a host.proxy
// stream to the daemon, which speaks the proxy protocol and decides
// everything. For host exceptions the daemon tells the agent (box.listen)
// on which loopback ports to listen; each connection there becomes a
// host.dial stream, which the daemon connects to the same port on the
// host's loopback only while the exception is in force. The agent holds no
// policy: whatever a box process does to it, the host still decides.
//
// Without a connected host a connection is closed at once, so the box has
// no network. The agent bounds the relays it runs at once and the streams
// it opens per window, below the daemon's channel budgets (muxguard), so
// that ordinary use never makes the daemon close the channel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// Relay limits of the agent.
const (
	// DefaultMaxRelays bounds the relayed connections at once (proxy and
	// host exceptions together); the daemon accepts as many.
	DefaultMaxRelays = 64
	// relayWindow and relayOpens bound the relay streams opened per
	// window: half of the daemon's budget for new streams, which leaves
	// the rest to in-box clients and hints.
	relayWindow = 10 * time.Second
	relayOpens  = 512
	// relayDrain is how long a client may keep sending after the host
	// has finished its direction.
	relayDrain = time.Minute
)

// relayLimiter counts the relay streams opened in the current window.
type relayLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	since time.Time
	n     int
	max   int
}

func (l *relayLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.since) >= relayWindow || now.Before(l.since) {
		l.since, l.n = now, 0
	}
	if l.n >= l.max {
		return false
	}
	l.n++
	return true
}

// relayTCP relays one accepted connection on a new stream to the host,
// announced by a request line for method. Without a host, a free relay
// place or room in the window, the connection is closed at once.
func (a *Agent) relayTCP(c net.Conn, method string, params any) {
	defer c.Close()
	select {
	case a.relays <- struct{}{}:
		defer func() { <-a.relays }()
	default:
		a.logf("%s: too many relayed connections at once; closed one", method)
		return
	}
	if !a.limiter.allow() {
		a.logf("%s: too many new connections in %s; closed one", method, relayWindow)
		return
	}
	s := a.current()
	if s == nil {
		return
	}
	st, err := s.OpenStream()
	if err != nil {
		return
	}
	defer st.Close()
	p, _ := json.Marshal(params)
	line, _ := json.Marshal(&proto.RPCMessage{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: method, Params: p})
	_ = st.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := st.Write(append(line, '\n')); err != nil {
		return
	}
	_ = st.SetWriteDeadline(time.Time{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(st, c)
		_ = st.Close() // the client is done sending (yamux: a half-close)
	}()
	_, _ = io.Copy(c, st)
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	// The host is done; the client gets a short while to finish.
	_ = c.SetReadDeadline(time.Now().Add(relayDrain))
	<-done
}

// proxyLoop listens on the proxy address and relays every connection on a
// host.proxy stream.
func (a *Agent) proxyLoop(ctx context.Context) {
	for ctx.Err() == nil {
		ln, err := net.Listen("tcp4", a.o.Proxy)
		if err != nil {
			a.logf("listening on %s for the proxy: %v", a.o.Proxy, err)
			if !a.sleep(ctx) {
				return
			}
			continue
		}
		stop := context.AfterFunc(ctx, func() { ln.Close() })
		a.acceptRelays(ctx, ln, proto.MethodHostProxy, proto.ProxyParams{})
		stop()
		ln.Close()
		if !a.sleep(ctx) {
			return
		}
	}
}

// acceptRelays accepts on ln until it closes.
func (a *Agent) acceptRelays(ctx context.Context, ln net.Listener, method string, params any) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				a.logf("accepting on %s: %v", ln.Addr(), err)
			}
			return
		}
		a.goSafe("relay", func() { a.relayTCP(c, method, params) })
	}
}

// handleListen serves box.listen: the agent listens on exactly the given
// loopback ports for host.dial relays.
func (a *Agent) handleListen(ctx context.Context) proto.Handler {
	return func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
		var p proto.ListenParams
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		if err := p.Validate(); err != nil {
			return nil, proto.Errorf(proto.CodeInvalidParams, "%s", err.Error())
		}
		return a.setListeners(ctx, p.Ports), nil
	}
}

// setListeners opens and closes host-exception listeners to match ports.
func (a *Agent) setListeners(ctx context.Context, ports []int) proto.ListenReply {
	want := map[int]bool{}
	for _, p := range ports {
		want[p] = true
	}
	a.lmu.Lock()
	defer a.lmu.Unlock()
	for p, ln := range a.listeners {
		if !want[p] {
			ln.Close()
			delete(a.listeners, p)
			a.logf("stopped relaying 127.0.0.1:%d to the host", p)
		}
	}
	reply := proto.ListenReply{}
	for _, p := range ports {
		if _, ok := a.listeners[p]; ok {
			continue
		}
		ln, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			reply.Failed = append(reply.Failed, proto.ListenFailure{Port: p, Error: proto.Truncate(fmt.Sprint(err), 200)})
			continue
		}
		a.listeners[p] = ln
		a.logf("relaying 127.0.0.1:%d to the host", p)
		port := p
		a.goSafe("host exception listener", func() { a.acceptRelays(ctx, ln, proto.MethodHostDial, proto.DialParams{Port: port}) })
	}
	return reply
}

// closeListeners closes every host-exception listener (the agent ends).
func (a *Agent) closeListeners() {
	a.lmu.Lock()
	defer a.lmu.Unlock()
	for p, ln := range a.listeners {
		ln.Close()
		delete(a.listeners, p)
	}
}
