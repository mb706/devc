//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// termConn is the web app's side of a terminal stream.
type termConn struct {
	t  *testing.T
	ws *websocket.Conn
	// out collects the pane's output; open and end the control messages.
	out  strings.Builder
	open map[string]any
	end  string
}

func (h *hubClient) terminal(t *testing.T, machine, sid, mode string) (*termConn, int) {
	t.Helper()
	hdr := http.Header{"Origin": {h.base}}
	hdr.Add("Cookie", (&http.Cookie{Name: hub.SessionCookie, Value: h.cookie}).String())
	u := "wss" + strings.TrimPrefix(h.base, "https") + "/api/terminal/" + machine + "/" + sid + "?mode=" + mode
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: h.c, HTTPHeader: hdr})
	if err != nil {
		if resp != nil {
			return nil, resp.StatusCode
		}
		t.Fatalf("terminal: %v", err)
	}
	ws.SetReadLimit(1 << 20)
	tc := &termConn{t: t, ws: ws}
	t.Cleanup(func() { ws.CloseNow() })
	return tc, 101
}

// until reads until cond holds (or the stream ends, or d passes).
func (c *termConn) until(d time.Duration, what string, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if c.end != "" || time.Now().After(deadline) {
			c.t.Fatalf("terminal: %s did not happen (end %q, output %q)", what, c.end, lastBytes(c.out.String(), 600))
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		typ, b, err := c.ws.Read(ctx)
		cancel()
		if err != nil {
			if c.end == "" {
				c.end = "closed: " + err.Error()
			}
			continue
		}
		if typ == websocket.MessageBinary {
			c.out.Write(b)
			continue
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		switch m["type"] {
		case "open":
			c.open = m
		case "end":
			c.end, _ = m["reason"].(string)
			if c.end == "" {
				c.end = "ended"
			}
		}
	}
}

func (c *termConn) send(v any) {
	b, _ := json.Marshal(v)
	if err := c.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatalf("terminal write: %v", err)
	}
}

func lastBytes(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// findChrome returns a headless Chrome for the browser check, or "".
func findChrome() string {
	if c := os.Getenv("CHROME"); c != "" {
		return c
	}
	home, _ := os.UserHomeDir()
	for _, pat := range []string{"chrome-for-testing/*/chrome-headless-shell-linux64/chrome-headless-shell", "chrome-for-testing/*/chrome-linux64/chrome"} {
		if m, _ := filepath.Glob(filepath.Join(home, ".local", "opt", pat)); len(m) > 0 {
			return m[0]
		}
	}
	for _, n := range []string{"chromium", "chromium-browser", "google-chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// browserProxy serves the hub to a browser on plain loopback HTTP: it
// presents the device certificate and the login session, and names the
// hub's origin, as the phone's browser would itself. (A headless browser
// cannot be given a client certificate.)
func browserProxy(t *testing.T, api *hubClient) string {
	target, _ := url.Parse(api.base)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = api.c.Transport
	rp.FlushInterval = -1
	direct := rp.Director
	rp.Director = func(r *http.Request) {
		direct(r)
		r.Host = target.Host
		if r.Header.Get("Origin") != "" {
			r.Header.Set("Origin", api.base)
		}
		r.Header.Del("Cookie")
		r.AddCookie(&http.Cookie{Name: hub.SessionCookie, Value: api.cookie})
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("Set-Cookie")
		return nil
	}
	srv := httptest.NewServer(rp)
	t.Cleanup(srv.Close)
	return srv.URL
}

// promptsIn counts a session's prompt events with the hash of text.
func promptsIn(t *testing.T, e *itest.Env, ref, sid, text string) int {
	n := 0
	for _, ev := range journalEvents(t, e, ref, sid) {
		var p proto.Prompt
		if ev.Type == proto.TypePrompt && json.Unmarshal(ev.Data, &p) == nil && p.SHA256 == hexSHA(text) {
			n++
		}
	}
	return n
}

// TestWebApp covers work order C12 section 6 against a real hub, machine
// daemon and fake harness: the app is served, projects list, a session
// starts in a new worktree from the hub, the terminal shows the pane (read
// only, then in control with both switches on), the changes show a diff,
// and the terminal ends with the session.
func TestWebApp(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fakeosc")
	e.Timeout = 5 * time.Minute
	up, _ := upstream(t)
	ref := itest.RealmA + "/webp"
	e.MustRun("project", "add", ref, "--from", "file://"+up, "--harness", "fakeosc")
	machine := "itw" + itest.Nonce

	port := freePort(t)
	dir := itest.InitHub(t, e, "hubW", "127.0.0.1:"+strconv.Itoa(port))
	ca := hub.Layout{Dir: dir}.CACert()
	mcert, mkey := issueCert(t, e, dir, "machine", machine)
	dcert, dkey := issueCert(t, e, dir, "device", "phone")
	setHubPassword(t, e, dir)
	hw := itest.StartHub(t, e, dir, "--listen", "127.0.0.1:"+strconv.Itoa(port))
	url := "https://127.0.0.1:" + strconv.Itoa(port)
	api := (&hubClient{t: t, base: url, c: tlsClient(t, ca, dcert, dkey)}).mustLogin()
	e.MustRun("join", url, "--cert", mcert, "--key", mkey, "--ca", ca)
	itest.StartDaemon(t, e)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	hubWait(t, 30*time.Second, "the machine online", func() bool { return api.machines()[machine].Online })

	// 1. The app is served, under the hub's policy.
	req, _ := api.request("GET", "/", nil)
	resp, err := api.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "/app/main.js") || !strings.HasPrefix(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Errorf("the app: %d %v", resp.StatusCode, resp.Header)
	}

	// 2. Projects: the box, its harness and worktree.
	project := func() proto.ProjectView {
		var pr struct{ Machines []hub.MachineProjects }
		if code, body := api.do("GET", "/api/projects?machine="+machine, nil, &pr); code != 200 {
			t.Fatalf("projects: %d %s", code, body)
		}
		for _, mp := range pr.Machines {
			for _, p := range mp.Projects {
				if p.Ref == ref {
					return p
				}
			}
		}
		t.Fatalf("no project %s in %+v", ref, pr)
		return proto.ProjectView{}
	}
	p := project()
	if p.Box != "running" || len(p.Harnesses) != 1 || p.Harnesses[0].ID != "fakeosc" || !p.Harnesses[0].Ready || p.DefaultBranch != "main" ||
		len(p.Worktrees) != 1 || p.Worktrees[0].Name != "main" || p.Worktrees[0].Branch != "main" || p.Worktrees[0].Dirty {
		t.Errorf("project: %+v", p)
	}

	// 3. A session in a new worktree, started from the hub.
	code, rec, body := api.command(machine, "start", newCommandID(), map[string]any{"ref": ref, "harness": "fakeosc", "worktree": "feat",
		"new_worktree": true, "prompt": "say started from the web"}, 60)
	if code != 200 || rec.Session == "" || rec.Outcome == proto.OutcomeRefused {
		t.Fatalf("start: %d %s", code, body)
	}
	sid := rec.Session
	hubWait(t, 60*time.Second, "the session on the hub", func() bool {
		d, ok := api.detail(machine, sid)
		return ok && d.Session.Worktree == "feat" && d.Session.State == proto.StateDone
	})
	if n := promptsIn(t, e, ref, sid, "say started from the web"); n != 1 {
		t.Errorf("the first prompt arrived %d times", n)
	}
	var last proto.ProjectView
	defer func() {
		if t.Failed() {
			t.Logf("last project: %+v", last)
		}
	}()
	hubWait(t, 30*time.Second, "the worktree in the project", func() bool {
		last = project()
		for _, w := range last.Worktrees {
			if w.Name == "feat" && w.Branch == "agent/feat" && w.Sessions == 1 {
				return true
			}
		}
		return false
	})

	// 4. The terminal observes the pane at its own size; input is refused.
	tc, st := api.terminal(t, machine, sid, "observe")
	if st != 101 {
		t.Fatalf("terminal: %d", st)
	}
	tc.until(20*time.Second, "the open message", func() bool { return tc.open != nil })
	if tc.open["mode"] != "observe" || tc.open["cols"].(float64) < 20 || tc.open["rows"].(float64) < 5 {
		t.Errorf("open: %v", tc.open)
	}
	api.command(machine, "send", newCommandID(), map[string]any{"session": sid, "text": "say visible in the terminal", "mode": "steer"}, 30)
	tc.until(30*time.Second, "the reply on the screen", func() bool { return strings.Contains(tc.out.String(), "visible in the terminal") })
	tc.send(map[string]string{"type": "input", "data": "x"})
	tc.until(10*time.Second, "the end after input", func() bool { return tc.end != "" })
	if !strings.Contains(tc.end, "only observes") {
		t.Errorf("input while observing: %q", tc.end)
	}
	if _, st := api.terminal(t, machine, sid, "control"); st != 403 {
		t.Errorf("control while the hub does not allow it: %d", st)
	}

	// 5. Control, with the hub's and the machine's switches on.
	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte("[terminal]\ncontrol = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hw.Restart()
	hubWait(t, 60*time.Second, "the machine back", func() bool { return api.machines()[machine].Online })
	if _, st := api.terminal(t, machine, sid, "control"); st != 101 {
		t.Fatalf("control on the hub: %d", st)
	}
	// The machine still refuses (the stream ends at once).
	tc, _ = api.terminal(t, machine, sid, "control")
	tc.until(20*time.Second, "the machine's refusal", func() bool { return tc.end != "" })
	if !strings.Contains(tc.end, "config.toml") {
		t.Errorf("control while the machine does not allow it: %q", tc.end)
	}
	// config.toml (from examples/config) has [terminal] control = false.
	cfg := filepath.Join(e.ConfigDir, "config.toml")
	old, _ := os.ReadFile(cfg)
	on := strings.Replace(string(old), "\ncontrol = false", "\ncontrol = true", 1)
	if on == string(old) {
		t.Fatalf("config.toml has no [terminal] control = false:\n%s", old)
	}
	e.WriteConfig("config.toml", on)
	if res := e.Run("config", "check"); strings.Contains(res.Stderr+res.Stdout, "config.toml") {
		t.Fatalf("config check: %s%s", res.Stdout, res.Stderr)
	}
	tc, _ = api.terminal(t, machine, sid, "control")
	tc.until(20*time.Second, "the open message in control mode", func() bool { return tc.open != nil })
	tc.send(map[string]string{"type": "input", "data": "say typed in the terminal\r"})
	hubWait(t, 30*time.Second, "the typed prompt", func() bool { return promptsIn(t, e, ref, sid, "say typed in the terminal") == 1 })
	tc.until(30*time.Second, "the typed prompt's reply", func() bool { return strings.Contains(tc.out.String(), "typed in the terminal") })
	tc.ws.Close(websocket.StatusNormalClosure, "")

	// 6. Changes: banked uncommitted work, and the branch against main.
	boxSh(t, e, ref, "cd "+inBox(ref)+"/feat && echo committed > c.txt && git add c.txt && git -c user.name=t -c user.email=t@x commit -qm feat && echo more >> a && echo new > u.txt")
	e.MustRun("bank", ref)
	var ch hub.ChangesReply
	if code, body := api.do("GET", "/api/changes/"+machine+"/"+sid+"?scope=snapshot", nil, &ch); code != 200 {
		t.Fatalf("changes: %d %s", code, body)
	}
	files := func() string {
		var out []string
		for _, f := range ch.Files {
			out = append(out, f.Path)
		}
		return strings.Join(out, ",")
	}
	if ch.Worktree != "feat" || ch.Branch != "agent/feat" || ch.Stale || files() != "a,u.txt" || !strings.Contains(ch.Diff, "+more") || !strings.Contains(ch.Diff, "+new") {
		t.Errorf("snapshot changes: %s %+v", files(), ch)
	}
	if code, body := api.do("GET", "/api/changes/"+machine+"/"+sid+"?scope=branch", nil, &ch); code != 200 {
		t.Fatalf("changes: %d %s", code, body)
	}
	if files() != "c.txt" || !strings.HasPrefix(ch.BaseLabel, "github/main") || !strings.Contains(ch.Diff, "+committed") {
		t.Errorf("branch changes: %s %+v", files(), ch)
	}

	// The web app itself, in a headless browser, when there is one: a
	// question answered from the session screen, a prompt sent, the
	// terminal and the changes shown (web/check.sh --url).
	if chrome := findChrome(); chrome != "" && os.Getenv("DEVC_ITEST_BROWSER") != "0" {
		api.command(machine, "send", newCommandID(), map[string]any{"session": sid, "text": "ask which colour? | red | blue", "mode": "steer"}, 30)
		hubWait(t, 30*time.Second, "the question", func() bool { return api.section(machine, sid) == hub.SectionNeedsYou })
		out := os.Getenv("DEVC_ITEST_WEB_OUT")
		if out == "" {
			out = t.TempDir()
		}
		cmd := exec.Command("bash", filepath.Join(e.Repo, "web", "check.sh"), "--url", browserProxy(t, api), "--session", machine+"/"+sid, out)
		cmd.Env = append(os.Environ(), "CHROME="+chrome)
		b, err := cmd.CombinedOutput()
		t.Logf("web/check.sh:\n%s", b)
		if err != nil {
			t.Fatalf("the browser check failed: %v", err)
		}
		var answered bool
		for _, ev := range journalEvents(t, e, ref, sid) {
			answered = answered || ev.Type == proto.TypeAnswer
		}
		if !answered || promptsIn(t, e, ref, sid, "say from the browser") != 1 {
			t.Errorf("from the browser: answered %v, prompts %d", answered, promptsIn(t, e, ref, sid, "say from the browser"))
		}
	} else {
		t.Log("no Chrome found (or DEVC_ITEST_BROWSER=0): the browser check is skipped")
	}

	// 7. The terminal ends with the session, and leaves no tmux client.
	tc, _ = api.terminal(t, machine, sid, "observe")
	tc.until(20*time.Second, "the open message", func() bool { return tc.open != nil })
	if code, rec, body := api.command(machine, "stop", newCommandID(), map[string]any{"session": sid}, 30); code != 200 || rec.Outcome == proto.OutcomeRefused {
		t.Fatalf("stop: %d %s", code, body)
	}
	tc.until(30*time.Second, "the end with the session", func() bool { return tc.end != "" })
	t.Logf("terminal ended: %s", tc.end)
	hubWait(t, 20*time.Second, "only the main tmux session left", func() bool {
		res := e.Run("run", ref, "--", "/opt/devc/bin/tmux", "-L", "devc", "list-sessions", "-F", "#{session_name}")
		return res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "devc" || strings.TrimSpace(res.Stdout) == ""
	})
}
