package svclogin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/toolbox"
)

func TestLogin(t *testing.T) {
	root := t.TempDir()
	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state"), Home: root,
		Getenv: func(k string) string { return map[string]string{"XDG_RUNTIME_DIR": filepath.Join(root, "run")}[k] }})
	if err != nil {
		t.Fatal(err)
	}
	store := secrets.NewFileStore(p)
	if err := store.Set("demo", "INTRANET_PASSWORD", []byte("hunter2-very-secret\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("demo", "INTRANET_USER", []byte("alice")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "config", "realms", "demo", "services", "intranet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "service.toml"), []byte("# definition\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "login.sh"), []byte("#!/bin/sh\necho login\n"), 0o755)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644)
	s := &config.Service{Name: "intranet", Realm: "demo", Dir: dir, Upstream: "https://intranet.example.org", Port: 8602,
		Auth: config.ServiceAuth{Kind: "cookie", Login: []string{"sh", "login.sh"}, Refresh: "12h", Secrets: []string{"INTRANET_USER", "INTRANET_PASSWORD"}, LoginPath: "/login"}}

	eng := engine.NewFake()
	eng.Images[toolbox.InstallerImage] = engine.ImageInfo{ID: "i"}
	now := time.Unix(1_800_000_000, 0)
	output := `[{"name":"session","value":"sessionvalue-123456","domain":"intranet.example.org","path":"/","expiry":` + itoa(now.Unix()+3600) + `}]`
	var scratchSeen []string
	eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		// Only the scratch directory is mounted, with the files and secrets.
		if len(o.Mounts) != 1 || o.Mounts[0].Target != LoginDir || o.Mounts[0].ReadOnly {
			t.Errorf("mounts: %+v", o.Mounts)
		}
		src := o.Mounts[0].Source
		for _, f := range []string{"login.sh", "secrets/INTRANET_PASSWORD", "secrets/INTRANET_USER", "service.toml", ".hidden"} {
			if b, err := os.ReadFile(filepath.Join(src, f)); err == nil {
				scratchSeen = append(scratchSeen, f+"="+string(b))
			}
		}
		if fi, err := os.Stat(filepath.Join(src, "secrets", "INTRANET_PASSWORD")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("secret file mode: %v %v", fi, err)
		}
		if !o.CapDropAll || o.Network != "" || o.Image != toolbox.InstallerImage || o.Workdir != LoginDir || strings.Join(o.Cmd, " ") != "sh login.sh" {
			t.Errorf("run options: %+v", o)
		}
		env := strings.Join(o.Env, " ")
		if !strings.Contains(env, "DEVC_SERVICE_UPSTREAM=https://intranet.example.org") || !strings.Contains(env, "DEVC_LOGIN_PATH=/login") || strings.Contains(env, "hunter2") {
			t.Errorf("environment: %q", o.Env)
		}
		return execx.Result{Stdout: []byte(output), Stderr: []byte("posting the form for alice with hunter2-very-secret\n\x1b[31mok\n")}, nil
	}
	l := &Login{Engine: eng, Store: store, Paths: p, ContextDir: filepath.Join(root, "ctx"), Now: func() time.Time { return now }}
	if err := WriteState(p.ServiceStateFile("demo", "intranet"), State{NeedsLogin: true, Since: now.Add(-time.Hour), Reason: "401"}); err != nil {
		t.Fatal(err)
	}
	res, err := l.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(scratchSeen, "|")
	if !strings.Contains(joined, "login.sh=#!/bin/sh") || !strings.Contains(joined, "INTRANET_PASSWORD=hunter2-very-secret") || strings.Contains(joined, "service.toml") || strings.Contains(joined, ".hidden") {
		t.Errorf("scratch directory: %q", scratchSeen)
	}
	if strings.Contains(res.Stderr, "hunter2") || strings.Contains(res.Stderr, "\x1b") || !strings.Contains(res.Stderr, Redacted) {
		t.Errorf("stderr: %q", res.Stderr)
	}
	b, err := store.Get("demo", config.JarSecret("intranet"))
	if err != nil {
		t.Fatal(err)
	}
	jar, err := ParseJar(b, "intranet")
	if err != nil || len(jar.Cookies) != 1 || !jar.LoginAt.Equal(now) {
		t.Fatalf("stored jar: %+v %v", jar, err)
	}
	if st, _ := ReadState(p.ServiceStateFile("demo", "intranet")); st.NeedsLogin {
		t.Errorf("the login did not clear the mark: %+v", st)
	}
	// The scratch directory is gone.
	if des, _ := os.ReadDir(p.TmpDir()); len(des) != 0 {
		t.Errorf("left in %s: %v", p.TmpDir(), des)
	}

	// Failures.
	fail := func(name string, out execx.Result, err error, want string) {
		t.Helper()
		eng.RunFunc = func(engine.RunOptions) (execx.Result, error) { return out, err }
		if _, got := l.Run(context.Background(), s); got == nil || !strings.Contains(got.Error(), want) || strings.Contains(got.Error(), "hunter2") {
			t.Errorf("%s: %v, want …%s…", name, got, want)
		}
	}
	fail("exit status", execx.Result{ExitCode: 3, Stderr: []byte("wrong password hunter2-very-secret")}, &execx.ExitError{Result: execx.Result{ExitCode: 3}}, "exited with status 3")
	fail("garbage", execx.Result{Stdout: []byte("<html>")}, nil, "not a JSON array")
	fail("too much", execx.Result{Stdout: []byte(strings.Repeat("x", MaxLoginOutput+10))}, nil, "printed more than")
	fail("other domain", execx.Result{Stdout: []byte(`[{"name":"a","value":"b","domain":"evil.example"}]`)}, nil, "not for the service's host")
	store.Delete("demo", "INTRANET_USER")
	fail("missing secret", execx.Result{}, nil, "needs the secret INTRANET_USER")
	h := *s
	h.Auth = config.ServiceAuth{Kind: "header", Header: "Authorization", ValueSecret: "TOKEN"}
	if _, err := l.Run(context.Background(), &h); err == nil || !strings.Contains(err.Error(), "devc secret set demo TOKEN") {
		t.Errorf("header service: %v", err)
	}
}
