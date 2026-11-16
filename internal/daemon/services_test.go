package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/svclogin"
)

const svcToken = "Bearer d43m0n-t0ken-for-wiki"

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// svcExchange sends a raw request to a service port of the in-process
// agent and returns the raw answer ("" when the port does not answer).
func svcExchange(t *testing.T, port int, req string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	io.WriteString(c, req)
	b, _ := io.ReadAll(c)
	return string(b)
}

func svcGet(port int, path string) string {
	return "GET " + path + " HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(port) + "\r\nConnection: close\r\n\r\n"
}

// TestServiceStreams runs service requests through a real channel and an
// in-process agent to a TLS upstream: the service is served only while it
// is in force, requests are checked, the credential is added and never
// comes back, cookie services need a login, and an automatic login runs
// once for concurrent requests (work order C18).
func TestServiceStreams(t *testing.T) {
	var mu sync.Mutex
	var seenAuth, seenCookie []string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		seenAuth = append(seenAuth, req.Header.Get("Authorization"))
		seenCookie = append(seenCookie, req.Header.Get("Cookie"))
		mu.Unlock()
		switch req.URL.Path {
		case "/api/unauth":
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Echo", req.Header.Get("Authorization")+" "+req.Header.Get("Cookie"))
		w.Header().Set("Set-Cookie", "fresh=from-the-upstream-1234")
		fmt.Fprintf(w, "hello %s %s", req.Host, req.URL.Path)
	}))
	defer up.Close()
	wikiPort, intraPort := freeTCPPort(t), freeTCPPort(t)
	r := newRig(t)
	svcDir := filepath.Join(r.p.ConfigDir(), "realms", "demo", "services")
	writeSvc := func(name, content string) {
		os.MkdirAll(filepath.Join(svcDir, name), 0o755)
		if err := os.WriteFile(filepath.Join(svcDir, name, "service.toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wiki := fmt.Sprintf("upstream = %q\nport = %d\n[auth]\nkind = \"header\"\nheader = \"Authorization\"\nvalue_secret = \"WIKI_TOKEN\"\n"+
		"[[allow]]\nmethods = [\"GET\"]\npath = \"/api/\"\n", up.URL, wikiPort)
	intranet := fmt.Sprintf("upstream = %q\nport = %d\n[auth]\nkind = \"cookie\"\nlogin = [\"sh\", \"login.sh\"]\nlogin_path = \"/login\"\n"+
		"[[allow]]\nmethods = [\"GET\"]\npath = \"/\"\n", up.URL, intraPort)
	writeSvc("wiki", wiki)
	writeSvc("intranet", intranet)
	project := "harnesses = [\"claude\"]\n[grants]\nservices = [\"wiki\", \"intranet\"]\n"
	r.writeProject(t, project)
	r.tweak = func(o *Options) { o.ServiceTransport = up.Client().Transport }
	r.boxUp(true)
	r.start()
	sid := newID()
	r.agentWithInput(&fakeBox{sid: sid})
	c := r.client(nil)
	r.waitChannel(c, proto.ChannelConnected)
	store := secrets.NewFileStore(r.p)

	// Without a record of the start nothing is served, not even through a
	// stream crafted in the box.
	sc := r.rawBoxStream(t, proto.MethodHostService, proto.ServiceStreamParams{Name: "wiki"})
	fmt.Fprint(sc, svcGet(wikiPort, "/api/x"))
	if b, _ := io.ReadAll(sc); !strings.Contains(string(b), "403") || !strings.Contains(string(b), "restart it") {
		t.Errorf("without a record: %s", b)
	}
	if got := svcExchange(t, wikiPort, svcGet(wikiPort, "/api/x")); got != "" {
		t.Errorf("the port answered before the start was recorded: %q", got)
	}
	r.recordStart()
	// The agent gets the ports with the next sync.
	if !waitFor(10*time.Second, func() bool { return svcExchange(t, wikiPort, svcGet(wikiPort, "/api/x")) != "" }) {
		t.Fatal("the wiki port did not open")
	}
	// The credential is not set yet.
	if got := svcExchange(t, wikiPort, svcGet(wikiPort, "/api/x")); !strings.Contains(got, "503") || !strings.Contains(got, "devc secret set demo WIKI_TOKEN") {
		t.Errorf("without the secret: %s", got)
	}
	store.Set("demo", "WIKI_TOKEN", []byte(svcToken+"\n"))
	got := svcExchange(t, wikiPort, "GET /api/page?q=1 HTTP/1.1\r\nHost: 127.0.0.1:"+strconv.Itoa(wikiPort)+"\r\nAuthorization: Bearer box-own\r\nConnection: close\r\n\r\n")
	if !strings.Contains(got, "200 OK") || !strings.Contains(got, "hello 127.0.0.1") || !strings.Contains(got, "/api/page") {
		t.Errorf("allowed request: %s", got)
	}
	mu.Lock()
	if len(seenAuth) == 0 || seenAuth[len(seenAuth)-1] != svcToken {
		t.Errorf("the upstream saw %q", seenAuth)
	}
	mu.Unlock()
	if strings.Contains(got, "d43m0n") || strings.Contains(got, "from-the-upstream") {
		t.Errorf("the credential or Set-Cookie came back: %s", got)
	}
	// Refusals.
	for _, tc := range []struct{ req, want string }{
		{"POST /api/x HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(wikiPort) + "\r\nContent-Length: 0\r\n\r\n", "no rule allows POST /api/x"},
		{svcGet(wikiPort, "/admin"), "no rule allows GET /admin"},
		{svcGet(wikiPort, "/api/../admin"), "400"},
		{svcGet(wikiPort, "/api/%2e%2e/admin"), "400"},
		{svcGet(wikiPort, "/api%2fadmin"), "encoded slash"},
		{"GET http://127.0.0.1/api/x HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(wikiPort) + "\r\n\r\n", "not origin-form"},
		{"GET /api/x HTTP/1.1\r\nHost: evil.example\r\n\r\n", "not this service's local address"},
		{"POST /api/x HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(wikiPort) + "\r\nContent-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n", "both Content-Length"},
	} {
		if got := svcExchange(t, wikiPort, tc.req); !strings.Contains(got, tc.want) || strings.Contains(got, "hello ") {
			t.Errorf("%q:\n%s", tc.req, got)
		}
	}
	// A service that is not granted, through a crafted stream.
	sc = r.rawBoxStream(t, proto.MethodHostService, proto.ServiceStreamParams{Name: "nope"})
	fmt.Fprint(sc, svcGet(wikiPort, "/api/x"))
	if b, _ := io.ReadAll(sc); !strings.Contains(string(b), "403") || !strings.Contains(string(b), "not granted") {
		t.Errorf("a service that is not granted: %s", b)
	}
	// The log has the requests, without headers.
	if !waitFor(5*time.Second, func() bool {
		list, _ := svclogin.ReadLog(r.p.ServiceLogFile("demo", "wiki"))
		return len(list) >= 5
	}) {
		t.Error("the service log is short")
	}
	raw, _ := os.ReadFile(r.p.ServiceLogFile("demo", "wiki"))
	if !strings.Contains(string(raw), `"path":"/api/page"`) || strings.Contains(string(raw), "q=1") || strings.Contains(string(raw), "d43m0n") || strings.Contains(string(raw), "box-own") {
		t.Errorf("service log:\n%s", raw)
	}

	// The cookie service: no login yet.
	r.startSession(c, sid)
	if !waitFor(10*time.Second, func() bool { return svcExchange(t, intraPort, svcGet(intraPort, "/x")) != "" }) {
		t.Fatal("the intranet port did not open")
	}
	got = svcExchange(t, intraPort, svcGet(intraPort, "/x"))
	if !strings.Contains(got, "503") || !strings.Contains(got, "devc service login demo intranet") {
		t.Errorf("without a login: %s", got)
	}
	if st, _ := svclogin.ReadState(r.p.ServiceStateFile("demo", "intranet")); !st.NeedsLogin {
		t.Errorf("state: %+v", st)
	}
	if !waitFor(10*time.Second, func() bool { return strings.Contains(strings.Join(r.journalTypes(c, sid), " "), proto.TypeNote) }) {
		t.Errorf("no note in the live session: %v", r.journalTypes(c, sid))
	}
	notes := func() int {
		evs, _ := c.Events(context.Background(), sid, 0)
		n := 0
		for _, ev := range evs.Events {
			if ev.Type == proto.TypeNote && strings.Contains(string(ev.Data), "needs a login") {
				n++
			}
		}
		return n
	}
	svcExchange(t, intraPort, svcGet(intraPort, "/y"))
	time.Sleep(300 * time.Millisecond)
	if n := notes(); n != 1 {
		t.Errorf("%d notes for one need", n)
	}
	// A login (as `devc service login` stores it).
	host := strings.Split(strings.TrimPrefix(up.URL, "https://"), ":")[0]
	storeJar := func(at time.Time, value string) {
		j := svclogin.NewJar("intranet", []svclogin.Cookie{{Name: "session", Value: value, Domain: host, Path: "/", Expiry: time.Now().Add(time.Hour).Unix()}}, at)
		b, _ := j.Marshal()
		if err := store.Set("demo", config.JarSecret("intranet"), b); err != nil {
			t.Fatal(err)
		}
	}
	storeJar(time.Now(), "c00kie-session-0001")
	got = svcExchange(t, intraPort, svcGet(intraPort, "/x"))
	if !strings.Contains(got, "200 OK") || strings.Contains(got, "c00kie") {
		t.Errorf("with a login: %s", got)
	}
	mu.Lock()
	if seenCookie[len(seenCookie)-1] != "session=c00kie-session-0001" {
		t.Errorf("the upstream saw cookies %q", seenCookie[len(seenCookie)-1])
	}
	mu.Unlock()
	// The upstream asks for a login: 503 now and for later requests.
	if got := svcExchange(t, intraPort, svcGet(intraPort, "/api/unauth")); !strings.Contains(got, "503") || !strings.Contains(got, "401") {
		t.Errorf("401 from the upstream: %s", got)
	}
	if got := svcExchange(t, intraPort, svcGet(intraPort, "/x")); !strings.Contains(got, "503") {
		t.Errorf("after the 401: %s", got)
	}
	if !waitFor(5*time.Second, func() bool { return notes() == 2 }) {
		t.Errorf("%d notes after the second need", notes())
	}

	// Automatic login: an expired jar and concurrent requests run the
	// login once.
	writeSvc("intranet", strings.Replace(intranet, "login_path", "auto = true\nlogin_path", 1))
	r.d.reload(true)
	storeJar(time.Now().Add(-13*time.Hour), "c00kie-session-0001")
	var logins atomic.Int32
	r.runner.On("/fake/devc", "service", "login").Do(func(cmd execx.Cmd) {
		logins.Add(1)
		time.Sleep(300 * time.Millisecond)
		storeJar(time.Now(), "c00kie-session-0002")
	})
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = svcExchange(t, intraPort, svcGet(intraPort, "/c/"+strconv.Itoa(i)))
		}(i)
	}
	wg.Wait()
	for i, res := range results {
		if !strings.Contains(res, "200 OK") {
			t.Errorf("concurrent request %d: %s", i, res)
		}
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("%d automatic logins", n)
	}
	if calls := r.devcCalls("login"); len(calls) != 1 || strings.Join(calls[0], " ") != "service login --quiet -- demo intranet" {
		t.Errorf("login calls: %q", calls)
	}

	// Removing the grant closes the port and refuses crafted streams.
	r.writeProject(t, "harnesses = [\"claude\"]\n[grants]\nservices = [\"intranet\"]\n")
	r.d.reload(true)
	if !waitFor(10*time.Second, func() bool { return svcExchange(t, wikiPort, svcGet(wikiPort, "/api/x")) == "" }) {
		t.Error("the wiki port still answers after the grant was removed")
	}
	sc = r.rawBoxStream(t, proto.MethodHostService, proto.ServiceStreamParams{Name: "wiki"})
	fmt.Fprint(sc, svcGet(wikiPort, "/api/x"))
	if b, _ := io.ReadAll(sc); !strings.Contains(string(b), "not granted") {
		t.Errorf("a removed grant: %s", b)
	}
	// A changed definition is not served until the box starts again.
	writeSvc("intranet", strings.Replace(intranet, `path = "/"`, `path = "/other/"`, 1))
	r.d.reload(true)
	sc = r.rawBoxStream(t, proto.MethodHostService, proto.ServiceStreamParams{Name: "intranet"})
	fmt.Fprint(sc, svcGet(intraPort, "/other/x"))
	if b, _ := io.ReadAll(sc); !strings.Contains(string(b), "changed after this box started") {
		t.Errorf("a changed definition: %s", b)
	}
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// TestServiceHostileStreams: garbage and oversized heads on service
// streams end them without disturbing the channel.
func TestServiceHostileStreams(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(filepath.Join(r.p.ConfigDir(), "realms", "demo", "services", "wiki"), 0o755)
	os.WriteFile(filepath.Join(r.p.ConfigDir(), "realms", "demo", "services", "wiki", "service.toml"),
		[]byte("upstream = \"https://127.0.0.1:1\"\nport = 18601\n[auth]\nkind = \"header\"\nheader = \"X-Key\"\nvalue_secret = \"K\"\n[[allow]]\nmethods = [\"GET\"]\npath = \"/\"\n"), 0o644)
	r.writeProject(t, "harnesses = [\"claude\"]\n[grants]\nservices = [\"wiki\"]\n")
	r.boxUp(true)
	r.start()
	r.agent(nil)
	c := r.client(nil)
	r.waitChannel(c, proto.ChannelConnected)
	r.recordStart()
	for _, raw := range []string{
		"\x00\x01\x02garbage\r\n\r\n",
		"GET / HTTP/1.1\r\nX: " + strings.Repeat("a", 70<<10) + "\r\n\r\n",
		"GET / HTTP/1.1\nHost: 127.0.0.1:18601\n\n",
	} {
		sc := r.rawBoxStream(t, proto.MethodHostService, proto.ServiceStreamParams{Name: "wiki"})
		io.WriteString(sc, raw)
		resp, err := http.ReadResponse(bufio.NewReader(sc), nil)
		if err != nil || resp.StatusCode < 400 {
			t.Errorf("%.30q: %v %v", raw, resp, err)
		}
	}
	// Invalid parameters close the stream.
	sc := r.rawBoxStream(t, proto.MethodHostService, map[string]any{"name": "../x"})
	if b, _ := io.ReadAll(sc); len(b) != 0 {
		t.Errorf("invalid name: %q", b)
	}
	r.waitChannel(c, proto.ChannelConnected)
}

// TestServiceStreamDeadlines: a box that stops reading the answer, or never
// finishes its body, holds its stream, its relay place and the handler only
// until the service's timeout and the grace; the channel stays (finding
// D2-04).
func TestServiceStreamDeadlines(t *testing.T) {
	old := serviceGrace
	serviceGrace = 500 * time.Millisecond
	t.Cleanup(func() { serviceGrace = old })
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/big":
			chunk := make([]byte, 64<<10)
			for i := 0; i < 256; i++ { // 16 MiB, far beyond every window
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		case "/api/upload":
			n, _ := io.Copy(io.Discard, req.Body)
			fmt.Fprintf(w, "got %d", n)
		default:
			fmt.Fprint(w, "fine")
		}
	}))
	defer up.Close()
	port := freeTCPPort(t)
	r := newRig(t)
	dir := filepath.Join(r.p.ConfigDir(), "realms", "demo", "services", "wiki")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "service.toml"), []byte(fmt.Sprintf("upstream = %q\nport = %d\n[auth]\nkind = \"header\"\nheader = \"Authorization\"\nvalue_secret = \"WIKI_TOKEN\"\n"+
		"[[allow]]\nmethods = [\"GET\", \"POST\"]\npath = \"/api/\"\n[limits]\ntimeout = \"1s\"\nmax_body = \"64m\"\n", up.URL, port)), 0o644)
	r.writeProject(t, "harnesses = [\"claude\"]\n[grants]\nservices = [\"wiki\"]\n")
	r.tweak = func(o *Options) { o.ServiceTransport = up.Client().Transport }
	r.boxUp(true)
	r.start()
	r.agent(nil)
	c := r.client(nil)
	r.waitChannel(c, proto.ChannelConnected)
	r.recordStart()
	secrets.NewFileStore(r.p).Set("demo", "WIKI_TOKEN", []byte(svcToken+"\n"))
	w := r.d.worker(hello)
	busy := func() int { return len(w.relays) }
	control := func(when string) {
		t.Helper()
		sc := r.rawBoxStream(t, proto.MethodHostService, proto.ServiceStreamParams{Name: "wiki"})
		fmt.Fprint(sc, svcGet(port, "/api/x"))
		if b, _ := io.ReadAll(sc); !strings.Contains(string(b), "200 OK") || !strings.Contains(string(b), "fine") {
			t.Errorf("%s: a request that is read: %q", when, b)
		}
	}
	control("before")
	if !waitFor(5*time.Second, func() bool { return busy() == 0 }) {
		t.Fatalf("relay places taken after a finished request: %d", busy())
	}
	for _, tc := range []struct{ name, req string }{
		{"stalled reader", svcGet(port, "/api/big")},
		{"unfinished body", fmt.Sprintf("POST /api/upload HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nContent-Length: 100000\r\nConnection: close\r\n\r\npartial", port)},
	} {
		start := time.Now()
		sc := r.rawBoxStream(t, proto.MethodHostService, proto.ServiceStreamParams{Name: "wiki"})
		fmt.Fprint(sc, tc.req)
		// The stream holds its place while the exchange runs …
		time.Sleep(400 * time.Millisecond)
		if busy() != 1 {
			t.Errorf("%s: %d relay places taken while it runs, want 1", tc.name, busy())
		}
		// … and gives it up after the timeout (1 s) and the grace.
		if !waitFor(10*time.Second, func() bool { return busy() == 0 }) {
			t.Errorf("%s: the relay place is still taken after %v", tc.name, time.Since(start))
		} else if el := time.Since(start); el > 4*time.Second {
			t.Errorf("%s: freed after %v", tc.name, el)
		}
		_ = sc.SetDeadline(time.Now().Add(5 * time.Second))
		b, err := io.ReadAll(sc)
		if err != nil || len(b) >= 16<<20 || strings.Contains(string(b), "got ") {
			t.Errorf("%s: the stream did not end: %d bytes, %v", tc.name, len(b), err)
		}
		// Only the stream ended: the channel is still up and serves.
		r.waitChannel(c, proto.ChannelConnected)
		control("after " + tc.name)
	}
}
