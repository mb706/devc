// This file serves service logins (work order C18). A service is a
// capability a box is started with (grants.services, recorded in
// box.Capabilities like secrets and SSH grants, finding C1-02): the daemon
// tells the box agent on which loopback ports to listen (box.services),
// the agent relays each connection unparsed on a host.service stream (the
// pattern of C13's proxy and host exceptions), and the daemon decides
// everything: the service must be in force for this box (granted at its
// start and now, with an unchanged definition), the request head is
// checked on its raw bytes and against the rules (svclogin.CheckHead,
// svclogin.Allowed), and only then does net/http's server parse it and
// svclogin.Proxy forward it to the upstream with the credential, which
// never travels towards the box.
//
// Cookie services need a login (devc service login), whose jar is a realm
// secret. A missing jar, one older than auth.refresh or without a live
// cookie for the upstream's host, or a 401 or a redirect to the login path
// from the upstream, means the service needs a login: boxes get 503 with a
// clear message and the live sessions of the box a note. With auth.auto
// the daemon runs the login itself, once at a time per service and at most
// once a minute, as a child process of the same binary (`devc service
// login`), so the CLI's checks, lock and container apply unchanged.

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/svclogin"
)

// Service proxy limits.
const (
	// serviceHeadTimeout bounds the wait for a request head.
	serviceHeadTimeout = 30 * time.Second
	// serviceSyncEvery is how often a connected box's service listeners are
	// compared with what is in force.
	serviceSyncEvery = 5 * time.Second
	// autoLoginGap is the least time between two automatic logins of one
	// service; autoLoginTimeout bounds one.
	autoLoginGap     = time.Minute
	autoLoginTimeout = 6 * time.Minute
)

// serviceGrace is how long a service stream may last beyond the service's
// limits.timeout, for the answer that says the time is up (finding D2-04).
var serviceGrace = 5 * time.Second

// serviceManager serves the host.service streams of all boxes.
type serviceManager struct {
	d     *Daemon
	proxy *svclogin.Proxy
	srv   *http.Server
	ln    *connListener

	mu   sync.Mutex
	logs map[string]*svclogin.Log
	// noted records, per service, the boxes whose sessions were told of
	// the current need for a login (by the time the need began).
	noted map[string]map[names.ProjectRef]time.Time
	// Automatic logins: the one running per service, and the time and
	// outcome of the last.
	flights  map[string]*loginFlight
	lastAuto map[string]time.Time
	lastErr  map[string]error
}

// loginFlight is an automatic login in progress; requests that need it
// meanwhile wait on done instead of starting another.
type loginFlight struct {
	done chan struct{}
	err  error
}

// newServiceTransport reaches the upstreams with the host's trust store
// (SSL_CERT_FILE and SSL_CERT_DIR apply), never through a proxy, without
// following redirects (the reverse proxy never does).
func newServiceTransport() *http.Transport {
	return &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    8,
		IdleConnTimeout:        90 * time.Second,
		ForceAttemptHTTP2:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: svclogin.MaxHead,
		ExpectContinueTimeout:  time.Second,
	}
}

func newServiceManager(d *Daemon) *serviceManager {
	tr := d.o.ServiceTransport
	if tr == nil {
		tr = newServiceTransport()
	}
	m := &serviceManager{d: d, proxy: &svclogin.Proxy{Transport: tr}, ln: newConnListener(),
		logs: map[string]*svclogin.Log{}, noted: map[string]map[names.ProjectRef]time.Time{},
		flights: map[string]*loginFlight{}, lastAuto: map[string]time.Time{}, lastErr: map[string]error{}}
	m.srv = &http.Server{
		Handler:           m.proxy,
		ReadHeaderTimeout: serviceHeadTimeout,
		MaxHeaderBytes:    svclogin.MaxHead,
		IdleTimeout:       time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if sc, ok := c.(*svcConn); ok {
				return svclogin.WithRequest(ctx, sc.req)
			}
			return ctx
		},
	}
	// One request per connection: what was checked on the raw bytes is
	// exactly what is served, and nothing can follow it.
	m.srv.SetKeepAlivesEnabled(false)
	return m
}

func (m *serviceManager) start() {
	m.d.goSafe("service proxy", func() { _ = m.srv.Serve(m.ln) })
}

func (m *serviceManager) stop() {
	_ = m.srv.Close()
	_ = m.ln.Close()
}

func serviceID(realm, name string) string { return realm + "/" + name }

// log is the request log of a service.
func (m *serviceManager) log(realm, name string) *svclogin.Log {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := serviceID(realm, name)
	l := m.logs[k]
	if l == nil {
		l = svclogin.NewLog(m.d.current().cfg.Paths.ServiceLogFile(realm, name))
		m.logs[k] = l
	}
	return l
}

// append logs one request of a box; a log that cannot be written is a
// warning and changes no decision.
func (m *serviceManager) append(realm, name string, e svclogin.LogEntry) {
	if err := m.log(realm, name).Append(e); err != nil {
		m.d.log.Warn("writing a service log", "service", serviceID(realm, name), "err", err)
	}
	m.d.log.Debug("service request", "service", serviceID(realm, name), "box", e.Box, "method", e.Method, "path", e.Path, "status", e.Status)
}

// serviceInForce is the definition of service name for the box of ref
// (container containerID), or the reason the box may not use it.
func (d *Daemon) serviceInForce(e *env, ref names.ProjectRef, containerID, name string) (*config.Service, string) {
	r, perr := resolvedOf(e, ref)
	if perr != nil {
		return nil, perr.Message
	}
	if !slices.Contains(r.Grants.Services, name) {
		return nil, fmt.Sprintf("the service %s is not granted to %s", name, ref)
	}
	caps, perr := d.startedWith(e.cfg.Paths, ref, containerID)
	if perr != nil {
		return nil, perr.Message
	}
	s, err := caps.Service(r, name)
	if err != nil {
		return nil, err.Error()
	}
	return s, ""
}

// servicePorts are the services the agent of the box of ref (container
// containerID) should serve now.
func (d *Daemon) servicePorts(ref names.ProjectRef, containerID string) []proto.ServicePort {
	e := d.current()
	r, perr := resolvedOf(e, ref)
	if perr != nil {
		return []proto.ServicePort{}
	}
	caps, err := box.ReadCapabilities(e.cfg.Paths, ref)
	if err != nil || caps.ContainerID != containerID {
		return []proto.ServicePort{}
	}
	out := []proto.ServicePort{}
	for _, sc := range caps.ServicePorts(r) {
		out = append(out, proto.ServicePort{Name: sc.Name, Port: sc.Port})
	}
	return out
}

// writeRefusal answers a service connection with a message and ends it.
func writeRefusal(c *relayConn, status int, msg string) {
	body := msg + "\n"
	_ = c.st.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(c.st, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
	c.Close()
}

// serveService serves one host.service stream, in this order: the service
// must be in force for this box, the head must arrive within 30 seconds
// and pass svclogin.CheckHead, and a rule must allow its method and
// normalised path; only then does the HTTP server see the stream. A
// refusal is answered with its status and logged here; an incomplete head
// only closes the stream, and a refusal for a service that is not defined
// is not logged, so a box cannot make the host create a log for a name of
// its choosing.
func (w *boxWorker) serveService(ctx context.Context, st *yamux.Stream, br *bufio.Reader, m *proto.RPCMessage) {
	conn := &relayConn{st: st, r: br}
	d := w.d
	var p proto.ServiceStreamParams
	if err := proto.DecodeParams(m.Params, &p); err != nil || !names.Valid(p.Name) {
		conn.Close()
		return
	}
	d.fresh() // a grant removed a moment ago is gone for this request
	e := d.current()
	entry := func(method, path string, status int, reason string) {
		d.svc.append(w.ref.Realm, p.Name, svclogin.LogEntry{Time: time.Now().UTC(), Box: w.ref.String(), Method: method, Path: path, Status: status, Reason: reason})
	}
	svc, why := d.serviceInForce(e, w.ref, w.containerID, p.Name)
	if svc == nil {
		d.log.Warn("refused a service connection", "box", names.BoxName(w.ref), "service", p.Name, "reason", why)
		if _, ok := e.cfg.Service(w.ref.Realm, p.Name); ok {
			entry("", "", http.StatusForbidden, why)
		}
		writeRefusal(conn, http.StatusForbidden, "devc service "+p.Name+": "+why)
		return
	}
	_ = st.SetReadDeadline(time.Now().Add(serviceHeadTimeout))
	raw, rf := svclogin.ReadHead(br)
	var head *svclogin.Head
	if rf == nil {
		head, rf = svclogin.CheckHead(raw, svc.Port)
	}
	if rf != nil {
		if rf.Status == 0 {
			conn.Close()
			return
		}
		method, path := "", ""
		if raw != nil {
			first, _, _ := strings.Cut(string(raw), "\r\n")
			parts := strings.SplitN(first, " ", 3)
			method = parts[0]
			if len(parts) > 1 {
				path, _, _ = strings.Cut(parts[1], "?")
			}
		}
		msg := "devc service " + svc.Name + ": " + rf.Msg
		entry(method, path, rf.Status, rf.Msg)
		writeRefusal(conn, rf.Status, msg)
		return
	}
	if svclogin.Allowed(svc.Allow, head.Method, head.Path) < 0 {
		entry(head.Method, head.Path, http.StatusForbidden, "no rule allows it")
		writeRefusal(conn, http.StatusForbidden, svclogin.Needed(svc, head.Method, head.Path))
		return
	}
	// The whole exchange ends with the service's timeout (finding D2-04):
	// every later read and write on the stream, by the HTTP server or the
	// proxy, carries that deadline, so a box that stops reading or never
	// finishes its body cannot hold the stream, its relay place and the
	// handler; only this stream ends, not the channel.
	limit := time.Now().Add(svc.TimeoutDuration() + serviceGrace)
	conn.limit = limit
	_ = conn.SetReadDeadline(limit)
	_ = conn.SetWriteDeadline(limit)
	up, err := svc.UpstreamURL()
	if err != nil {
		writeRefusal(conn, http.StatusBadGateway, "devc service "+svc.Name+": invalid upstream")
		return
	}
	ref := w.ref
	req := &svclogin.Request{Service: svc, Upstream: up, Head: head, Box: ref.String(),
		Credential: func(ctx context.Context, path string) (svclogin.Credential, error) {
			return d.svc.credential(ctx, ref, svc, up.Hostname(), path)
		},
		LoginNeeded: func(reason string) { d.svc.loginNeeded(ref, svc, reason) },
		Log:         func(e svclogin.LogEntry) { d.svc.append(ref.Realm, svc.Name, e) },
	}
	sc := &svcConn{rc: conn, r: io.MultiReader(bytes.NewReader(raw), br), req: req, done: make(chan struct{})}
	if !d.svc.ln.push(ctx, sc) {
		conn.Close()
		return
	}
	// Past the limit the stream is closed whatever the handler is doing.
	end := time.NewTimer(time.Until(limit) + time.Second)
	defer end.Stop()
	select {
	case <-sc.done:
	case <-ctx.Done():
		_ = sc.Close()
	case <-end.C:
		d.log.Warn("ended a service stream past its time", "box", names.BoxName(ref), "service", svc.Name)
		_ = sc.Close()
	}
}

// credential is what the proxy adds to a request of the box of ref.
func (m *serviceManager) credential(ctx context.Context, ref names.ProjectRef, svc *config.Service, host, path string) (svclogin.Credential, error) {
	d := m.d
	e := d.current()
	store := d.secretStore(e)
	switch svc.Auth.Kind {
	case config.ServiceAuthHeader:
		v, err := store.Get(svc.Realm, svc.Auth.ValueSecret)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			return svclogin.Credential{}, &svclogin.Unavailable{Msg: fmt.Sprintf("devc service %s: its credential is not set on the host; ask the user to run 'devc secret set %s %s'", svc.Name, svc.Realm, svc.Auth.ValueSecret)}
		case err != nil:
			d.log.Error("reading a service credential", "service", serviceID(svc.Realm, svc.Name), "err", err)
			return svclogin.Credential{}, &svclogin.Unavailable{Msg: fmt.Sprintf("devc service %s: the host could not read its credential", svc.Name)}
		}
		cred, err := svclogin.HeaderCredential(svc.Auth.Header, v)
		if err != nil {
			return svclogin.Credential{}, &svclogin.Unavailable{Msg: fmt.Sprintf("devc service %s: %s; ask the user to set it again with 'devc secret set %s %s'", svc.Name, err, svc.Realm, svc.Auth.ValueSecret)}
		}
		return cred, nil
	case config.ServiceAuthCookie:
		jar, problem := m.loginProblem(svc, host)
		if problem != "" && svc.Auth.Auto {
			aerr := m.autoLogin(ctx, svc)
			jar, problem = m.loginProblem(svc, host)
			if problem != "" {
				// The failure's text comes from the login command's output,
				// which may hold what the box must not see: it stays on the
				// host (the state file, the daemon's log).
				msg := fmt.Sprintf("devc service %s needs a login (%s) and the automatic login did not help", svc.Name, problem)
				if aerr != nil {
					msg = fmt.Sprintf("devc service %s needs a login (%s); the automatic login failed (the user sees why with 'devc service status %s %s')", svc.Name, problem, svc.Realm, svc.Name)
				}
				m.markNeeded(ref, svc, jar, problem)
				return svclogin.Credential{}, &svclogin.Unavailable{Msg: msg + fmt.Sprintf("; ask the user to run 'devc service login %s %s' on the host, then retry", svc.Realm, svc.Name)}
			}
		}
		if problem != "" {
			m.markNeeded(ref, svc, jar, problem)
			return svclogin.Credential{}, &svclogin.Unavailable{Msg: fmt.Sprintf("devc service %s needs a login: %s. Ask the user to run 'devc service login %s %s' on the host, then retry.", svc.Name, problem, svc.Realm, svc.Name)}
		}
		return jar.Credential(host, path, time.Now()), nil
	}
	return svclogin.Credential{}, fmt.Errorf("unknown kind %q", svc.Auth.Kind)
}

// loadJar reads a cookie service's jar (nil when there is none).
func (m *serviceManager) loadJar(svc *config.Service) (*svclogin.Jar, error) {
	e := m.d.current()
	b, err := m.d.secretStore(e).Get(svc.Realm, config.JarSecret(svc.Name))
	if errors.Is(err, secrets.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return svclogin.ParseJar(b, svc.Name)
}

// loginProblem returns the jar and why it cannot be used ("" when it can).
func (m *serviceManager) loginProblem(svc *config.Service, host string) (*svclogin.Jar, string) {
	jar, err := m.loadJar(svc)
	if err != nil {
		m.d.log.Warn("reading a cookie jar", "service", serviceID(svc.Realm, svc.Name), "err", err)
		return nil, "the stored cookie jar cannot be used"
	}
	if p := jar.Problem(svc.RefreshDuration(), host, time.Now()); p != "" {
		return jar, p
	}
	st, err := svclogin.ReadState(m.d.current().cfg.Paths.ServiceStateFile(svc.Realm, svc.Name))
	if err != nil {
		m.d.log.Warn("reading a service's login state", "service", serviceID(svc.Realm, svc.Name), "err", err)
	}
	if need, reason := st.Current(jar); need {
		return jar, reason
	}
	return jar, ""
}

// markNeeded records that a cookie service needs a login, once per need,
// and tells the live sessions of the box of ref, once per box and need.
func (m *serviceManager) markNeeded(ref names.ProjectRef, svc *config.Service, jar *svclogin.Jar, reason string) {
	d := m.d
	file := d.current().cfg.Paths.ServiceStateFile(svc.Realm, svc.Name)
	k := serviceID(svc.Realm, svc.Name)
	m.mu.Lock()
	st, err := svclogin.ReadState(file)
	if err != nil {
		d.log.Warn("reading a service's login state", "service", k, "err", err)
	}
	if need, _ := st.Current(jar); !need {
		st.NeedsLogin, st.Since, st.Reason = true, time.Now().UTC(), reason
		if werr := svclogin.WriteState(file, st); werr != nil {
			d.log.Warn("writing a service's login state", "service", k, "err", werr)
		}
		d.log.Warn("service needs a login", "service", k, "reason", reason)
	}
	since := st.Since
	if m.noted[k] == nil {
		m.noted[k] = map[names.ProjectRef]time.Time{}
	}
	tell := !m.noted[k][ref].Equal(since)
	m.noted[k][ref] = since
	m.mu.Unlock()
	if tell {
		text := fmt.Sprintf("devc: the service %s needs a login (%s): run 'devc service login %s %s' on the host", svc.Name, reason, svc.Realm, svc.Name)
		d.goSafe("service note "+ref.String(), func() { d.noteLiveSessions(ref, text) })
	}
}

// loginNeeded handles a cookie service's upstream asking for a login.
func (m *serviceManager) loginNeeded(ref names.ProjectRef, svc *config.Service, reason string) {
	jar, err := m.loadJar(svc)
	if err != nil {
		jar = nil
	}
	m.markNeeded(ref, svc, jar, reason)
	if svc.Auth.Auto {
		m.d.goSafe("service login "+serviceID(svc.Realm, svc.Name), func() { _ = m.autoLogin(m.d.ctx, svc) })
	}
}

// autoLogin runs the service's login, at most one at a time per service
// (callers that arrive meanwhile wait for it) and at most once per
// autoLoginGap; within the gap the last outcome is returned and the caller
// looks at the jar again.
func (m *serviceManager) autoLogin(ctx context.Context, svc *config.Service) error {
	k := serviceID(svc.Realm, svc.Name)
	m.mu.Lock()
	f := m.flights[k]
	if f == nil {
		if t, ok := m.lastAuto[k]; ok && time.Since(t) < autoLoginGap {
			err := m.lastErr[k]
			m.mu.Unlock()
			return err
		}
		f = &loginFlight{done: make(chan struct{})}
		m.flights[k] = f
		m.lastAuto[k] = time.Now()
		m.mu.Unlock()
		realm, name := svc.Realm, svc.Name
		m.d.goSafe("service login "+k, func() {
			err := m.runLogin(realm, name)
			m.mu.Lock()
			f.err = err
			m.lastErr[k] = err
			delete(m.flights, k)
			m.mu.Unlock()
			close(f.done)
		})
	} else {
		m.mu.Unlock()
	}
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runLogin runs `devc service login` as a child process.
func (m *serviceManager) runLogin(realm, name string) error {
	d := m.d
	k := serviceID(realm, name)
	d.log.Info("logging in to a service automatically", "service", k)
	res, err := d.runChild(d.ctx, []string{"service", "login", "--quiet", "--", realm, name}, autoLoginTimeout)
	file := d.current().cfg.Paths.ServiceStateFile(realm, name)
	if err != nil {
		msg := lastLine(string(res.Stderr))
		if msg == "" {
			msg = err.Error()
		}
		msg = printableShort(strings.TrimPrefix(msg, "devc: "))
		d.log.Warn("automatic service login failed", "service", k, "err", msg)
		m.mu.Lock()
		if st, rerr := svclogin.ReadState(file); rerr == nil {
			st.AutoError, st.AutoAt = msg, time.Now().UTC()
			_ = svclogin.WriteState(file, st)
		}
		m.mu.Unlock()
		return errors.New(msg)
	}
	d.log.Info("logged in to a service", "service", k)
	return nil
}

// noteLiveSessions records a note in every live session of the box of ref.
func (d *Daemon) noteLiveSessions(ref names.ProjectRef, text string) {
	for _, v := range d.hub.views(listFilter{realm: ref.Realm, project: ref.Project, live: true}) {
		if err := d.recordHost(d.ctx, ref, v.Session, proto.TypeNote, proto.Note{Text: proto.Truncate(text, proto.MaxNoteText)}); err != nil {
			d.log.Warn("recording a service note", "ref", ref.String(), "session", v.Session, "err", err)
		}
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func printableShort(s string) string { return proto.Printable(proto.Truncate(s, 300)) }

// syncServices brings the box agent's service listeners up to date. force
// sends the list even when it did not change (a new channel).
func (w *boxWorker) syncServices(ctx context.Context, force bool) {
	ports := w.d.servicePorts(w.ref, w.containerID)
	w.mu.Lock()
	same := w.svcSynced && slices.Equal(ports, w.svcPorts)
	skip := w.svcUnsupported || (!w.svcSynced && len(ports) == 0)
	w.mu.Unlock()
	if skip {
		// Nothing to serve and nothing served yet: an agent from before
		// service logins need not be asked.
		w.mu.Lock()
		if !w.svcUnsupported {
			w.svcPorts, w.svcSynced = ports, true
		}
		w.mu.Unlock()
		return
	}
	if (same && !force) || !w.connected() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var reply proto.ListenReply
	err := w.call(cctx, proto.MethodBoxServices, proto.ServicesParams{Services: ports}, &reply)
	var pe *proto.Error
	switch {
	case errors.As(err, &pe) && pe.Code == proto.CodeMethodNotFound:
		w.d.log.Warn("the box's agent cannot serve services; restart the box", "box", names.BoxName(w.ref))
		w.mu.Lock()
		w.svcUnsupported = true
		w.mu.Unlock()
		return
	case err != nil:
		w.d.log.Warn("updating the box's service listeners", "box", names.BoxName(w.ref), "err", err)
		return
	}
	for _, f := range reply.Failed {
		if f.Port > 0 && f.Port <= 65535 {
			w.d.log.Warn("a service port cannot be opened in the box", "box", names.BoxName(w.ref), "port", f.Port, "err", proto.Printable(proto.Truncate(f.Error, 200)))
		}
	}
	w.mu.Lock()
	w.svcPorts, w.svcSynced = ports, true
	w.mu.Unlock()
}

// serviceLoop keeps the listeners in step while the channel is connected.
func (w *boxWorker) serviceLoop(ctx context.Context, sess *yamux.Session) {
	w.mu.Lock()
	w.svcSynced, w.svcUnsupported = false, false
	w.mu.Unlock()
	w.syncServices(ctx, true)
	t := time.NewTicker(serviceSyncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.CloseChan():
			return
		case <-t.C:
			w.syncServices(ctx, false)
		}
	}
}

// connListener hands service connections to the HTTP server.
type connListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newConnListener() *connListener {
	return &connListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *connListener) Addr() net.Addr { return svcAddr{} }

func (l *connListener) push(ctx context.Context, c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.done:
		return false
	case <-ctx.Done():
		return false
	}
}

type svcAddr struct{}

func (svcAddr) Network() string { return "devc" }
func (svcAddr) String() string  { return "box" }

// svcConn is a service stream as the HTTP server sees it: reads replay the
// checked head first.
type svcConn struct {
	rc   *relayConn
	r    io.Reader
	req  *svclogin.Request
	done chan struct{}
	once sync.Once
}

func (c *svcConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *svcConn) Write(b []byte) (int, error) { return c.rc.Write(b) }
func (c *svcConn) CloseWrite() error           { return c.rc.CloseWrite() }
func (c *svcConn) LocalAddr() net.Addr         { return svcAddr{} }
func (c *svcConn) RemoteAddr() net.Addr        { return svcAddr{} }

// The deadlines the HTTP server sets are clamped to the exchange's limit
// by relayConn (the server clears its read deadline after the head).
func (c *svcConn) SetDeadline(t time.Time) error {
	_ = c.rc.SetReadDeadline(t)
	return c.rc.SetWriteDeadline(t)
}
func (c *svcConn) SetReadDeadline(t time.Time) error  { return c.rc.SetReadDeadline(t) }
func (c *svcConn) SetWriteDeadline(t time.Time) error { return c.rc.SetWriteDeadline(t) }

func (c *svcConn) Close() error {
	c.once.Do(func() {
		c.rc.Close()
		close(c.done)
	})
	return nil
}
