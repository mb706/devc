package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/svclogin"
)

// TestServiceCommands exercises devc service ls, status and log, and the
// refusals of login (work order C18). The login itself is covered by
// svclogin.TestLogin and the integration tests.
func TestServiceCommands(t *testing.T) {
	cfg := copyConfig(t)
	state := t.TempDir()
	env := map[string]string{"DEVC_CONFIG_DIR": cfg, "DEVC_STATE_DIR": state, "DEVC_RUNTIME_DIR": t.TempDir(), EnvNoDaemon: "1", "HOME": t.TempDir()}
	write := func(rel, content string) {
		p := filepath.Join(cfg, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errOut := run(t, env, "service", "ls")
	if code != 0 || !strings.Contains(errOut, "no services") {
		t.Errorf("ls without services: %d %q", code, errOut)
	}
	write("realms/demo/services/wiki/service.toml", "upstream = \"https://wiki.example.org\"\nport = 8601\n[auth]\nkind = \"header\"\nheader = \"Authorization\"\nvalue_secret = \"WIKI_TOKEN\"\n[[allow]]\nmethods = [\"GET\"]\npath = \"/api/\"\n")
	write("realms/demo/services/intranet/service.toml", "upstream = \"https://intranet.example.org\"\nport = 8602\n[auth]\nkind = \"cookie\"\nlogin = [\"sh\", \"login.sh\"]\nrefresh = \"1h\"\n[[allow]]\nmethods = [\"GET\", \"HEAD\"]\npath = \"/\"\n")
	b, _ := os.ReadFile(filepath.Join(cfg, "realms/demo/projects/hello/project.toml"))
	write("realms/demo/projects/hello/project.toml", string(b)+"\n[grants]\nservices = [\"wiki\", \"intranet\"]\n")

	code, out, errOut := run(t, env, "service", "ls")
	if code != 0 || !strings.Contains(out, "demo/wiki") || !strings.Contains(out, "no-secret") || !strings.Contains(out, "demo/intranet") ||
		!strings.Contains(out, "needs-login (there has been no login yet)") || !strings.Contains(out, "demo/hello") {
		t.Errorf("ls: %d\n%s%s", code, out, errOut)
	}
	p, err := paths.Resolve(paths.Options{ConfigDir: cfg, StateDir: state, Home: env["HOME"], Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	store := secrets.NewFileStore(p)
	store.Set("demo", "WIKI_TOKEN", []byte("Bearer xyz-token-123"))
	now := time.Now()
	j := svclogin.NewJar("intranet", []svclogin.Cookie{{Name: "s", Value: "v-cookie-123", Domain: "intranet.example.org", Path: "/", Expiry: now.Add(30 * time.Minute).Unix()}}, now)
	jb, _ := j.Marshal()
	store.Set("demo", config.JarSecret("intranet"), jb)

	var rows []serviceStatus
	code, out, _ = run(t, env, "service", "ls", "demo", "--json")
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 2 {
		t.Fatalf("ls --json: %d %s", code, out)
	}
	for _, r := range rows {
		if r.Login != "ok" {
			t.Errorf("%s: %+v", r.Name, r)
		}
		if strings.Contains(out, "xyz-token") || strings.Contains(out, "v-cookie") {
			t.Errorf("ls shows a credential: %s", out)
		}
	}
	code, out, _ = run(t, env, "service", "status", "demo", "intranet")
	if code != 0 || !strings.Contains(out, "login:     ok") || !strings.Contains(out, "valid:     until") || !strings.Contains(out, "http://127.0.0.1:8602/") || !strings.Contains(out, "GET,HEAD /") {
		t.Errorf("status: %d\n%s", code, out)
	}
	// A mark newer than the login.
	if err := svclogin.WriteState(p.ServiceStateFile("demo", "intranet"), svclogin.State{NeedsLogin: true, Since: now.Add(time.Second), Reason: "the service answered 401 Unauthorized"}); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, env, "service", "status", "demo", "intranet", "--json")
	var st serviceStatus
	if code != 1 || json.Unmarshal([]byte(out), &st) != nil || st.Login != "needs-login" || !strings.Contains(st.Detail, "401") {
		t.Errorf("status --json after a 401: %d %s", code, out)
	}
	// The unit status is still there without arguments.
	env["DEVC_SYSTEMD_USER_DIR"] = t.TempDir()
	if code, out, _ = run(t, env, "service", "status"); code != 1 || !strings.Contains(out, "not installed") {
		t.Errorf("unit status: %d %q", code, out)
	}
	if code, _, errOut = run(t, env, "service", "status", "demo"); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("status with one argument: %d %q", code, errOut)
	}
	// The log.
	l := svclogin.NewLog(p.ServiceLogFile("demo", "wiki"))
	l.Append(svclogin.LogEntry{Time: now, Box: "demo/hello", Method: "GET", Path: "/api/x", Status: 200})
	l.Append(svclogin.LogEntry{Time: now, Box: "demo/hello", Method: "POST", Path: "/api/x", Status: 403, Reason: "no rule allows it"})
	code, out, _ = run(t, env, "service", "log", "demo", "wiki")
	if code != 0 || !strings.Contains(out, "/api/x") || !strings.Contains(out, "403") || !strings.Contains(out, "no rule allows it") {
		t.Errorf("log: %d\n%s", code, out)
	}
	if code, out, _ = run(t, env, "service", "log", "demo", "wiki", "-n", "1", "--json"); code != 0 || strings.Count(out, `"status"`) != 1 {
		t.Errorf("log -n 1 --json: %d %s", code, out)
	}
	// Refusals of login.
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"service", "login", "demo", "wiki"}, 1, "needs no login"},
		{[]string{"service", "login", "demo", "nope"}, 1, "no service"},
		{[]string{"service", "login", "Demo", "wiki"}, 2, "invalid realm"},
		{[]string{"service", "login", "demo", "../x"}, 2, "invalid service name"},
		{[]string{"service", "login", "demo"}, 2, "usage"},
	} {
		if code, _, errOut := run(t, env, c.args...); code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: %d %q", c.args, code, errOut)
		}
	}
}
