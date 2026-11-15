package execx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFakeMatching(t *testing.T) {
	f := NewFake()
	f.On("podman").Stdout("generic")
	f.On("podman", "info").Stdout("info")
	f.On("podman", "info", "--format", "json").Stdout("json")
	f.On("git", "--version").NotFound()
	f.On("podman", "inspect").Exit(125).Stderr("Error: no such container x\nmore")
	f.On("podman", "start").Stdout("once").Times(1)

	ctx := context.Background()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"ps"}, "generic"},
		{[]string{"info"}, "info"},
		{[]string{"info", "--format", "json"}, "json"},
		{[]string{"info", "--format", "text"}, "info"},
	} {
		res, err := f.Run(ctx, Cmd{Name: "podman", Args: tc.args})
		if err != nil || string(res.Stdout) != tc.want {
			t.Errorf("podman %v = %q, %v; want %q", tc.args, res.Stdout, err, tc.want)
		}
	}

	_, err := f.Run(ctx, Cmd{Name: "git", Args: []string{"--version"}})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("git: err = %v, want ErrNotFound", err)
	}

	res, err := f.Run(ctx, Cmd{Name: "podman", Args: []string{"inspect", "x"}})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Result.ExitCode != 125 || res.ExitCode != 125 {
		t.Fatalf("inspect: %v %+v", err, res)
	}
	if !strings.Contains(err.Error(), "no such container x") || strings.Contains(err.Error(), "more") {
		t.Errorf("ExitError message = %q", err)
	}

	if res, _ := f.Run(ctx, Cmd{Name: "podman", Args: []string{"start"}}); string(res.Stdout) != "once" {
		t.Errorf("first start = %q", res.Stdout)
	}
	if res, _ := f.Run(ctx, Cmd{Name: "podman", Args: []string{"start"}}); string(res.Stdout) != "generic" {
		t.Errorf("second start = %q, want fallback to generic", res.Stdout)
	}

	if _, err := f.Run(ctx, Cmd{Name: "nft"}); err == nil || !strings.Contains(err.Error(), "unexpected command") {
		t.Errorf("unmatched: %v", err)
	}

	if !f.Called("podman", "info", "--format") || f.Called("podman", "rm") {
		t.Error("Called is wrong")
	}
	if n := len(f.Calls()); n != 9 {
		t.Errorf("recorded %d calls, want 9", n)
	}
}

func TestFakeStdinAndInteractive(t *testing.T) {
	f := NewFake()
	var seen []string
	f.On("podman", "exec").Exit(3).Do(func(c Cmd) { seen = append(seen, c.Args[1]) })
	f.On("podman", "secret").Stdout("ok")
	ctx := context.Background()
	code, err := f.Interactive(ctx, Cmd{Name: "podman", Args: []string{"exec", "box"}})
	if err != nil || code != 3 {
		t.Errorf("Interactive = %d, %v", code, err)
	}
	if _, err := f.Run(ctx, Cmd{Name: "podman", Args: []string{"secret"}, Stdin: strings.NewReader("tok")}); err != nil {
		t.Fatal(err)
	}
	if st := f.Stdins(); len(st) != 2 || st[0] != "" || st[1] != "tok" {
		t.Errorf("Stdins = %q", st)
	}
	if len(seen) != 1 || seen[0] != "box" {
		t.Errorf("Do callback saw %v", seen)
	}
}

func TestRealRun(t *testing.T) {
	ctx := context.Background()
	r := Real{}
	res, err := r.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", `printf out; printf err >&2; printf "%s" "$FOO"`}, Env: []string{"FOO=bar"}})
	if err != nil || string(res.Stdout) != "outbar" || string(res.Stderr) != "err" || res.ExitCode != 0 {
		t.Fatalf("Run = %+v, %v", res, err)
	}

	res, err = r.Run(ctx, Cmd{Name: "cat", Stdin: strings.NewReader("piped")})
	if err != nil || string(res.Stdout) != "piped" {
		t.Errorf("stdin: %q, %v", res.Stdout, err)
	}

	res, err = r.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "echo 'token s3cret rejected' >&2; exit 4"}, Redact: []string{"s3cret"}})
	var ee *ExitError
	if !errors.As(err, &ee) || res.ExitCode != 4 || ee.Result.ExitCode != 4 {
		t.Fatalf("exit: %+v, %v", res, err)
	}
	if strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("secret not redacted: %q", err)
	}

	_, err = r.Run(ctx, Cmd{Name: "devc-no-such-binary-xyz"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing binary: %v", err)
	}

	start := time.Now()
	_, err = r.Run(ctx, Cmd{Name: "sleep", Args: []string{"30"}, Timeout: 200 * time.Millisecond})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("timeout took %v", time.Since(start))
	}

	res, err = r.Run(ctx, Cmd{Name: "head", Args: []string{"-c", "5000", "/dev/zero"}, MaxOutput: 1000})
	if err != nil || len(res.Stdout) != 1000 || !res.Truncated {
		t.Errorf("cap: %d bytes, truncated %v, %v", len(res.Stdout), res.Truncated, err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Run(cctx, Cmd{Name: "true"}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: %v", err)
	}
}

// TestWorkDir: without an explicit directory every command runs in "/",
// never in the caller's current directory (which may be a project).
func TestWorkDir(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	res, err := Real{}.Run(ctx, Cmd{Name: "pwd"})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "/" {
		t.Errorf("pwd without Dir = %q, %v", res.Stdout, err)
	}
	var out strings.Builder
	if _, err := (Real{}).Interactive(ctx, Cmd{Name: "pwd", Stdin: strings.NewReader(""), Stdout: &out}); err != nil || strings.TrimSpace(out.String()) != "/" {
		t.Errorf("interactive pwd without Dir = %q, %v", out.String(), err)
	}
	dir := t.TempDir()
	res, err = Real{}.Run(ctx, Cmd{Name: "pwd", Dir: dir})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != dir {
		t.Errorf("pwd with Dir = %q, %v", res.Stdout, err)
	}
	f := NewFake()
	f.On("git")
	_, _ = f.Run(ctx, Cmd{Name: "git"})
	_, _ = f.Interactive(ctx, Cmd{Name: "git", Dir: "/srv"})
	if c := f.Calls(); c[0].Dir != "/" || c[1].Dir != "/srv" {
		t.Errorf("fake recorded directories %q, %q", c[0].Dir, c[1].Dir)
	}
}

func TestRealInteractiveExitCode(t *testing.T) {
	code, err := Real{}.Interactive(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "exit 7"}, Stdin: strings.NewReader("")})
	if err != nil || code != 7 {
		t.Errorf("Interactive = %d, %v", code, err)
	}
}

func TestRealInteractiveWriters(t *testing.T) {
	var out, errb strings.Builder
	code, err := Real{}.Interactive(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo out; echo err >&2"},
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb})
	if err != nil || code != 0 || out.String() != "out\n" || errb.String() != "err\n" {
		t.Errorf("Interactive = %d, %v, %q, %q", code, err, out.String(), errb.String())
	}
}

func TestCmdString(t *testing.T) {
	c := Cmd{Name: "podman", Args: []string{"run", "a b", "", "--env=TOKEN=abc"}, Redact: []string{"abc"}}
	if got := c.String(); got != `podman run "a b" "" --env=TOKEN=[redacted]` {
		t.Errorf("String = %s", got)
	}
}

func TestUnset(t *testing.T) {
	c := Cmd{Unset: []string{"GIT_*", "HOME"}}
	got := c.Environ([]string{"GIT_DIR=/x", "GIT_WORK_TREE=/y", "HOME=/h", "HOMER=s", "PATH=/bin", "XGIT_DIR=z"})
	if strings.Join(got, " ") != "HOMER=s PATH=/bin XGIT_DIR=z" {
		t.Errorf("%q", got)
	}
	t.Setenv("DEVC_TEST_UNSET", "visible")
	res, err := Real{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo ${DEVC_TEST_UNSET:-gone} $EXTRA"},
		Unset: []string{"DEVC_TEST_*"}, Env: []string{"EXTRA=added"}})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "gone added" {
		t.Errorf("%q %v", res.Stdout, err)
	}
}
