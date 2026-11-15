package hub

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// TestLoginLimits: many logins of one certificate at once check at most
// LoginFailures passwords (one attempt at a time per certificate, the lock
// decided right before the password); a login while another of the same
// certificate runs, or while the hashing queue is full, gets 429 (finding
// C2-03).
func TestLoginLimits(t *testing.T) {
	h := newTestHub(t, "hub.test")
	a := h.issue(RoleDevice, "phone")
	b := h.issue(RoleDevice, "tablet")
	c := h.issue(RoleDevice, "laptop")
	p := newProxyServer(t, h)

	const n = 20
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- p.login(a, "wrong password!", "").Code
		}()
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for code := range codes {
		count[code]++
	}
	if count[401] == 0 || count[401] > LoginFailures || count[401]+count[429] != n {
		t.Errorf("parallel wrong passwords: %v (at most %d may be checked)", count, LoginFailures)
	}
	if strings.Count(p.logText(), "outcome=\"wrong password") > LoginFailures {
		t.Errorf("more than %d passwords checked:\n%s", LoginFailures, p.logText())
	}

	// Another attempt of the same certificate in progress.
	release, ok := p.loginSlot(b.Fingerprint)
	if !ok {
		t.Fatal("no login slot")
	}
	if w := p.login(b, testPassword, ""); w.Code != 429 || !strings.Contains(w.Body.String(), "in progress") {
		t.Errorf("while another login runs: %d %s", w.Code, w.Body)
	}
	release()
	if w := p.login(b, testPassword, ""); w.Code != 200 {
		t.Errorf("after it: %d %s", w.Code, w.Body)
	}

	// The hashing queue is full.
	for i := 0; i < cap(p.hashQueue); i++ {
		p.hashQueue <- struct{}{}
	}
	if w := p.login(c, testPassword, ""); w.Code != 429 || !strings.Contains(w.Body.String(), "busy") {
		t.Errorf("hashing queue full: %d %s", w.Code, w.Body)
	}
	for i := 0; i < cap(p.hashQueue); i++ {
		<-p.hashQueue
	}
	if w := p.login(c, testPassword, ""); w.Code != 200 {
		t.Errorf("queue free again: %d %s", w.Code, w.Body)
	}
}

// rawRequest writes a request with a body that stops after part of it.
func rawRequest(t *testing.T, addr string, is *Issued, method, path string, length int, part string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: hub.test\r\n%s: %s\r\nContent-Type: application/json\r\n%s: 1\r\nOrigin: https://hub.test\r\nContent-Length: %d\r\n\r\n%s",
		method, path, ProxyHeader, proxyHeader(is), RequestHeader, length, part)
	return conn
}

// TestSlowBodies: a request body that never completes is cut off after
// bodyTimeout, and requests without a login session in progress are
// bounded (finding C2-04).
func TestSlowBodies(t *testing.T) {
	oldTimeout, oldAnon := bodyTimeout, maxAnonymous
	t.Cleanup(func() { bodyTimeout, maxAnonymous = oldTimeout, oldAnon })
	bodyTimeout, maxAnonymous = 300*time.Millisecond, 2
	h := newTestHub(t, "hub.test")
	phone := h.issue(RoleDevice, "phone")
	tablet := h.issue(RoleDevice, "tablet")
	laptop := h.issue(RoleDevice, "laptop")
	mach := h.issue(RoleMachine, "m1")
	addr, _ := runHub(t, h)

	// An unfinished login body: answered (400) once the deadline passes.
	began := time.Now()
	conn := rawRequest(t, addr, phone, "POST", "/api/login", 100, `{"pass`)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	conn.Close()
	if err != nil {
		t.Fatalf("no answer to an unfinished body: %v", err)
	}
	resp.Body.Close()
	if d := time.Since(began); resp.StatusCode != 400 || d < bodyTimeout || d > 3*time.Second {
		t.Errorf("unfinished body: %s after %s", resp.Status, d)
	}

	// A command that waits longer than the body deadline still gets its
	// answer: the deadline ends with the body.
	api := newAPIClient(t, addr, laptop)
	m := newFakeMachine(t)
	m.sendDelay = 3 * bodyTimeout
	defer m.connect(addr, mach).Close()
	waitFor(t, "the machine", func() bool { return machineOnline(t, api) })
	if code, body := api.do("POST", "/api/commands", `{"machine":"m1","type":"send","id":"123e4567-e89b-12d3-a456-426614174000","params":{"session":"s0000000000000000000a","text":"x"}}`); code != 200 {
		t.Errorf("a command that waits beyond the body deadline: %d %s", code, body)
	}

	// Two logins hold their bodies: a third request without a login
	// session is refused.
	bodyTimeout = 5 * time.Second
	c1 := rawRequest(t, addr, phone, "POST", "/api/login", 100, `{"pass`)
	defer c1.Close()
	c2 := rawRequest(t, addr, tablet, "POST", "/api/login", 100, `{"pass`)
	defer c2.Close()
	time.Sleep(300 * time.Millisecond)
	req, _ := http.NewRequest("GET", "http://"+addr+"/app/main.js", nil)
	req.Host = "hub.test"
	req.Header.Set(ProxyHeader, proxyHeader(laptop))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 429 {
		t.Errorf("a third request without a login: %s", r.Status)
	}
	// A logged-in request is not counted.
	if code, _ := api.do("GET", "/api/me", ""); code != 200 {
		t.Errorf("logged in while two logins hang: %d", code)
	}
	c1.Close()
	c2.Close()
	waitFor(t, "the slots freed", func() bool {
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
}

// TestRPCErrors: an outcome the hub cannot know is marked uncertain; a
// refusal or a busy machine is not (finding C2-11 and the busy decision of
// C12r1).
func TestRPCErrors(t *testing.T) {
	for _, c := range []struct {
		err       error
		code      int
		uncertain bool
	}{
		{errOffline, 503, true},
		{context.DeadlineExceeded, 504, true},
		{errors.New("closed"), 502, true},
		{proto.Errorf(proto.CodeBusy, "busy"), 429, false},
		{proto.Errorf(proto.CodeRefused, "no"), 403, false},
		{proto.Errorf(proto.CodeNotFound, "none"), 404, false},
	} {
		w := httptest.NewRecorder()
		writeRPCError(w, c.err)
		if w.Code != c.code || strings.Contains(w.Body.String(), `"uncertain":true`) != c.uncertain {
			t.Errorf("%v: %d %s", c.err, w.Code, w.Body)
		}
	}
}
