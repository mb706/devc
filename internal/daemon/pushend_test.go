package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
)

// slowReader hands out r at about rate bytes per second.
type slowReader struct {
	r     io.Reader
	rate  int
	start time.Time
	n     int
}

func (s *slowReader) Read(b []byte) (int, error) {
	if s.start.IsZero() {
		s.start = time.Now()
	}
	if len(b) > 256<<10 {
		b = b[:256<<10]
	}
	n, err := s.r.Read(b)
	s.n += n
	if ahead := time.Duration(float64(s.n)/float64(s.rate)*float64(time.Second)) - time.Since(s.start); ahead > 0 {
		time.Sleep(ahead)
	}
	return n, err
}

// processesIn lists the running processes that work in dir: their
// working directory or command line names it (index-pack's command line
// does not; it runs in the relay).
func processesIn(dir string) []string {
	var out []string
	des, _ := os.ReadDir("/proc")
	for _, de := range des {
		if de.Name()[0] < '0' || de.Name()[0] > '9' {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + de.Name() + "/cmdline")
		if err != nil {
			continue
		}
		cwd, _ := os.Readlink("/proc/" + de.Name() + "/cwd")
		if !bytes.Contains(cmdline, []byte(dir)) && cwd != dir && !strings.HasPrefix(cwd, dir+"/") {
			continue
		}
		st, _ := os.ReadFile("/proc/" + de.Name() + "/stat")
		if i := bytes.LastIndexByte(st, ')'); i >= 0 && strings.HasPrefix(strings.TrimSpace(string(st[i+1:])), "Z") {
			continue
		}
		out = append(out, de.Name()+": "+string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})))
	}
	return out
}

// TestPushEndedDuringIndexPack: the watchdog ends a push while index-pack
// works: while it receives a large pack, and while it completes a thin
// pack (appending the base of a delta, after all input arrived). When
// runPush returns, no git process of the push runs any more, nothing of it
// is left in the relay (no quarantine, no temporary pack, no lock), the
// relay does not grow afterwards and passes fsck, and the next push
// succeeds (finding E1-16). Measured with git 2.43 before the change
// (SIGTERM to receive-pack alone): receive-pack ended at once, index-pack
// went on; while completing a thin pack it wrote the 200 MB base into a
// quarantine that devc then removed under it.
func TestPushEndedDuringIndexPack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	old := gitx.WatchEvery
	gitx.WatchEvery = 10 * time.Millisecond
	t.Cleanup(func() { gitx.WatchEvery = old })
	dir := t.TempDir()
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	git := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = cwd, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	bare := func(name string) string {
		p := filepath.Join(dir, name)
		git(dir, "init", "-q", "--bare", p)
		git(dir, "--git-dir="+p, "config", "receive.unpackLimit", "1")
		return p
	}
	// A commit with 96 MB that does not compress, and the stream of its
	// push; then a commit that changes it a little, and the stream of its
	// push to a relay that has the first (a thin pack: one delta).
	src := filepath.Join(dir, "src")
	git(dir, "init", "-q", "-b", "main", src)
	big := make([]byte, 96<<20)
	rand.Read(big)
	os.WriteFile(filepath.Join(src, "big"), big, 0o644)
	git(src, "add", "big")
	git(src, "commit", "-qm", "one")
	first := git(src, "rev-parse", "HEAD")
	base := bare("base.git")
	full := filepath.Join(dir, "stream-full")
	git(src, "push", "-q", "--receive-pack=tee "+full+" | git-receive-pack", base, "main")
	os.WriteFile(filepath.Join(src, "big"), append(big, "appended\n"...), 0o644)
	git(src, "commit", "-qam", "two")
	second := git(src, "rev-parse", "HEAD")
	scratch := filepath.Join(dir, "scratch.git")
	exec.Command("cp", "-a", base, scratch).Run()
	thin := filepath.Join(dir, "stream-thin")
	git(src, "push", "-q", "--receive-pack=tee "+thin+" | git-receive-pack", scratch, "main")

	hooks := filepath.Join(dir, "hooks")
	os.MkdirAll(hooks, 0o700)
	host := &gitx.Host{Runner: execx.Real{}, HooksDir: hooks, Background: true}
	gs := &gitx.Service{Host: host}
	d := &Daemon{freeSpace: func(string) (int64, error) { return 100 << 30, nil }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tc := range []struct {
		name   string
		relay  func() string
		stream string
		rate   int // bytes per second; 0: as fast as it goes
		want   string
	}{
		{"receiving a large pack", func() string { return bare("relay-receive.git") }, full, 16 << 20, first},
		{"completing a thin pack", func() string {
			p := filepath.Join(dir, "relay-thin.git")
			exec.Command("cp", "-a", base, p).Run()
			return p
		}, thin, 0, second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			relay := tc.relay()
			call, err := gitx.ServeCall(relay, gitx.ServiceReceivePack, hooks, nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			used, _ := gitx.RelayUsage(relay)
			f, _ := os.Open(tc.stream)
			defer f.Close()
			var in io.Reader = f
			if tc.rate > 0 {
				in = &slowReader{r: f, rate: tc.rate}
			}
			released := 0
			_, _, why, err := d.runPush(context.Background(), gs, call, io.Discard, in, 0, relay, used, used+4<<20, func() { released++ })
			if !strings.Contains(why, "git.relay_quota") {
				t.Fatalf("the push was not ended by the watchdog: %q %v", why, err)
			}
			if errors.Is(err, execx.ErrGroupLeft) || released != 1 {
				t.Errorf("runPush: %v, released %d times", err, released)
			}
			if ps := processesIn(relay); len(ps) > 0 {
				t.Errorf("git processes still run: %q", ps)
			}
			var left []string
			filepath.WalkDir(relay, func(p string, de os.DirEntry, err error) error {
				if err == nil && (strings.HasPrefix(de.Name(), gitx.QuarantinePrefix) || strings.HasPrefix(de.Name(), "incoming-") ||
					strings.HasPrefix(de.Name(), "tmp_pack_") || strings.HasPrefix(de.Name(), "tmp_idx_") || strings.HasSuffix(de.Name(), ".lock")) {
					left = append(left, p)
				}
				return nil
			})
			if len(left) > 0 {
				t.Errorf("left in the relay: %q", left)
			}
			// Nothing grows behind devc's back afterwards, and the space of
			// the ended push is free again.
			time.Sleep(time.Second)
			if ps := processesIn(relay); len(ps) > 0 {
				t.Errorf("git processes run a second later: %q", ps)
			}
			if after, _ := gitx.RelayUsage(relay); after != used {
				t.Errorf("the relay holds %d bytes after the push ended, %d before", after, used)
			}
			git(dir, "--git-dir="+relay, "fsck", "--no-progress")
			// The next push (without a budget) succeeds.
			f.Seek(0, io.SeekStart)
			code, stderr, why, err := d.runPush(context.Background(), gs, call, io.Discard, f, 0, relay, 0, 0, nil)
			if code != 0 || why != "" || err != nil {
				t.Fatalf("the next push: %d %q %q %v", code, stderr, why, err)
			}
			if got := git(dir, "--git-dir="+relay, "rev-parse", "refs/heads/main"); got != tc.want {
				t.Errorf("main in the relay: %s, want %s", got, tc.want)
			}
			git(dir, "--git-dir="+relay, "fsck", "--no-progress")
		})
	}
}

// TestPushGroupLeft: git processes of a push that do not end (the runner
// reports their group left, together with the push's own failure) keep
// the relay as the push left it: the error is returned, the quarantine
// stays, the lock is not given back and pushes to the relay are refused
// with a message saying why. Once the processes are gone, the quarantine
// is removed, the lock given back and pushes taken again, without a
// restart (finding F1-01). The control: the same push without processes
// left removes the quarantine and gives the lock back before runPush
// returns, and nothing is refused. The runner's side (real processes) is
// execx.TestKillGroupLeft.
func TestPushGroupLeft(t *testing.T) {
	dir := t.TempDir()
	relay := filepath.Join(dir, "relay.git")
	hooks := filepath.Join(dir, "hooks")
	for _, p := range []string{filepath.Join(relay, "objects"), hooks} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	quarantine := filepath.Join(relay, "objects", gitx.QuarantinePrefix+"x1")
	ref := names.ProjectRef{Realm: "r", Project: "p"}
	for _, tc := range []struct {
		name string
		left bool
	}{{"processes left", true}, {"control: none left", false}} {
		t.Run(tc.name, func(t *testing.T) {
			gone := make(chan struct{})
			failure := &execx.ExitError{Cmd: "git receive-pack", Result: execx.Result{ExitCode: 1}}
			var runErr error = failure
			if tc.left {
				runErr = errors.Join(failure, &execx.GroupLeftError{PGID: 4242, Gone: gone})
			}
			f := execx.NewFake()
			f.On("git").Do(func(execx.Cmd) {
				// receive-pack has written into a quarantine.
				os.MkdirAll(filepath.Join(quarantine, "pack"), 0o700)
				os.WriteFile(filepath.Join(quarantine, "pack", "tmp_pack_1"), []byte("pack"), 0o600)
			}).Err(runErr)
			gs := &gitx.Service{Host: &gitx.Host{Runner: f, HooksDir: hooks, Background: true}}
			d := &Daemon{freeSpace: func(string) (int64, error) { return 100 << 30, nil }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			call, err := gitx.ServeCall(relay, gitx.ServiceReceivePack, hooks, nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			var released atomic.Int32
			_, _, _, err = d.runPush(context.Background(), gs, call, io.Discard, strings.NewReader(""), 0, relay, 0, 0, func() { released.Add(1) })
			if !errors.As(err, new(*execx.ExitError)) || errors.Is(err, execx.ErrGroupLeft) != tc.left {
				t.Fatalf("runPush: %v", err)
			}
			_, qerr := os.Stat(quarantine)
			if !tc.left {
				if qerr == nil || released.Load() != 1 || d.heldRefusal(ref, relay) != "" {
					t.Errorf("control: quarantine %v, released %d, refusal %q", qerr, released.Load(), d.heldRefusal(ref, relay))
				}
				return
			}
			// While the processes run: nothing removed, nothing given
			// back, pushes refused; other relays are not affected.
			time.Sleep(200 * time.Millisecond)
			if qerr != nil || released.Load() != 0 {
				t.Errorf("while the processes run: quarantine %v, released %d", qerr, released.Load())
			}
			if msg := d.heldRefusal(ref, relay); !strings.Contains(msg, "git processes of an earlier push to r/p have not ended") {
				t.Errorf("refusal: %q", msg)
			}
			if msg := d.heldRefusal(ref, filepath.Join(dir, "other.git")); msg != "" {
				t.Errorf("another relay is refused: %q", msg)
			}
			close(gone)
			for i := 0; i < 500 && released.Load() == 0; i++ {
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := os.Stat(quarantine); !errors.Is(err, os.ErrNotExist) || released.Load() != 1 || d.heldRefusal(ref, relay) != "" {
				t.Errorf("after the processes ended: quarantine %v, released %d, refusal %q", err, released.Load(), d.heldRefusal(ref, relay))
			}
		})
	}
}
