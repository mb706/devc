// This file implements the machine side of the machine link (work order
// C11a section 3): one WebSocket connection per hub listed in config.toml,
// dialled out with the client certificate `devc join` stored, reconnected
// with backoff capped at two minutes. Over it the hub is the caller and
// the daemon the server; the daemon itself calls only hub.hello and
// hub.role.
//
// What a hub may do is decided here and nowhere else: hubMethods is the
// allowlist, hubHandlers builds the hub's method table from it, and a
// decision arriving this way has origin hub. The hub cannot stop the
// daemon, read the hub or archive status, call net.sync, decide high-risk
// requests (requests.MayDecide), declare a stronger origin
// (requests.EffectiveOrigin) or learn the daemon's directories and PID;
// nothing it may call starts or stops a box or writes the configuration.
// The first connected hub in the list is the notifier; every connected hub
// is told its role whenever that changes.

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/fleet"
	"github.com/mb706/devc/internal/hubnode"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/wsconn"
)

// hubMethods is the allowlist of daemon methods a hub may call: reading
// boxes, sessions and events, the session commands of C09 (box-scoped:
// they type into or start and stop sessions in a box), receipts, listing
// and deciding requests (low-risk only, by origin), and for the web app
// (C12) reading projects and a session's changes and a session's terminal
// (observing; typing into that one pane only in control mode, which the
// machine's own configuration must allow). Anything else, daemon.stop and
// hubs.status included, does not exist for a hub.
var hubMethods = []string{
	proto.MethodDaemonInfo,
	proto.MethodBoxesList,
	proto.MethodSessionsList,
	proto.MethodSessionsGet,
	proto.MethodEventsRead,
	proto.MethodEventsSubscribe,
	proto.MethodSessionSend,
	proto.MethodSessionAnswer,
	proto.MethodSessionInterrupt,
	proto.MethodSessionKeys,
	proto.MethodSessionStart,
	proto.MethodSessionStop,
	proto.MethodSessionSeen,
	proto.MethodCommandGet,
	proto.MethodRequestsList,
	proto.MethodRequestsDecide,
	proto.MethodProjectsList,
	proto.MethodChangesGet,
	proto.MethodTerminalOpen,
	proto.MethodTerminalConfirm,
	proto.MethodTerminalInput,
	proto.MethodTerminalClose,
}

// hubHandlers is the method table of a hub connection: the allowlisted
// methods, with the hub's origin where it matters.
func (d *Daemon) hubHandlers() map[string]proto.Handler {
	local := d.handlers()
	out := make(map[string]proto.Handler, len(hubMethods))
	for _, m := range hubMethods {
		out[m] = local[m]
	}
	// Decisions carry origin hub; requests.MayDecide refuses high-risk ones.
	out[proto.MethodRequestsDecide] = d.decideHandler(proto.OriginHub)
	// No host details (directories, PID) for a hub.
	out[proto.MethodDaemonInfo] = d.hubInfo
	// No synchronous ingestion and reconciliation on a hub's behalf: the
	// hub follows the event stream.
	out[proto.MethodSessionsList] = func(ctx context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
		var p proto.SessionsListParams
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		p.Sync = false
		b, _ := json.Marshal(p)
		return d.apiSessions(ctx, c, b)
	}
	return out
}

func (d *Daemon) hubInfo(ctx context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
	res, err := d.apiInfo(ctx, c, params)
	if err != nil {
		return nil, err
	}
	info := res.(proto.DaemonInfo)
	info.ConfigDir, info.StateDir, info.PID = "", "", 0
	return info, nil
}

// Backoff of hub connections, the timeouts of dialling and of pings, and
// how often a hub without credentials is looked at again.
const (
	hubBackoffMin  = time.Second
	hubBackoffMax  = 2 * time.Minute
	hubDialTimeout = 30 * time.Second
	hubPing        = 30 * time.Second
	hubPingTimeout = 20 * time.Second
	// hubStable is how long a connection must have lasted for the next
	// attempt to start from the shortest pause again.
	hubStable    = 30 * time.Second
	hubCredsWait = 30 * time.Second
)

// hubManager keeps one link per listed hub.
type hubManager struct {
	d *Daemon

	mu    sync.Mutex
	ctx   context.Context
	p     *paths.Paths
	order []string
	links map[string]*hubLink
}

func newHubManager(d *Daemon) *hubManager {
	return &hubManager{d: d, links: map[string]*hubLink{}}
}

// hubLink is the connection to one hub.
type hubLink struct {
	m     *hubManager
	url   string
	stamp string
	stop  context.CancelFunc
	done  chan struct{}

	mu       sync.Mutex
	state    string
	detail   string
	since    time.Time
	machine  string
	conn     *proto.Conn
	roleWant bool
	roleSent *bool
	roleCh   chan struct{}

	// uploader keeps this machine's archive on the hub current
	// (archive.go); archiveWake starts a pass at once.
	uploader    *fleet.Uploader
	archiveWake chan struct{}
}

// start begins connecting (the daemon runs).
func (m *hubManager) start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.sync(nil)
}

// sync makes the links follow the hub list of c (nil: the last one seen)
// and the stored credentials.
func (m *hubManager) sync(c *config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c != nil {
		m.p = c.Paths
		m.order = m.order[:0]
		for _, h := range c.Global.Hubs {
			u, err := hubnode.NormalizeURL(h.URL)
			if err == nil && !contains(m.order, u) {
				m.order = append(m.order, u)
			}
		}
	}
	if m.ctx == nil || m.p == nil {
		return
	}
	want := map[string]bool{}
	for _, u := range m.order {
		want[u] = true
		stamp := hubnode.Stamp(m.p, u)
		if l := m.links[u]; l != nil {
			if l.stamp == stamp {
				continue
			}
			m.d.log.Info("hub credentials changed; reconnecting", "hub", u)
			l.stop()
		}
		ctx, cancel := context.WithCancel(m.ctx)
		l := &hubLink{m: m, url: u, stamp: stamp, stop: cancel, done: make(chan struct{}), state: proto.HubConnecting,
			since: time.Now().UTC(), roleCh: make(chan struct{}, 1), archiveWake: make(chan struct{}, 1)}
		if old := m.links[u]; old != nil {
			// The bookkeeping carries over a reconnection with new
			// credentials (the machine name decides whether it is kept).
			l.uploader = old.uploader
		}
		if l.uploader == nil {
			l.uploader = m.d.newUploader(u)
		}
		m.links[u] = l
		p := m.p
		m.d.goSafe("hub "+u, func() {
			defer close(l.done)
			l.run(ctx, p)
		})
		m.d.goSafe("archive "+u, func() { l.archiveLoop(ctx) })
	}
	for u, l := range m.links {
		if !want[u] {
			m.d.log.Info("hub removed from the list; disconnecting", "hub", u)
			l.stop()
			delete(m.links, u)
		}
	}
	m.recomputeLocked()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// recomputeLocked makes the first connected hub in the list the notifier
// and tells every connected hub its role.
func (m *hubManager) recomputeLocked() {
	first := ""
	for _, u := range m.order {
		if l := m.links[u]; l != nil && l.connected() {
			first = u
			break
		}
	}
	for _, u := range m.order {
		if l := m.links[u]; l != nil {
			l.wantRole(u == first)
		}
	}
}

func (m *hubManager) recompute() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recomputeLocked()
}

// status reports every listed hub.
func (m *hubManager) status() []proto.HubStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []proto.HubStatus{}
	for _, u := range m.order {
		st := proto.HubStatus{URL: u, State: proto.HubDisconnected}
		if l := m.links[u]; l != nil {
			l.mu.Lock()
			since := l.since
			st.State, st.Detail, st.Since, st.Machine = l.state, l.detail, &since, l.machine
			st.Notifier = l.state == proto.HubConnected && l.roleSent != nil && *l.roleSent
			l.mu.Unlock()
			if l.uploader != nil {
				a := l.uploader.Status()
				st.Archive = &a
			}
		}
		out = append(out, st)
	}
	return out
}

func (l *hubLink) connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state == proto.HubConnected
}

func (l *hubLink) setState(state, detail string) {
	l.mu.Lock()
	if l.state != state || l.detail != detail {
		l.state, l.detail, l.since = state, proto.Printable(proto.Truncate(detail, 300)), time.Now().UTC()
	}
	l.mu.Unlock()
}

func (l *hubLink) wantRole(on bool) {
	l.mu.Lock()
	l.roleWant = on
	l.mu.Unlock()
	select {
	case l.roleCh <- struct{}{}:
	default:
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// run connects again and again until the link is stopped.
func (l *hubLink) run(ctx context.Context, p *paths.Paths) {
	log := l.m.d.log
	backoff := hubBackoffMin
	for ctx.Err() == nil {
		creds, err := hubnode.Load(p, l.url)
		if err != nil {
			l.setState(proto.HubNoCreds, err.Error())
			// A new join changes the stamp, and sync starts a new link.
			if !sleepCtx(ctx, hubCredsWait) {
				return
			}
			continue
		}
		l.setState(proto.HubConnecting, "")
		began := time.Now()
		helloed, err := l.connectOnce(ctx, creds)
		if ctx.Err() != nil {
			return
		}
		detail := "connection ended"
		if err != nil {
			detail = err.Error()
		}
		l.setState(proto.HubDisconnected, detail)
		l.m.recompute()
		if helloed && time.Since(began) >= hubStable {
			backoff = hubBackoffMin
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff)/5+1))
		log.Info("hub connection lost; retrying", "hub", l.url, "in", wait.Round(time.Millisecond), "err", detail)
		if !sleepCtx(ctx, wait) {
			return
		}
		backoff = min(backoff*2, hubBackoffMax)
	}
}

// connectOnce dials, says hello, serves the hub until the connection ends,
// and reports whether hello succeeded.
func (l *hubLink) connectOnce(ctx context.Context, creds *hubnode.Creds) (bool, error) {
	d := l.m.d
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:     creds.TLSConfig(),
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: hubDialTimeout,
		ForceAttemptHTTP2:   false,
	}}
	defer client.CloseIdleConnections()
	dctx, cancel := context.WithTimeout(ctx, hubDialTimeout)
	ws, resp, err := websocket.Dial(dctx, hubnode.MachineURL(l.url), &websocket.DialOptions{HTTPClient: client})
	cancel()
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("the hub refused the connection: %s", resp.Status)
		}
		return false, err
	}
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()
	wc := wsconn.New(cctx, ws)
	conn := proto.NewConn(wc, proto.Options{
		Handlers:     d.hubHandlers(),
		Workers:      8,
		Context:      cctx,
		CallTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		Log:          d.log,
	})
	defer conn.Close()
	host, _ := os.Hostname()
	if len(host) > 255 {
		host = host[:255]
	}
	var reply proto.HubHelloReply
	hctx, hcancel := context.WithTimeout(cctx, 30*time.Second)
	err = conn.Call(hctx, proto.MethodHubHello, proto.HubHello{Protocol: proto.HubProtocol, Name: proto.Printable(host), Version: proto.Printable(d.version), Instance: d.instance}, &reply)
	hcancel()
	if err != nil {
		return false, fmt.Errorf("hub.hello: %w", err)
	}
	if reply.Protocol != proto.HubProtocol || !names.Valid(reply.Machine) {
		return false, fmt.Errorf("the hub answered hello with protocol %d and machine %q", reply.Protocol, proto.Printable(proto.Truncate(reply.Machine, 64)))
	}
	l.mu.Lock()
	l.conn, l.machine, l.roleSent = conn, reply.Machine, nil
	l.mu.Unlock()
	l.setState(proto.HubConnected, "")
	d.log.Info("connected to hub", "hub", l.url, "machine", reply.Machine)
	l.m.recompute()
	goTask(d.log, &task{what: "the pings of the hub link " + l.url, end: func() { _ = conn.Close() }}, func() { l.pinger(cctx, wc, conn) })
	goTask(d.log, &task{what: "the roles of the hub link " + l.url, end: func() { _ = conn.Close() }}, func() { l.roles(cctx, conn) })
	<-conn.Done()
	l.mu.Lock()
	l.conn, l.roleSent = nil, nil
	l.mu.Unlock()
	return true, conn.Err()
}

// pinger checks that the hub is still there; the hub reads nothing for
// long stretches.
func (l *hubLink) pinger(ctx context.Context, wc *wsconn.Conn, conn *proto.Conn) {
	t := time.NewTicker(hubPing)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, hubPingTimeout)
		err := wc.WS().Ping(pctx)
		cancel()
		if err != nil {
			l.m.d.log.Info("hub does not answer pings", "hub", l.url, "err", err)
			conn.Close()
			return
		}
	}
}

// roles sends hub.role whenever the wanted role differs from the one sent.
func (l *hubLink) roles(ctx context.Context, conn *proto.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.roleCh:
		}
		for {
			l.mu.Lock()
			want, sent := l.roleWant, l.roleSent
			l.mu.Unlock()
			if sent != nil && *sent == want {
				break
			}
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := conn.Call(cctx, proto.MethodHubRole, proto.HubRole{Notifier: want}, nil)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					l.m.d.log.Warn("telling the hub its role", "hub", l.url, "err", err)
					conn.Close()
				}
				return
			}
			l.mu.Lock()
			l.roleSent = &want
			l.mu.Unlock()
			l.m.d.log.Info("hub role", "hub", l.url, "notifier", want)
		}
	}
}

// apiHubs serves hubs.status on the local socket.
func (d *Daemon) apiHubs(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	if err := noParams(params); err != nil {
		return nil, err
	}
	d.fresh()
	return proto.HubsStatusResult{Hubs: d.hubs.status()}, nil
}
