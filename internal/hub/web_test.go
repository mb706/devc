package hub

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/wsconn"
	"github.com/mb706/devc/web"
)

// TestStaticFiles: every embedded file is served to a device certificate
// without a login, with its content type, the security headers and the
// right cache lifetime; machines and requests without a certificate get
// nothing.
func TestStaticFiles(t *testing.T) {
	h := newTestHub(t, "hub.test")
	dev := h.issue(RoleDevice, "phone")
	mach := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	want := map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8", ".webmanifest": "application/manifest+json", ".svg": "image/svg+xml", ".png": "image/png"}
	paths := StaticPaths()
	if len(paths) < 25 {
		t.Fatalf("only %d files embedded: %v", len(paths), paths)
	}
	for _, f := range append(paths, "/") {
		if f == "/index.html" {
			continue // served as /
		}
		w := p.req(dev, "GET", f, "", "", nil)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", f, w.Code, w.Body)
			continue
		}
		ext := path.Ext(f)
		if f == "/" {
			ext = ".html"
		}
		hd := w.Header()
		if hd.Get("Content-Type") != want[ext] {
			t.Errorf("%s: content type %q", f, hd.Get("Content-Type"))
		}
		if hd.Get("Content-Security-Policy") != contentSecurityPolicy || hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("X-Frame-Options") != "DENY" ||
			hd.Get("Referrer-Policy") != "no-referrer" || hd.Get("Cross-Origin-Resource-Policy") != "same-origin" {
			t.Errorf("%s: security headers %v", f, hd)
		}
		cc := hd.Get("Cache-Control")
		if strings.HasPrefix(f, "/vendor/") != strings.Contains(cc, "immutable") || (!strings.HasPrefix(f, "/vendor/") && cc != "no-cache") {
			t.Errorf("%s: cache control %q", f, cc)
		}
		if et := hd.Get("ETag"); et == "" {
			t.Errorf("%s: no ETag", f)
		} else if w2 := p.req(dev, "GET", f, "", "", func(r *http.Request) { r.Header.Set("If-None-Match", et) }); w2.Code != http.StatusNotModified {
			t.Errorf("%s: revalidation %d", f, w2.Code)
		}
	}
	// The page may be opened from another site; its parts may not be read
	// by one.
	cross := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }
	if w := p.req(dev, "GET", "/", "", "", cross); w.Code != 200 {
		t.Errorf("the page from a link elsewhere: %d", w.Code)
	}
	if w := p.req(dev, "GET", "/app/main.js", "", "", cross); w.Code != 403 {
		t.Errorf("a script read cross-site: %d", w.Code)
	}
	for _, c := range []struct {
		path string
		code int
	}{{"/app/nothing.js", 404}, {"/vendor/preact-10.29.8/LICENSE", 404}, {"/index.html", 404}, {"/check.sh", 404}, {"/embed.go", 404}} {
		if w := p.req(dev, "GET", c.path, "", "", nil); w.Code != c.code {
			t.Errorf("%s: %d", c.path, w.Code)
		}
	}
	if w := p.req(mach, "GET", "/", "", "", nil); w.Code != 403 {
		t.Errorf("a machine certificate: %d", w.Code)
	}
	if w := p.req(nil, "GET", "/app/main.js", "", "", nil); w.Code != 404 {
		t.Errorf("no certificate: %d", w.Code)
	}
}

var (
	refRE    = regexp.MustCompile(`(?:src|href)="([^"]+)"`)
	importRE = regexp.MustCompile(`(?m)^(?:import|export)\s[^'"\n]*?from\s*['"]([^'"]+)['"]|import\(\s*['"]([^'"]+)['"]\s*\)`)
	constRE  = regexp.MustCompile(`'(/vendor/[^']+)'`)
)

// TestAppReferences: index.html, the app's imports and the service
// worker's shell name only files that exist, and the shell lists them all.
func TestAppReferences(t *testing.T) {
	have := map[string]bool{"/": true}
	for _, p := range StaticPaths() {
		have[p] = true
	}
	index, err := fs.ReadFile(web.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range refRE.FindAllStringSubmatch(string(index), -1) {
		n++
		if !have[m[1]] {
			t.Errorf("index.html refers to %s, which is not served", m[1])
		}
	}
	if n < 6 {
		t.Errorf("index.html has %d references", n)
	}
	js, _ := fs.Glob(web.Files, "app/*.js")
	for _, f := range js {
		b, _ := fs.ReadFile(web.Files, f)
		for _, m := range append(importRE.FindAllStringSubmatch(string(b), -1), constRE.FindAllStringSubmatch(string(b), -1)...) {
			ref := m[1]
			if ref == "" {
				ref = m[2]
			}
			if strings.HasPrefix(ref, "./") {
				ref = "/app/" + ref[2:]
			}
			if !strings.HasPrefix(ref, "/") || !have[ref] {
				t.Errorf("%s imports %s, which is not served", f, m[1])
			}
		}
	}
	sw, _ := fs.ReadFile(web.Files, "sw.js")
	var shell []string
	inShell := map[string]bool{}
	for _, m := range regexp.MustCompile(`'(/[^']*)'`).FindAllStringSubmatch(string(sw), -1) {
		if have[m[1]] && m[1] != "/index.html" && !inShell[m[1]] {
			inShell[m[1]] = true
			shell = append(shell, m[1])
		}
	}
	var all []string
	for p := range have {
		if p != "/index.html" && p != "/sw.js" && p != "/icons/maskable.svg" {
			all = append(all, p)
		}
	}
	sort.Strings(all)
	sort.Strings(shell)
	if !slices.Equal(shell, all) {
		t.Errorf("the service worker's shell %v differs from the files %v", shell, all)
	}
	if strings.Contains(string(sw), "'/api") {
		t.Error("the service worker names the API")
	}
	// Every embedded file is in the repository's web directory (not, say,
	// an editor's backup).
	for p := range have {
		if p != "/" {
			if _, err := os.Stat("../../web" + p); err != nil {
				t.Errorf("%s: %v", p, err)
			}
		}
	}
}

// TestNoInlineCode scans the app for what the content security policy
// would refuse or what would defeat it: inline scripts and styles, event
// handler attributes, HTML built from strings, eval.
func TestNoInlineCode(t *testing.T) {
	htmlBad := []*regexp.Regexp{
		regexp.MustCompile(`(?i)<script(?:\s[^>]*)?>`), // checked below: needs src
		regexp.MustCompile(`(?i)<style`),
		regexp.MustCompile(`(?i)\sstyle\s*=`),
		regexp.MustCompile(`(?i)\son[a-z]+\s*=`),
		regexp.MustCompile(`(?i)javascript:`),
	}
	jsBad := []*regexp.Regexp{
		regexp.MustCompile(`innerHTML|outerHTML\s*=|insertAdjacentHTML|document\.write|dangerouslySetInnerHTML`),
		regexp.MustCompile(`\beval\s*\(|new Function\b|setTimeout\(\s*['"]`),
		regexp.MustCompile(`(?i)javascript:`),
		regexp.MustCompile(`\sstyle=["$]`),
		regexp.MustCompile(`\son[a-z]+="`),
	}
	err := fs.WalkDir(web.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, "vendor/") {
			return err
		}
		b, _ := fs.ReadFile(web.Files, p)
		s := string(b)
		switch path.Ext(p) {
		case ".html":
			for i, re := range htmlBad {
				for _, m := range re.FindAllString(s, -1) {
					if i == 0 && regexp.MustCompile(`\ssrc="`).MatchString(m) {
						continue
					}
					t.Errorf("%s: inline code %q", p, m)
				}
			}
		case ".js":
			for _, re := range jsBad {
				for _, m := range re.FindAllString(s, -1) {
					t.Errorf("%s: %q", p, m)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestWebRoutes: the endpoints of the web app need a login; the app's
// files a device certificate.
func TestWebRoutes(t *testing.T) {
	want := map[string]access{
		"GET /api/projects":                     accessUser,
		"GET /api/changes/{machine}/{session}":  accessUser,
		"GET /api/terminal/{machine}/{session}": accessUser,
		"GET /{$}":                              accessDevice,
		"GET /app/{file...}":                    accessDevice,
		"GET /vendor/{file...}":                 accessDevice,
		"GET /sw.js":                            accessDevice,
		"GET /manifest.webmanifest":             accessDevice,
		"GET /icons/{file...}":                  accessDevice,
	}
	for _, rt := range routeTable() {
		k := rt.method + " " + rt.path
		if a, ok := want[k]; ok {
			if rt.access != a {
				t.Errorf("%s: access %d, want %d", k, rt.access, a)
			}
			if rt.xsite != xsiteRead && k != "GET /{$}" {
				t.Errorf("%s: cross-site rule %d", k, rt.xsite)
			}
			delete(want, k)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing routes: %v", want)
	}
}

// webMachine is a fake machine with the web app's methods.
type webMachine struct {
	*fakeMachine
	mu       sync.Mutex
	changes  []proto.ChangesParams
	opens    []proto.TerminalOpenParams
	inputs   []string
	closes   []string
	confirms []string
	scope    string // answer with this scope (default: the asked one)
	// openAnswer, when set, replaces the answer to terminal.open.
	openAnswer func(p proto.TerminalOpenParams) (any, error)
	// noConfirm leaves terminal.confirm out, as a machine older than it;
	// reply is the hub's answer to the hello.
	noConfirm bool
	reply     proto.HubHelloReply
}

func (w *webMachine) handlers() map[string]proto.Handler {
	hs := w.fakeMachine.handlers()
	hs[proto.MethodProjectsList] = func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
		return proto.ProjectsListResult{Projects: []proto.ProjectView{
			{Ref: "demo/hello", Realm: "demo", Project: "hello", Box: "running", DefaultBranch: "main",
				Harnesses: []proto.HarnessView{{ID: "fake", Ready: true}, {ID: "Bad Name"}},
				Worktrees: []proto.WorktreeView{{Name: "main", Branch: "main", Head: "abc123", Origin: &proto.Divergence{Ref: "origin/main", Ahead: 1}},
					{Name: "../x", Branch: "x"}, {Name: "neg", Upstream: &proto.Divergence{Ahead: -1}}}},
			{Ref: "demo/other", Realm: "demo", Project: "nope", Box: "running"},
			{Ref: "demo/b", Realm: "demo", Project: "b", Box: "weird"},
		}}, nil
	}
	hs[proto.MethodChangesGet] = func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
		var p proto.ChangesParams
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		w.mu.Lock()
		w.changes = append(w.changes, p)
		scope := w.scope
		w.mu.Unlock()
		if scope == "" {
			scope = p.Scope
		}
		return proto.ChangesResult{Scope: scope, Worktree: "main", Branch: "main", Base: "aaaa", Head: "bbbb", BaseLabel: "main\x1b[31m",
			Files: []proto.ChangedFile{{Path: "a.go\x07", Added: 2, Deleted: 1}}, Added: 2, Deleted: 1, Diff: "+x\x1b[2J\n-y\n"}, nil
	}
	hs[proto.MethodTerminalOpen] = func(_ context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
		var p proto.TerminalOpenParams
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		w.mu.Lock()
		w.opens = append(w.opens, p)
		answer := w.openAnswer
		w.mu.Unlock()
		if answer != nil {
			return answer(p)
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			c.Notify(proto.NotifyTerminalData, proto.TerminalData{Stream: p.Stream, Data: []byte("hello \x1b[1mpane\x1b[0m")})
			c.Notify(proto.NotifyTerminalData, proto.TerminalData{Stream: strings.Repeat("f", 32), Data: []byte("not yours")})
			c.Notify(proto.NotifyTerminalSize, proto.TerminalSize{Stream: p.Stream, Cols: 100, Rows: 30})
		}()
		return proto.TerminalOpenResult{Stream: p.Stream, Mode: p.Mode, Cols: 80, Rows: 24}, nil
	}
	hs[proto.MethodTerminalInput] = func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
		var p proto.TerminalInputParams
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		w.mu.Lock()
		w.inputs = append(w.inputs, p.Data)
		w.mu.Unlock()
		return struct{}{}, nil
	}
	hs[proto.MethodTerminalClose] = func(_ context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
		var p proto.TerminalStreamParams
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		w.mu.Lock()
		w.closes = append(w.closes, p.Stream)
		w.mu.Unlock()
		return struct{}{}, nil
	}
	hs[proto.MethodTerminalConfirm] = func(_ context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
		var p proto.TerminalStreamParams
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		w.mu.Lock()
		w.confirms = append(w.confirms, p.Stream)
		w.mu.Unlock()
		return struct{}{}, nil
	}
	if w.noConfirm {
		delete(hs, proto.MethodTerminalConfirm)
	}
	return hs
}

// TestTerminalOpenCleanup: the hub confirms a stream whose open it
// accepted, and sends terminal.close for one whose open failed, whose
// answer it rejected or whose answer did not come in time (finding
// G5-06). Control: a valid open is confirmed and closed once the page goes.
func TestTerminalOpenCleanup(t *testing.T) {
	oldWait := termOpenWait
	termOpenWait = 300 * time.Millisecond
	t.Cleanup(func() { termOpenWait = oldWait })
	h := newTestHub(t, "hub.test")
	mach := h.issue(RoleMachine, "m1")
	dev := h.issue(RoleDevice, "phone")
	addr, _ := runHub(t, h)
	api := newAPIClient(t, addr, dev)
	m := &webMachine{fakeMachine: newFakeMachine(t)}
	sid := "s0000000000000000000a"
	m.add(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
	m.connect(addr, mach)
	waitFor(t, "the machine online", func() bool {
		code, body := api.do("GET", "/api/machines", "")
		return code == 200 && strings.Contains(body, `"online":true`)
	})
	release := make(chan struct{})
	defer close(release)
	for name, answer := range map[string]func(proto.TerminalOpenParams) (any, error){
		"refused": func(proto.TerminalOpenParams) (any, error) {
			return nil, proto.Errorf(proto.CodeBusy, "too many terminals are open")
		},
		"invalid answer": func(p proto.TerminalOpenParams) (any, error) {
			return proto.TerminalOpenResult{Stream: strings.Repeat("e", 32), Mode: p.Mode, Cols: 80, Rows: 24}, nil
		},
		"late answer": func(p proto.TerminalOpenParams) (any, error) {
			<-release
			return proto.TerminalOpenResult{Stream: p.Stream, Mode: p.Mode, Cols: 80, Rows: 24}, nil
		},
	} {
		m.mu.Lock()
		m.openAnswer, m.opens, m.closes, m.confirms = answer, nil, nil, nil
		m.mu.Unlock()
		ws, _, err := api.terminal(sid, "observe")
		if err != nil {
			t.Fatal(err)
		}
		if evs := readTerm(t, ws, 1); evs[0].text["type"] != "end" {
			t.Errorf("%s: %+v", name, evs)
		}
		ws.CloseNow()
		waitFor(t, name+": the machine's stream closed", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.closes) == 1 })
		m.mu.Lock()
		if len(m.opens) != 1 || m.closes[0] != m.opens[0].Stream || len(m.confirms) != 0 {
			t.Errorf("%s: opens %+v closes %q confirms %q", name, m.opens, m.closes, m.confirms)
		}
		m.mu.Unlock()
	}
	// Control: a valid open is confirmed, and closed when the page goes.
	m.mu.Lock()
	m.openAnswer, m.opens, m.closes, m.confirms = nil, nil, nil, nil
	m.mu.Unlock()
	ws, _, err := api.terminal(sid, "observe")
	if err != nil {
		t.Fatal(err)
	}
	if evs := readTerm(t, ws, 1); evs[0].text["type"] != "open" {
		t.Errorf("valid open: %+v", evs)
	}
	m.mu.Lock()
	if len(m.confirms) != 1 || m.confirms[0] != m.opens[0].Stream || len(m.closes) != 0 {
		t.Errorf("valid open: confirms %q closes %q", m.confirms, m.closes)
	}
	m.mu.Unlock()
	ws.CloseNow()
	waitFor(t, "the valid stream closed", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.closes) == 1 })
}

func (w *webMachine) connect(addr string, cert *Issued) {
	w.t.Helper()
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, "ws://"+addr+"/machine", &websocket.DialOptions{Host: "hub.test", HTTPHeader: http.Header{ProxyHeader: {proxyHeader(cert)}}})
	if err != nil {
		w.t.Fatal(err)
	}
	c := proto.NewConn(wsconn.New(ctx, ws), proto.Options{Handlers: w.handlers(), Workers: 4})
	w.fakeMachine.mu.Lock()
	w.fakeMachine.conn = c
	w.fakeMachine.mu.Unlock()
	var reply proto.HubHelloReply
	if err := c.Call(ctx, proto.MethodHubHello, proto.HubHello{Protocol: proto.HubProtocol, Name: "fakehost", Version: "v", Instance: "abcd"}, &reply); err != nil {
		w.t.Fatal(err)
	}
	w.mu.Lock()
	w.reply = reply
	w.mu.Unlock()
	w.t.Cleanup(func() { c.Close() })
}

// TestTerminalOlderMachine: the hub announces in its hello that it
// confirms terminals, and confirms every one it opens; a machine older
// than terminal.confirm answers "unknown method", and its terminal opens
// as before instead of ending (finding H3-01). The control, a machine
// that serves the method, is TestTerminalOpenCleanup's.
func TestTerminalOlderMachine(t *testing.T) {
	h := newTestHub(t, "hub.test")
	mach := h.issue(RoleMachine, "m1")
	dev := h.issue(RoleDevice, "phone")
	addr, _ := runHub(t, h)
	api := newAPIClient(t, addr, dev)
	m := &webMachine{fakeMachine: newFakeMachine(t), noConfirm: true}
	sid := "s0000000000000000000a"
	m.add(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
	m.connect(addr, mach)
	if !slices.Contains(m.reply.Features, proto.FeatureTerminalConfirm) {
		t.Errorf("the hub's hello does not announce terminal confirmation: %+v", m.reply)
	}
	waitFor(t, "the machine online", func() bool {
		code, body := api.do("GET", "/api/machines", "")
		return code == 200 && strings.Contains(body, `"online":true`)
	})
	ws, _, err := api.terminal(sid, "observe")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if evs := readTerm(t, ws, 1); evs[0].text["type"] != "open" {
		t.Errorf("a terminal on an older machine: %+v", evs)
	}
	m.mu.Lock()
	if len(m.opens) != 1 || len(m.closes) != 0 {
		t.Errorf("opens %+v closes %q", m.opens, m.closes)
	}
	m.mu.Unlock()
}

// terminal opens a terminal WebSocket as the web app would.
func (a apiClient) terminal(sid, mode string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{ProxyHeader: {proxyHeader(a.cert)}, "Origin": {"https://hub.test"}}
	h.Add("Cookie", (&http.Cookie{Name: SessionCookie, Value: a.cookie}).String())
	return websocket.Dial(context.Background(), "ws://"+a.addr+"/api/terminal/m1/"+sid+"?mode="+mode, &websocket.DialOptions{Host: "hub.test", HTTPHeader: h})
}

type termEvent struct {
	text   map[string]any
	binary string
}

func readTerm(t *testing.T, ws *websocket.Conn, n int) []termEvent {
	t.Helper()
	var out []termEvent
	for len(out) < n {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		typ, b, err := ws.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("terminal read after %v: %v", out, err)
		}
		if typ == websocket.MessageBinary {
			out = append(out, termEvent{binary: string(b)})
			continue
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		if m["type"] == "ping" {
			continue
		}
		out = append(out, termEvent{text: m})
	}
	return out
}

func TestWebEndpoints(t *testing.T) {
	h := newTestHub(t, "hub.test")
	mach := h.issue(RoleMachine, "m1")
	dev := h.issue(RoleDevice, "phone")
	addr, srv := runHub(t, h)
	api := newAPIClient(t, addr, dev)

	// Before the machine connects: listed offline; changes refused.
	m := &webMachine{fakeMachine: newFakeMachine(t)}
	sid := "s0000000000000000000a"
	m.add(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
	m.connect(addr, mach)
	waitFor(t, "the machine online", func() bool {
		code, body := api.do("GET", "/api/machines", "")
		return code == 200 && strings.Contains(body, `"online":true`)
	})

	// Projects: invalid entries are dropped.
	code, body := api.do("GET", "/api/projects", "")
	var pr struct{ Machines []MachineProjects }
	decodeJSON(t, body, &pr)
	if code != 200 || len(pr.Machines) != 1 || !pr.Machines[0].Online || len(pr.Machines[0].Projects) != 1 {
		t.Fatalf("projects: %d %s", code, body)
	}
	p := pr.Machines[0].Projects[0]
	if p.Ref != "demo/hello" || len(p.Harnesses) != 1 || len(p.Worktrees) != 1 || p.Worktrees[0].Origin.Ahead != 1 {
		t.Errorf("project %+v", p)
	}
	for q, want := range map[string]int{"?machine=m1": 200, "?machine=m9": 404, "?machine=Bad!": 400} {
		if code, body := api.do("GET", "/api/projects"+q, ""); code != want {
			t.Errorf("projects%s: %d %s", q, code, body)
		}
	}

	// Changes: forwarded with the scope, texts made printable.
	code, body = api.do("GET", "/api/changes/m1/"+sid+"?scope=branch", "")
	var ch ChangesReply
	decodeJSON(t, body, &ch)
	if code != 200 || ch.Scope != "branch" || ch.Machine != "m1" || len(ch.Files) != 1 || strings.ContainsAny(ch.Files[0].Path+ch.BaseLabel, "\x07\x1b") ||
		strings.Contains(ch.Diff, "\x1b") || !strings.Contains(ch.Diff, "+x") {
		t.Errorf("changes: %d %s", code, body)
	}
	if code, _ := api.do("GET", "/api/changes/m1/"+sid, ""); code != 200 {
		t.Errorf("changes without scope: %d", code)
	}
	m.mu.Lock()
	if len(m.changes) != 2 || m.changes[0] != (proto.ChangesParams{Session: sid, Ref: "demo/hello", Scope: "branch"}) || m.changes[1].Scope != proto.ScopeSnapshot {
		t.Errorf("changes asked: %+v", m.changes)
	}
	m.scope = "snapshot"
	m.mu.Unlock()
	if code, _ := api.do("GET", "/api/changes/m1/"+sid+"?scope=branch", ""); code != 502 {
		t.Errorf("an answer for another scope: %d", code)
	}
	for _, q := range []string{"/api/changes/m1/" + sid + "?scope=all", "/api/changes/m1/x"} {
		if code, _ := api.do("GET", q, ""); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	if code, _ := api.do("GET", "/api/changes/m2/"+sid, ""); code != 404 {
		t.Errorf("changes of an unknown machine: %d", code)
	}

	// The terminal observes: output and size arrive, other streams' data
	// does not, input ends the stream.
	ws, _, err := api.terminal(sid, "observe")
	if err != nil {
		t.Fatal(err)
	}
	evs := readTerm(t, ws, 3)
	if evs[0].text["type"] != "open" || evs[0].text["cols"] != 80.0 || evs[1].binary != "hello \x1b[1mpane\x1b[0m" || evs[2].text["type"] != "size" || evs[2].text["cols"] != 100.0 {
		t.Errorf("observing: %+v", evs)
	}
	ws.Write(context.Background(), websocket.MessageText, []byte(`{"type":"input","data":"x"}`))
	if evs := readTerm(t, ws, 1); evs[0].text["type"] != "end" || !strings.Contains(evs[0].text["reason"].(string), "only observes") {
		t.Errorf("input while observing: %+v", evs)
	}
	ws.CloseNow()
	waitFor(t, "the machine's stream closed", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.closes) == 1 })
	m.mu.Lock()
	if len(m.inputs) != 0 || len(m.opens) != 1 || m.opens[0].Mode != "observe" || m.opens[0].Stream != m.closes[0] || !proto.ValidStreamID(m.opens[0].Stream) {
		t.Errorf("machine saw opens %+v inputs %q closes %q", m.opens, m.inputs, m.closes)
	}
	m.mu.Unlock()

	// Control: refused unless the hub allows it.
	if _, resp, err := api.terminal(sid, "control"); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Errorf("control without permission: %v %v", resp, err)
	}
	if code, body := api.do("GET", "/api/me", ""); code != 200 || !strings.Contains(body, `"terminal_control":false`) {
		t.Errorf("me: %s", body)
	}
	srv.cfg.Terminal.Control = true
	if code, body := api.do("GET", "/api/me", ""); code != 200 || !strings.Contains(body, `"terminal_control":true`) {
		t.Errorf("me: %s", body)
	}
	termHidden, termPing = 200*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { termHidden, termPing = 30*time.Minute, 25*time.Second })
	ws, _, err = api.terminal(sid, "control")
	if err != nil {
		t.Fatal(err)
	}
	ws.Write(context.Background(), websocket.MessageText, []byte(`{"type":"visible","visible":true}`))
	readTerm(t, ws, 3)
	ws.Write(context.Background(), websocket.MessageText, []byte(`{"type":"input","data":"ls\r"}`))
	waitFor(t, "the input on the machine", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return slices.Equal(m.inputs, []string{"ls\r"}) })

	// Hidden for too long: the stream ends.
	ws.Write(context.Background(), websocket.MessageText, []byte(`{"type":"visible","visible":false}`))
	if evs := readTerm(t, ws, 1); evs[0].text["type"] != "end" || !strings.Contains(evs[0].text["reason"].(string), "not visible") {
		t.Errorf("hidden: %+v", evs)
	}
	ws.CloseNow()

	// A page that never says it is visible (hidden while connecting) is
	// hidden from the start: the deadline applies (finding C2-17).
	ws, _, err = api.terminal(sid, "observe")
	if err != nil {
		t.Fatal(err)
	}
	if evs := readTerm(t, ws, 4); evs[3].text["type"] != "end" || !strings.Contains(evs[3].text["reason"].(string), "not visible") {
		t.Errorf("never visible: %+v", evs)
	}
	ws.CloseNow()

	// The machine ends a stream.
	termHidden = 30 * time.Minute
	ws, _, err = api.terminal(sid, "observe")
	if err != nil {
		t.Fatal(err)
	}
	readTerm(t, ws, 3)
	m.mu.Lock()
	last := m.opens[len(m.opens)-1].Stream
	m.mu.Unlock()
	m.fakeMachine.conn.Notify(proto.NotifyTerminalEnd, proto.TerminalEnd{Stream: last, Reason: "the session ended"})
	if evs := readTerm(t, ws, 1); evs[0].text["type"] != "end" || evs[0].text["reason"] != "the session ended" {
		t.Errorf("ended by the machine: %+v", evs)
	}
	ws.CloseNow()

	// A browser that cannot keep up loses its stream, the link stays.
	k := &termSink{ch: make(chan termMsg, 2), done: make(chan struct{})}
	for i := 0; i < 3; i++ {
		k.put(termMsg{data: []byte("x")})
	}
	select {
	case <-k.done:
	default:
		t.Error("a full sink did not fail")
	}
}

// TestDevMode: the development mode starts only with DEVC_DEV=1 on a
// loopback address, serves invented sessions in every inbox section
// without authentication, keeps the Host check and removes its directory.
func TestDevMode(t *testing.T) {
	t.Setenv(DevEnv, "")
	if _, err := New(Options{Listen: "127.0.0.1:0", DevNoAuth: true}); err == nil || !strings.Contains(err.Error(), DevEnv) {
		t.Errorf("without %s: %v", DevEnv, err)
	}
	t.Setenv(DevEnv, "1")
	if _, err := New(Options{Listen: "0.0.0.0:0", DevNoAuth: true}); err == nil {
		t.Error("a development hub on every address")
	}
	ready := make(chan string, 1)
	s, err := New(Options{Listen: "127.0.0.1:0", DevNoAuth: true, Ready: func(a string) { ready <- a }})
	if err != nil {
		t.Fatal(err)
	}
	dir := s.o.Dir
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	addr := <-ready
	get := func(p, host string) (int, string) {
		req, _ := http.NewRequest("GET", "http://"+addr+p, nil)
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := new(strings.Builder)
		buf := make([]byte, 64<<10)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, b.String()
	}
	waitFor(t, "sessions in every section", func() bool {
		code, body := get("/api/inbox", "")
		var in struct{ Sections []InboxSection }
		if code != 200 || json.Unmarshal([]byte(body), &in) != nil {
			return false
		}
		for _, sec := range in.Sections {
			if len(sec.Sessions) == 0 {
				return false
			}
		}
		return true
	})
	waitFor(t, "an offline machine", func() bool {
		_, body := get("/api/machines", "")
		return strings.Contains(body, `"name":"old-server","online":false`) || strings.Contains(body, `"online":false`)
	})
	if code, body := get("/", ""); code != 200 || !strings.Contains(body, "/app/main.js") {
		t.Errorf("the app: %d", code)
	}
	if code, body := get("/api/me", ""); code != 200 || !strings.Contains(body, `"dev":true`) {
		t.Errorf("me: %d %s", code, body)
	}
	if code, _ := get("/api/inbox", "evil.example"); code != http.StatusMisdirectedRequest {
		t.Errorf("another Host: %d", code)
	}
	if code, _ := get("/machine", ""); code != 403 {
		t.Errorf("/machine: %d", code)
	}
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the development directory %s is still there: %v", dir, err)
	}
}
