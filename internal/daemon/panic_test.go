package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/proto"
)

// lockedLog collects a daemon's log for a test that reads it while the
// daemon writes.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// panicSecret stands for what a handler could panic with.
const panicSecret = "ghs_PANICSECRET0123456789"

// TestNoBareGoroutines: every goroutine of the daemon starts through
// goTask or goSafe, and every callback the runtime runs in a goroutine of
// its own through afterFunc or contextAfter, all of which recover a panic
// (findings X6 2, H2-04): no other go statement, time.AfterFunc or
// context.AfterFunc is left in the package. This test is what keeps the
// statement on the explorer's daemon page true. Control: the checker finds
// each of the three in a source made for it.
func TestNoBareGoroutines(t *testing.T) {
	fset := token.NewFileSet()
	bad, err := parser.ParseFile(fset, "bad.go", `package daemon
var later = func() { go work() }
func f(ctx context.Context) {
	go work()
	time.AfterFunc(time.Second, work)
	context.AfterFunc(ctx, work)
	goTask(nil, nil, work)
}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := bareGoroutines(fset, "bad.go", bad); len(got) != 4 {
		t.Errorf("the checker found %d of 4 in the control:\n%s", len(got), strings.Join(got, "\n"))
	}
	files, _ := filepath.Glob("*.go")
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, problem := range bareGoroutines(fset, f, file) {
			t.Error(problem)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d files checked", checked)
	}
}

// recovering are the functions of daemon.go that start goroutines and
// callbacks under recoverTask.
var recovering = map[string]bool{"goTask": true, "goSafe": true, "afterFunc": true, "contextAfter": true}

// bareGoroutines lists the go statements and AfterFunc calls of a file
// outside the recovering functions, each with what to do instead.
func bareGoroutines(fset *token.FileSet, name string, file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			return !(name == "daemon.go" && recovering[n.Name.Name])
		case *ast.GoStmt:
			out = append(out, fmt.Sprintf("%s: a go statement outside goTask and goSafe: a panic there ends the daemon; "+
				"start it with goTask(log, &task{...}, fn) (or d.goSafe for the daemon's own loops), which recovers it", fset.Position(n.Pos())))
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AfterFunc" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && (pkg.Name == "time" || pkg.Name == "context") {
				helper := map[string]string{"time": "afterFunc(log, &task{...}, d, fn)", "context": "contextAfter(ctx, log, &task{...}, fn)"}[pkg.Name]
				out = append(out, fmt.Sprintf("%s: %s.AfterFunc runs its function in a goroutine of its own, outside goTask: a panic there ends the daemon; "+
					"use %s, which recovers it", fset.Position(n.Pos()), pkg.Name, helper))
			}
		}
		return true
	})
	return out
}

// TestBoxStreamPanic: a panic while serving a box's raw stream ends that
// stream only (finding X6 2). The daemon goes on serving, the stream's
// places are given back (more panicking streams than there are places,
// then an ordinary request), and the log names the box and the method and
// where it happened, but not what the code panicked with.
func TestBoxStreamPanic(t *testing.T) {
	var logw lockedLog
	r := newRig(t)
	r.logw = &logw
	r.runner.On("git").Do(func(c execx.Cmd) {
		if slices.Contains(c.Args, "upload-pack") {
			panic(errors.New("cannot use token " + panicSecret))
		}
	})
	relay := r.p.RelayRepo(hello)
	os.MkdirAll(relay, 0o700)
	os.WriteFile(filepath.Join(relay, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
	r.boxUp(true)
	r.start()
	r.agent(nil)
	c := r.client(nil)
	r.waitChannel(c, proto.ChannelConnected)
	for i := 0; i < maxBoxStreams+8; i++ {
		st := r.rawBoxStream(t, proto.MethodHostGit, proto.GitParams{Service: gitx.ServiceUploadPack})
		br := bufio.NewReader(st)
		line, err := br.ReadString('\n')
		if err != nil || !strings.Contains(line, `"service":"upload-pack"`) {
			t.Fatalf("stream %d: %q %v", i, line, err)
		}
		// The fake git reads its input to the end first.
		st.(*net.UnixConn).CloseWrite()
		if _, err := io.ReadAll(br); err != nil && os.IsTimeout(err) {
			t.Fatalf("stream %d did not end: %v\n%s", i, err, logw.String())
		}
		st.Close()
	}
	// The daemon and the channel still serve; the places came back.
	var pong proto.PingReply
	if err := r.boxCall(proto.MethodHostPing, nil, &pong); err != nil {
		t.Fatalf("a request after the panics: %v", err)
	}
	if err := c.Call(context.Background(), proto.MethodBoxesList, nil, nil); err != nil {
		t.Fatalf("the daemon after the panics: %v", err)
	}
	log := logw.String()
	if strings.Contains(log, panicSecret) {
		t.Errorf("the log holds what the code panicked with:\n%s", log)
	}
	if n := strings.Count(log, "panic recovered"); n != maxBoxStreams+8 ||
		!strings.Contains(log, "box=devc-demo-hello in=host.git panic=\"a value of type *errors.errorString\"") ||
		!strings.Contains(log, "serveGitProcess") {
		t.Errorf("%d panics logged:\n%s", n, log)
	}
}

// TestPushPanicReleases: a panic while a push runs gives back what the
// push holds (the box lock, through release) before the goroutine ends
// (finding X6 2). Control: a push that ends normally releases once too.
func TestPushPanicReleases(t *testing.T) {
	dir := t.TempDir()
	relay := filepath.Join(dir, "relay.git")
	hooks := filepath.Join(dir, "hooks")
	os.MkdirAll(filepath.Join(relay, "objects"), 0o700)
	os.MkdirAll(hooks, 0o700)
	for _, tc := range []struct {
		name  string
		panic bool
	}{{"panic", true}, {"control", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := execx.NewFake()
			f.On("git").Do(func(execx.Cmd) {
				if tc.panic {
					panic("index-pack: " + panicSecret)
				}
			})
			gs := &gitx.Service{Host: &gitx.Host{Runner: f, HooksDir: hooks, Background: true}}
			d := &Daemon{freeSpace: func(string) (int64, error) { return 100 << 30, nil }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			call, err := gitx.ServeCall(relay, gitx.ServiceReceivePack, hooks, nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			var released atomic.Int32
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				d.runPush(context.Background(), gs, call, io.Discard, strings.NewReader(""), 0, relay, 0, 0, func() { released.Add(1) })
				return false
			}()
			if panicked != tc.panic || released.Load() != 1 {
				t.Errorf("panicked %v, released %d times", panicked, released.Load())
			}
		})
	}
}

// TestProjectsPanic: a panic in a projects.list worker fails that request
// only, gives back the worker's place and the worktree read it started,
// and is logged without what it panicked with (finding X6 2). Control:
// the next call, without the panic, answers.
func TestProjectsPanic(t *testing.T) {
	var logw lockedLog
	r := newRig(t)
	r.logw = &logw
	var boom atomic.Bool
	boom.Store(true)
	r.eng.ExecFunc = func(o engine.ExecOptions) (execx.Result, error) {
		if slices.Contains(o.Cmd, "worktree") && boom.Load() {
			panic("worktree list: " + panicSecret)
		}
		return execx.Result{Stdout: []byte("[]\n")}, nil
	}
	r.boxUp(true)
	r.start()
	c := r.client(nil)
	for deadline := time.Now().Add(10 * time.Second); r.d.boxState(hello) != "running"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the box watch did not see the box")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var res proto.ProjectsListResult
	if err := c.Call(ctx, proto.MethodProjectsList, proto.ProjectsListParams{}, &res); !proto.IsCode(err, proto.CodeInternal) {
		t.Fatalf("projects.list with a panicking worker: %v", err)
	}
	boom.Store(false)
	if err := c.Call(ctx, proto.MethodProjectsList, proto.ProjectsListParams{}, &res); err != nil || len(res.Projects) == 0 {
		t.Fatalf("projects.list after the panic: %v %+v", err, res)
	}
	log := logw.String()
	if strings.Contains(log, panicSecret) || !strings.Contains(log, "in=projects.list") {
		t.Errorf("log:\n%s", log)
	}
}
