package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGUIUsage(t *testing.T) {
	for _, args := range [][]string{
		{"gui", "bogus"},
		{"gui", "up", "a/b", "c/d"},
		{"gui", "up", "--host-port", "70000"},
		{"gui", "up", "--nope"},
		{"gui", "down", "a/b", "c/d"},
		{"gui", "status", "a/b", "c/d"},
		{"gui", "build", "extra"},
		{"browser", "a/b", "c/d"},
		{"browser", "--port", "0"},
		{"browser", "--port", "65536"},
		{"browser", "--host-port", "-1"},
	} {
		code, _, errOut := run(t, nil, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
}

func TestGUIHelp(t *testing.T) {
	code, out, _ := run(t, nil, "gui", "up", "--help")
	if code != 0 || !strings.Contains(out, "DISPLAY=127.0.0.1:1") || !strings.Contains(out, "drive and read the display") ||
		!strings.Contains(out, "like exposing the hub") {
		t.Errorf("gui up --help: %d\n%s", code, out)
	}
	code, out, _ = run(t, nil, "browser", "--help")
	if code != 0 || !strings.Contains(out, "--remote-debugging-port=9222 --remote-debugging-address=127.0.0.1 --no-sandbox") ||
		!strings.Contains(out, "chrome://inspect") {
		t.Errorf("browser --help: %d\n%s", code, out)
	}
	code, out, _ = run(t, nil, "gui")
	for _, sub := range []string{"up", "down", "status", "build"} {
		if code != 0 || !strings.Contains(out, sub) {
			t.Errorf("gui: %d %s", code, out)
		}
	}
}

// TestBrowserProbeNoRedirect: the probe of a box's debugging port never
// follows a redirect, which would be fetched from the host (finding D2-05).
func TestBrowserProbeNoRedirect(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"Browser":"Target/1.0"}`)
	}))
	defer target.Close()
	for _, code := range []int{http.StatusFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/admin?action=reset", code)
		}))
		addr := strings.TrimPrefix(box.URL, "http://")
		if browserAnswers(addr) {
			t.Errorf("%d: a redirect counted as a browser", code)
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("%d: the redirect target was fetched %d times", code, n)
		}
		// Positive control: an ordinary client follows the same redirect.
		resp, err := http.Get(box.URL + "/json/version")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if hits.Swap(0) != 1 {
			t.Errorf("%d: control: the redirect was not followed by a default client", code)
		}
		box.Close()
	}
	// A browser that answers is recognised.
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"Browser":"Chrome/140.0"}`)
	}))
	defer browser.Close()
	if !browserAnswers(strings.TrimPrefix(browser.URL, "http://")) {
		t.Error("a browser was not recognised")
	}
}
