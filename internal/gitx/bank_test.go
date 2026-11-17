package gitx

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/paths"
)

// initWorld is a world with an initialised relay and box repository.
func initWorld(t *testing.T) (*world, string) {
	t.Helper()
	w := newWorld(t)
	ctx := context.Background()
	def, err := w.svc.InitRelay(ctx, w.p)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.InitBox(ctx, w.p, def); err != nil {
		t.Fatal(err)
	}
	return w, filepath.Join(w.box, "main")
}

// fetches returns the host git fetches recorded since call n, with the
// refspecs each read from its standard input.
func (w *world) fetches(n int) [][]string {
	var out [][]string
	for _, c := range w.rec.calls[n:] {
		if c.Name != "git" || !slices.Contains(c.Args, "fetch") || !slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "ext::") }) {
			continue
		}
		args := append([]string{}, c.Args...)
		if c.Stdin != nil {
			if sr, ok := c.Stdin.(*strings.Reader); ok {
				sr.Seek(0, 0)
				b := make([]byte, sr.Len())
				sr.Read(b)
				args = append(args, strings.Fields(string(b))...)
			}
		}
		out = append(out, args)
	}
	return out
}

// bankRefs lists the relay's bank refs and snapshot refs with their values.
func (w *world) bankRefs() string {
	return w.git(w.dir, "--git-dir="+w.p.Relay.Dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/devc/bank/", "refs/devc/snap/", "refs/heads/")
}

// leftovers lists temporary packs, quarantines and lock files in a relay.
func leftovers(relay string) []string {
	var left []string
	filepath.WalkDir(relay, func(p string, de fs.DirEntry, err error) error {
		if err == nil && (strings.HasPrefix(de.Name(), "tmp_") || strings.HasPrefix(de.Name(), QuarantinePrefix) || strings.HasSuffix(de.Name(), ".lock")) {
			left = append(left, p)
		}
		return nil
	})
	return left
}

// TestBankListsAndFetchesByName: a bank lists the box's refs first and
// fetches only those whose value the relay does not record, each by name;
// no wildcard reaches git, and a bank with nothing new fetches nothing.
// More changed branches than a bank takes, or a listing over its bound,
// refuse the bank before anything is fetched; the same box below the
// limits is banked (X7 2).
func TestBankListsAndFetchesByName(t *testing.T) {
	w, main := initWorld(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(main, "f"), []byte("2\n"), 0o644)
	w.git(main, "commit", "-qam", "c1")
	w.git(main, "branch", "b1")
	w.git(main, "tag", "v1")
	os.WriteFile(filepath.Join(main, "dirty"), []byte("x\n"), 0o644)
	n := len(w.rec.calls)
	if _, err := w.svc.Bank(ctx, w.p); err != nil {
		t.Fatal(err)
	}
	got := w.fetches(n)
	if len(got) != 1 {
		t.Fatalf("fetches: %q", got)
	}
	for _, a := range got[0] {
		if strings.Contains(a, "*") {
			t.Errorf("a wildcard reached git: %q", got[0])
		}
	}
	for _, want := range []string{"--stdin", "+refs/heads/main:refs/devc/incoming/heads/main", "+refs/heads/b1:refs/devc/incoming/heads/b1",
		"+refs/tags/v1:refs/devc/incoming/tags/v1", "+refs/devc/wip/main:refs/devc/incoming/wip/main"} {
		if !slices.Contains(got[0], want) {
			t.Errorf("the fetch lacks %q: %q", want, got[0])
		}
	}
	// Nothing new: listed, nothing fetched.
	os.Remove(filepath.Join(main, "dirty"))
	n = len(w.rec.calls)
	res, err := w.svc.Bank(ctx, w.p)
	if err != nil || len(res.Branches)+len(res.Tags)+len(res.Snapshots) != 0 {
		t.Fatalf("a bank without changes: %+v %v", res, err)
	}
	if got := w.fetches(n); len(got) != 0 {
		t.Errorf("a bank without changes fetched: %q", got)
	}
	// One changed branch: only it is fetched.
	w.git(main, "checkout", "-q", "b1")
	os.WriteFile(filepath.Join(main, "f"), []byte("3\n"), 0o644)
	w.git(main, "commit", "-qam", "c2")
	n = len(w.rec.calls)
	if res, err := w.svc.Bank(ctx, w.p); err != nil || len(res.Branches) != 1 || res.Branches[0].Branch != "b1" {
		t.Fatalf("bank of b1: %+v %v", res, err)
	}
	if got := w.fetches(n); len(got) != 1 || slices.ContainsFunc(got[0], func(a string) bool { return strings.Contains(a, "heads/main") }) {
		t.Errorf("fetches of one changed branch: %q", got)
	}

	// Over the bound on changed branches: refused, nothing fetched.
	old := maxBankFetch
	maxBankFetch = 3
	t.Cleanup(func() { maxBankFetch = old })
	var lines []string
	tip := w.git(main, "rev-parse", "HEAD~1")
	for i := range 4 {
		lines = append(lines, fmt.Sprintf("create refs/heads/many%d %s", i, tip))
	}
	w.gitIn(main, strings.Join(lines, "\n")+"\n", "update-ref", "--stdin")
	before := w.bankRefs()
	n = len(w.rec.calls)
	_, err = w.svc.Bank(ctx, w.p)
	var np *NotPreservedError
	if !errors.As(err, &np) || !np.All || !strings.Contains(err.Error(), "4 changed branches, more than the 3 a bank takes") {
		t.Errorf("more changed branches than a bank takes: %v", err)
	}
	if got := w.fetches(n); len(got) != 0 {
		t.Errorf("a refused bank fetched: %q", got)
	}
	if after := w.bankRefs(); after != before {
		t.Errorf("a refused bank changed the relay:\n%s\n%s", before, after)
	}
	// The control: three changed branches are banked.
	w.git(main, "update-ref", "-d", "refs/heads/many3")
	if res, err := w.svc.Bank(ctx, w.p); err != nil || len(res.Branches) != 3 {
		t.Errorf("three changed branches: %+v %v", res, err)
	}

	// Over the bound on the listing: refused, nothing fetched.
	oldList := maxBankListing
	t.Cleanup(func() { maxBankListing = oldList })
	listed := strings.Count(w.git(w.box, "--git-dir=.repo", "for-each-ref", "refs/heads/", "refs/devc/wip/", "refs/tags/"), "\n") + 1
	maxBankListing = listed - 1
	n = len(w.rec.calls)
	if _, err := w.svc.Bank(ctx, w.p); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d refs", listed-1)) {
		t.Errorf("a listing over its bound: %v", err)
	}
	if got := w.fetches(n); len(got) != 0 {
		t.Errorf("a refused bank fetched: %q", got)
	}
	maxBankListing = listed
	if _, err := w.svc.Bank(ctx, w.p); err != nil {
		t.Errorf("a listing at its bound: %v", err)
	}
}

// gitIn runs git in dir with input on its standard input.
func (w *world) gitIn(dir, input string, args ...string) {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv()...)
	cmd.Stdin = strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		w.t.Fatalf("git %q: %v\n%s", args, err, out)
	}
}

// TestBankFetchBudget: a bank's fetch is bounded like a push. It does not
// start without the free-space reserve or with the relay over its budget,
// and the watchdog ends a fetch whose pack would take the relay over
// git.relay_quota. An ended fetch leaves no temporary pack, quarantine or
// lock file, earlier bank refs are untouched, the relay passes fsck, and
// the next bank, with room, banks the work (X7 1).
func TestBankFetchBudget(t *testing.T) {
	w, main := initWorld(t)
	ctx := context.Background()
	old := WatchEvery
	WatchEvery = 5 * time.Millisecond
	t.Cleanup(func() { WatchEvery = old })
	os.WriteFile(filepath.Join(main, "f"), []byte("2\n"), 0o644)
	w.git(main, "commit", "-qam", "c1")
	if _, err := w.svc.Bank(ctx, w.p); err != nil {
		t.Fatal(err)
	}
	banked := w.bankRefs()
	// A commit with 64 MiB that does not compress.
	big := make([]byte, 64<<20)
	rand.Read(big)
	os.WriteFile(filepath.Join(main, "big"), big, 0o644)
	w.git(main, "add", "big")
	w.git(main, "commit", "-qm", "big")
	bigTip := w.git(main, "rev-parse", "HEAD")

	// Without the free-space reserve: refused before git starts.
	w.svc.Free = func(string) (int64, error) { return paths.StateMinFree - 1, nil }
	n := len(w.rec.calls)
	if _, err := w.svc.Bank(ctx, w.p); err == nil || !strings.Contains(err.Error(), "free") {
		t.Errorf("a bank without the free-space reserve: %v", err)
	}
	if got := w.fetches(n); len(got) != 0 {
		t.Errorf("a bank without the reserve fetched: %q", got)
	}
	w.svc.Free = func(string) (int64, error) { return 100 << 30, nil }

	used, err := RelayUsage(w.p.Relay.Dir)
	if err != nil {
		t.Fatal(err)
	}
	// A relay already over its budget: refused before git starts.
	w.p.Resolved.Git.RelayQuota = used - 1
	n = len(w.rec.calls)
	if _, err := w.svc.Bank(ctx, w.p); err == nil || !strings.Contains(err.Error(), "git.relay_quota") {
		t.Errorf("a bank into a relay over its budget: %v", err)
	}
	if got := w.fetches(n); len(got) != 0 {
		t.Errorf("a bank into a full relay fetched: %q", got)
	}

	// The pack would take the relay over its budget: the fetch is ended.
	w.p.Resolved.Git.RelayQuota = used + 4<<20
	_, err = w.svc.Bank(ctx, w.p)
	var np *NotPreservedError
	if !errors.As(err, &np) || !np.All || !strings.Contains(err.Error(), "devc ended the fetch") || !strings.Contains(err.Error(), "git.relay_quota") {
		t.Fatalf("a fetch beyond the budget: %v", err)
	}
	if left := leftovers(w.p.Relay.Dir); len(left) > 0 {
		t.Errorf("left in the relay: %q", left)
	}
	time.Sleep(200 * time.Millisecond)
	if after, _ := RelayUsage(w.p.Relay.Dir); after > used+4<<20 {
		t.Errorf("the relay holds %d bytes after the fetch was ended, %d before", after, used)
	}
	if got := w.bankRefs(); got != banked {
		t.Errorf("an ended bank changed the bank refs or heads:\n%s\n%s", banked, got)
	}
	if refs := w.git(w.dir, "--git-dir="+w.p.Relay.Dir, "for-each-ref", IncomingPrefix); refs != "" {
		t.Errorf("incoming refs left: %s", refs)
	}
	w.git(w.dir, "--git-dir="+w.p.Relay.Dir, "fsck", "--no-progress")

	// The control: with room, the next bank banks the commit.
	w.p.Resolved.Git.RelayQuota = 1 << 30
	res, err := w.svc.Bank(ctx, w.p)
	if err != nil || len(res.Branches) != 1 || res.Branches[0].New != bigTip {
		t.Fatalf("the next bank: %+v %v", res, err)
	}
	if left := leftovers(w.p.Relay.Dir); len(left) > 0 {
		t.Errorf("left in the relay after a bank: %q", left)
	}
}
