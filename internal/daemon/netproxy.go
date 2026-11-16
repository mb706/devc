package daemon

// The daemon's network services (work order C13). A box of level
// allowlist reaches the network only through host.proxy streams, which
// the daemon serves with netpolicy.Proxy: it refuses by default, resolves
// names itself and connects only to public addresses. Host exceptions
// (`devc net allow host:<port>`) become host.dial streams, which the
// daemon connects to the host's loopback only while the exception is in
// force; the box agent listens on the ports the daemon gives it
// (box.listen) and nothing else.
//
// What a box may reach comes from two places, kept apart on purpose:
//
//   - the allowlist in the configuration is a capability of the box's
//     start: the proxy serves the entries in force both when the box was
//     started and now (box.Capabilities.ProxyAllowlist), like secrets and
//     SSH grants (finding C1-02);
//   - exceptions are granted while the box runs, only from the host (the
//     CLI, or an approved request carried out by the same CLI), and kept in
//     the project's exception record in the state directory. The box
//     channel offers no way to create one: a box can only ask, with
//     host.request, and a network request is high risk, so only the local
//     socket can approve it.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/proto"
)

// Limits of the network services.
const (
	// maxRelayStreams bounds the raw relay streams (proxy and host
	// exceptions) of one box at once; the box agent keeps to the same.
	maxRelayStreams = boxside.DefaultMaxRelays
	// netSyncEvery is how often a connected box's listeners are compared
	// with its exception record (expiry, changes made without net.sync).
	netSyncEvery = 5 * time.Second
	// dialTimeout bounds a connection to the host's loopback.
	dialTimeout = 10 * time.Second
	// hostAddrsEvery is how long the host's own addresses are cached.
	hostAddrsEvery = 10 * time.Second
)

// proxyHint is added to a refusal: how the agent asks for more.
const proxyHint = "; if the task needs it, ask the user (in the box: devc request net <name>:<port>)"

// relayConn is a box stream whose first line was read: reads come from
// the reader that read it.
type relayConn struct {
	st *yamux.Stream
	r  io.Reader
}

func (c *relayConn) Read(b []byte) (int, error)         { return c.r.Read(b) }
func (c *relayConn) SetReadDeadline(t time.Time) error  { return c.st.SetReadDeadline(t) }
func (c *relayConn) SetWriteDeadline(t time.Time) error { return c.st.SetWriteDeadline(t) }

// Write has a stall deadline, as every raw box stream (finding C1-09).
func (c *relayConn) Write(b []byte) (int, error) {
	_ = c.st.SetWriteDeadline(time.Now().Add(streamStall))
	return c.st.Write(b)
}

// CloseWrite half-closes: a yamux Close sends FIN and keeps reading.
func (c *relayConn) CloseWrite() error { return c.st.Close() }

// Close ends both directions at once.
func (c *relayConn) Close() error {
	rawStream{st: c.st}.abort()
	return nil
}

// hostAddrCache caches the host's own addresses for the proxy.
type hostAddrCache struct {
	mu    sync.Mutex
	at    time.Time
	addrs []netip.Addr
	list  func() ([]netip.Addr, error)
}

func (h *hostAddrCache) contains(a netip.Addr) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if time.Since(h.at) > hostAddrsEvery || h.addrs == nil {
		list := h.list
		if list == nil {
			list = netpolicy.HostAddrs
		}
		if as, err := list(); err == nil {
			h.addrs, h.at = as, time.Now()
		}
	}
	return slices.Contains(h.addrs, a)
}

// netLog returns the proxy log of a project.
func (d *Daemon) netLog(ref names.ProjectRef) *netpolicy.NetLog {
	d.netMu.Lock()
	defer d.netMu.Unlock()
	if d.netLogs == nil {
		d.netLogs = map[names.ProjectRef]*netpolicy.NetLog{}
	}
	l := d.netLogs[ref]
	if l == nil {
		l = netpolicy.NewNetLog(d.current().cfg.Paths.NetLogFile(ref))
		d.netLogs[ref] = l
	}
	return l
}

// exceptions reads a project's exception record.
func (d *Daemon) exceptions(ref names.ProjectRef) ([]netpolicy.Exception, error) {
	return netpolicy.ReadExceptions(d.current().cfg.Paths.BoxNetExceptionsFile(ref))
}

// proxyPolicy is what the proxy serves for the box of ref (container
// containerID) now.
func (d *Daemon) proxyPolicy(ref names.ProjectRef, containerID string) (netpolicy.ProxyPolicy, error) {
	e := d.current()
	r, perr := resolvedOf(e, ref)
	if perr != nil {
		return netpolicy.ProxyPolicy{}, errors.New(perr.Message)
	}
	caps, perr := d.startedWith(e.cfg.Paths, ref, containerID)
	if perr != nil {
		return netpolicy.ProxyPolicy{}, errors.New(perr.Message)
	}
	allow, err := caps.ProxyAllowlist(r)
	if errors.Is(err, box.ErrNotAllowlist) {
		return netpolicy.ProxyPolicy{}, fmt.Errorf("%s does not use the allowlist proxy (it was not started at level allowlist, or its configuration changed since; ask the user to bring it up again)", ref)
	}
	if err != nil {
		return netpolicy.ProxyPolicy{}, err
	}
	list, err := d.exceptions(ref)
	if err != nil {
		d.log.Error("reading network exceptions", "ref", ref.String(), "err", err)
		list = nil
	}
	return netpolicy.ProxyPolicy{Allow: allow, Exceptions: netpolicy.NameEntries(list, time.Now())}, nil
}

// serveProxy serves a host.proxy stream with the allowlist proxy.
func (w *boxWorker) serveProxy(ctx context.Context, st *yamux.Stream, br *bufio.Reader, m *proto.RPCMessage) {
	var p proto.ProxyParams
	conn := &relayConn{st: st, r: br}
	if err := proto.DecodeParams(m.Params, &p); err != nil {
		conn.Close()
		return
	}
	d, ref, id := w.d, w.ref, w.containerID
	log := d.netLog(ref)
	px := &netpolicy.Proxy{
		Policy:    func() (netpolicy.ProxyPolicy, error) { return d.proxyPolicy(ref, id) },
		Resolve:   d.o.Resolve,
		Forbidden: d.hostAddrs.contains,
		Exempt:    d.o.ProxyExempt,
		Hint:      proxyHint,
		Log: func(e netpolicy.LogEntry) {
			if err := log.Append(e); err != nil {
				d.log.Warn("writing the proxy log", "ref", ref.String(), "err", err)
			}
			d.log.Debug("proxy", "box", names.BoxName(ref), "host", e.Host, "port", e.Port, "allowed", e.Allowed, "reason", e.Reason)
		},
	}
	px.Serve(ctx, conn)
}

// refuseBusy answers a proxy or service stream that found no free relay
// place.
func refuseBusy(st *yamux.Stream, who string) {
	body := who + ": too many connections at once from this box\n"
	_ = st.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(st, "HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	rawStream{st: st}.abort()
}

// dialRelay is one host.dial connection, ended when its exception ends.
type dialRelay struct {
	port   int
	cancel context.CancelFunc
}

// serveDial serves a host.dial stream: a connection to the host's loopback
// on a port a host exception allows now.
func (w *boxWorker) serveDial(ctx context.Context, st *yamux.Stream, br *bufio.Reader, m *proto.RPCMessage) {
	conn := &relayConn{st: st, r: br}
	defer conn.Close()
	var p proto.DialParams
	if err := proto.DecodeParams(m.Params, &p); err != nil || p.Port < 1 || p.Port > 65535 {
		return
	}
	list, err := w.d.exceptions(w.ref)
	if err != nil {
		w.d.log.Error("reading network exceptions", "ref", w.ref.String(), "err", err)
		return
	}
	if !netpolicy.HostAllowed(list, p.Port, time.Now()) {
		w.d.log.Info("host connection refused: no exception", "box", names.BoxName(w.ref), "port", p.Port)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rel := &dialRelay{port: p.Port, cancel: cancel}
	w.mu.Lock()
	if w.dials == nil {
		w.dials = map[*dialRelay]bool{}
	}
	w.dials[rel] = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.dials, rel)
		w.mu.Unlock()
	}()
	dctx, dcancel := context.WithTimeout(ctx, dialTimeout)
	var dialer net.Dialer
	up, err := dialer.DialContext(dctx, "tcp4", "127.0.0.1:"+strconv.Itoa(p.Port))
	dcancel()
	if err != nil {
		w.d.log.Info("host connection failed", "box", names.BoxName(w.ref), "port", p.Port, "err", err)
		return
	}
	defer up.Close()
	stop := context.AfterFunc(ctx, func() { _ = up.Close(); conn.Close() })
	defer stop()
	w.d.log.Debug("host connection", "box", names.BoxName(w.ref), "port", p.Port)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(up, conn)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, _ = io.Copy(conn, up)
	_ = conn.CloseWrite()
	_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
	<-done
}

// syncNet brings the box agent's host-exception listeners up to date with
// the exception record and ends relays whose exception is gone. force
// sends the list even when it did not change (a new channel).
func (w *boxWorker) syncNet(ctx context.Context, force bool) proto.NetSyncResult {
	res := proto.NetSyncResult{Ports: []int{}}
	list, err := w.d.exceptions(w.ref)
	if err != nil {
		w.d.log.Error("reading network exceptions", "ref", w.ref.String(), "err", err)
		res.Warnings = append(res.Warnings, "the exception record cannot be read: "+err.Error())
		list = nil
	}
	ports := netpolicy.HostPorts(list, time.Now())
	if ports == nil {
		ports = []int{}
	}
	w.mu.Lock()
	for rel := range w.dials {
		if !slices.Contains(ports, rel.port) {
			rel.cancel()
		}
	}
	same := w.netSynced && slices.Equal(ports, w.netPorts)
	w.mu.Unlock()
	res.Connected = w.connected()
	if !res.Connected {
		return res
	}
	res.Ports = ports
	if same && !force {
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var reply proto.ListenReply
	if err := w.call(cctx, proto.MethodBoxListen, proto.ListenParams{Ports: ports}, &reply); err != nil {
		w.d.log.Warn("updating the box's host-exception listeners", "box", names.BoxName(w.ref), "err", err)
		res.Warnings = append(res.Warnings, "the box agent did not take the list of ports: "+proto.Printable(err.Error()))
		return res
	}
	for _, f := range reply.Failed {
		if f.Port < 1 || f.Port > 65535 {
			continue
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("port %d cannot be opened in the box: %s", f.Port, proto.Printable(proto.Truncate(f.Error, 200))))
	}
	w.mu.Lock()
	w.netPorts, w.netSynced = ports, true
	w.mu.Unlock()
	return res
}

// netLoop keeps the listeners in step while the channel is connected.
func (w *boxWorker) netLoop(ctx context.Context, sess *yamux.Session) {
	w.mu.Lock()
	w.netSynced = false
	w.mu.Unlock()
	w.syncNet(ctx, true)
	t := time.NewTicker(netSyncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.CloseChan():
			return
		case <-t.C:
			w.syncNet(ctx, false)
		}
	}
}

// apiNetSync serves net.sync: the CLI changed a project's exceptions.
func (d *Daemon) apiNetSync(ctx context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.NetSyncParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ref, err := names.Parse(p.Ref)
	if err != nil {
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid project")
	}
	d.fresh()
	if _, perr := resolvedOf(d.current(), ref); perr != nil {
		return nil, perr
	}
	w := d.worker(ref)
	if w == nil {
		return proto.NetSyncResult{Ports: []int{}}, nil
	}
	return w.syncNet(ctx, true), nil
}
