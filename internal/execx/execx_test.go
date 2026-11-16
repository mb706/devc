package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// running reports whether pid runs (exists and is not a zombie).
func running(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(b, ')')
	f := strings.Fields(string(b[i+1:]))
	return len(f) > 0 && f[0] != "Z" && f[0] != "X"
}

func childPIDs(t *testing.T, file string) []int {
	t.Helper()
	b, _ := os.ReadFile(file)
	var out []int
	for _, l := range strings.Fields(string(b)) {
		n, err := strconv.Atoi(l)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestKillGroup: a KillGroup command that is ended takes what it started
// with it, and so does one that exits leaving children behind; both are
// gone when Run and Interactive return (finding E1-16). Without the
// option, the children survive (the control).
func TestKillGroup(t *testing.T) {
	script := `trap '' TERM; sleep 60 & echo $! >>"$1"; (trap '' TERM; sleep 60) & echo $! >>"$1"; echo started; wait`
	for _, tc := range []struct {
		name      string
		killGroup bool
		cancel    bool
		setsid    bool
	}{
		{"ended", true, true, false},
		{"ended, in its own session", true, true, true},
		{"exited, leaving children", true, false, false},
		{"without the option", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pids := filepath.Join(t.TempDir(), "pids")
			s := script
			if !tc.cancel {
				s = `sleep 60 & echo $! >>"$1"; sleep 60 & echo $! >>"$1"; exec 1>&- 2>&-`
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := Cmd{Name: "sh", Args: []string{"-c", s, "sh", pids}, KillGroup: tc.killGroup, Setsid: tc.setsid, Timeout: time.Minute}
			if tc.cancel {
				go func() {
					for i := 0; i < 500; i++ {
						if len(childPIDs(t, pids)) == 2 {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					cancel()
				}()
			}
			start := time.Now()
			_, _ = Real{}.Run(ctx, c)
			kids := childPIDs(t, pids)
			if len(kids) != 2 {
				t.Fatalf("children %v", kids)
			}
			defer func() {
				for _, k := range kids {
					syscall.Kill(k, syscall.SIGKILL)
				}
			}()
			for _, k := range kids {
				if running(k) == tc.killGroup {
					t.Errorf("child %d running: %v", k, running(k))
				}
			}
			if tc.killGroup && time.Since(start) > 15*time.Second {
				t.Errorf("Run took %s", time.Since(start))
			}
		})
	}
	// Interactive ends the group the same way.
	pids := filepath.Join(t.TempDir(), "pids")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out bytes.Buffer
	Real{}.Interactive(ctx, Cmd{Name: "sh", Args: []string{"-c", script, "sh", pids}, KillGroup: true, Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")})
	for _, k := range childPIDs(t, pids) {
		if running(k) {
			syscall.Kill(k, syscall.SIGKILL)
			t.Errorf("Interactive: child %d survived", k)
		}
	}
}

// TestKillGroupLeft: a member of a KillGroup command's group that does
// not end when it is killed and outlives its leader, while the command
// fails for another reason (an exit status; the context). The group's
// error is reported with the other failure, also by Interactive (finding
// F1-01); the leader is held (not reaped) for as long as a member runs
// and nothing is ever signalled under its ID once it is reaped (F1-U1);
// when the member has ended, the watcher reaps the leader and closes
// Gone. SIGKILL cannot be ignored, so the test has the runner send
// SIGTERM instead: a member that ignores it plays one in uninterruptible
// I/O. The control: the same command with a member that does not ignore
// it is reported with its exit status alone and is gone, with the leader
// reaped, when Run returns.
func TestKillGroupLeft(t *testing.T) {
	oldWait, oldSignal := groupWait, signalGroup
	t.Cleanup(func() { groupWait, signalGroup = oldWait, oldSignal })
	groupWait = 500 * time.Millisecond
	var mu sync.Mutex
	var late []string
	signalGroup = func(pgid int, sig syscall.Signal) error {
		// The group's ID is the leader's: it must still be there,
		// running or a zombie, whenever the group is signalled.
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pgid)); err != nil {
			mu.Lock()
			late = append(late, fmt.Sprintf("%v to %d", sig, pgid))
			mu.Unlock()
			return nil
		}
		if sig == syscall.SIGKILL {
			sig = syscall.SIGTERM
		}
		return syscall.Kill(-pgid, sig)
	}
	state := func(pid int) string {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return "reaped"
		}
		f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
		return f[0]
	}
	// The leader writes its own ID; the member, which keeps standard
	// output open, writes its own once it ignores the signal, and the
	// leader goes on only then.
	const leave = `echo $$ >>"$1"; sh -c 'trap "" TERM; echo $$ >>"$1"; exec sleep 60' sh "$1" & ` +
		`while [ "$(wc -l <"$1")" -lt 2 ]; do sleep 0.01; done; `
	for _, tc := range []struct {
		name        string
		script      string
		interactive bool
		cancel      bool
		left        bool
	}{
		{"exit status", leave + `exit 3`, false, false, true},
		{"exit status, Interactive", leave + `exit 3`, true, false, true},
		{"ended by the context", leave + `exec sleep 60`, false, true, true},
		{"ended by the context, Interactive", leave + `exec sleep 60`, true, true, true},
		{"control: the member ends", `echo $$ >>"$1"; sleep 60 & echo $! >>"$1"; exit 3`, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			late = nil
			mu.Unlock()
			pids := filepath.Join(t.TempDir(), "pids")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				go func() {
					for i := 0; i < 500 && len(childPIDs(t, pids)) < 2; i++ {
						time.Sleep(10 * time.Millisecond)
					}
					cancel()
				}()
			}
			c := Cmd{Name: "sh", Args: []string{"-c", tc.script, "sh", pids}, KillGroup: true, Timeout: time.Minute}
			start := time.Now()
			var err error
			code := -2
			if tc.interactive {
				c.Stdin, c.Stdout, c.Stderr = strings.NewReader(""), io.Discard, io.Discard
				code, err = Real{}.Interactive(ctx, c)
			} else {
				var res Result
				res, err = Real{}.Run(ctx, c)
				code = res.ExitCode
			}
			took := time.Since(start)
			ids := childPIDs(t, pids)
			if len(ids) != 2 {
				t.Fatalf("pids %v", ids)
			}
			leader, member := ids[0], ids[1]
			defer syscall.Kill(member, syscall.SIGKILL)
			if took > 10*time.Second {
				t.Errorf("returned after %s", took)
			}
			// The other failure is reported.
			wantCode := 3
			if tc.cancel {
				wantCode = -1
				if !errors.Is(err, context.Canceled) {
					t.Errorf("the cancellation is not reported: %v", err)
				}
			} else if !tc.interactive {
				var ee *ExitError
				if !errors.As(err, &ee) || ee.Result.ExitCode != 3 {
					t.Errorf("the exit status is not reported: %v", err)
				}
			}
			if code != wantCode {
				t.Errorf("exit code %d, want %d", code, wantCode)
			}
			var gl *GroupLeftError
			if !tc.left {
				if errors.Is(err, ErrGroupLeft) || errors.As(err, &gl) {
					t.Errorf("control: %v", err)
				}
				if running(member) || state(leader) != "reaped" {
					t.Errorf("control: member %s, leader %s", state(member), state(leader))
				}
				return
			}
			if !errors.Is(err, ErrGroupLeft) || !errors.As(err, &gl) || gl.PGID != leader {
				t.Fatalf("the group is not reported: %v", err)
			}
			// Held while the member runs, and watched.
			time.Sleep(300 * time.Millisecond)
			select {
			case <-gl.Gone:
				t.Fatal("Gone closed while a member runs")
			default:
			}
			if !running(member) || state(leader) != "Z" {
				t.Fatalf("member %s, leader %s (want running and a zombie)", state(member), state(leader))
			}
			syscall.Kill(member, syscall.SIGKILL)
			select {
			case <-gl.Gone:
			case <-time.After(10 * time.Second):
				t.Fatal("Gone not closed after the member ended")
			}
			if s := state(leader); s != "reaped" {
				t.Errorf("leader after Gone: %s", s)
			}
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if len(late) > 0 {
				t.Errorf("signalled after the leader was reaped: %q", late)
			}
		})
	}
}
