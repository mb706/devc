package gitx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/paths"
)

func TestHostCmd(t *testing.T) {
	h := &Host{HooksDir: "/state/local/git/no-hooks"}
	cmd, err := h.Cmd(Call{Relay: "/state/relay/p.git", Args: []string{"log", "--oneline"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--git-dir=/state/relay/p.git", "-c", "core.hooksPath=/state/local/git/no-hooks", "-c", "core.fsmonitor=false",
		"log", "--no-ext-diff", "--no-textconv", "--oneline"}
	if cmd.Name != "git" || !slices.Equal(cmd.Args, want) || cmd.Dir != "/" {
		t.Errorf("internal log: %s %q in %q", cmd.Name, cmd.Args, cmd.Dir)
	}
	if !slices.Equal(cmd.Unset, []string{"GIT_*"}) || !slices.Contains(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null") ||
		!slices.Contains(cmd.Env, "GIT_CONFIG_NOSYSTEM=1") || !slices.Contains(cmd.Env, "LC_ALL=C") || !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("internal environment: unset %q env %q", cmd.Unset, cmd.Env)
	}

	cmd, _ = h.Cmd(Call{Relay: "/state/relay/p.git", Args: []string{"fetch", "ext::podman exec x %S /w/r/p/.repo"}, Box: true})
	if !slices.Equal(cmd.Args[5:9], []string{"-c", "protocol.allow=never", "-c", "protocol.ext.allow=always"}) || cmd.Args[9] != "fetch" {
		t.Errorf("box fetch: %q", cmd.Args)
	}
	if cmd.Timeout != transferTimeout {
		t.Errorf("box fetch timeout %s", cmd.Timeout)
	}
	// Only the box transport gets ext.
	cmd, _ = h.Cmd(Call{Relay: "/state/relay/p.git", Args: []string{"fetch", "github"}, Mode: Upstream})
	if slices.Contains(cmd.Args, "protocol.ext.allow=always") || slices.Contains(cmd.Unset, "GIT_*") || !slices.Contains(cmd.Unset, "GIT_DIR") ||
		!slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") || slices.Contains(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Errorf("upstream fetch: %q unset %q env %q", cmd.Args, cmd.Unset, cmd.Env)
	}
	h.Terminal = true
	cmd, _ = h.Cmd(Call{Relay: "/state/relay/p.git", Args: []string{"fetch", "github"}, Mode: Upstream})
	if slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("an attached upstream command may prompt: %q", cmd.Env)
	}
	cmd, _ = h.Cmd(Call{Relay: "/state/relay/p.git", Args: []string{"diff"}, Mode: Display})
	if slices.Contains(cmd.Env, "LC_ALL=C") || !slices.Contains(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Errorf("display environment %q", cmd.Env)
	}

	for _, c := range []Call{
		{Relay: "relative.git", Args: []string{"log"}},
		{Relay: "/a/../b.git", Args: []string{"log"}},
		{Relay: "/r.git", Args: []string{"--exec-path=/x", "log"}},
		{Relay: "/r.git"},
	} {
		if _, err := h.Cmd(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	if _, err := (&Host{HooksDir: "rel"}).Cmd(Call{Relay: "/r.git", Args: []string{"log"}}); err == nil {
		t.Error("accepted a relative hooks directory")
	}
}

func TestBoxURL(t *testing.T) {
	u, err := BoxURL("/opt/my tools/podman", "devc-r-p", "/w/r/p/.repo")
	if err != nil || u != "ext::/opt/my% tools/podman exec -i -u 1000:1000 devc-r-p %S /w/r/p/.repo" {
		t.Errorf("%q %v", u, err)
	}
	if u, _ := BoxURL("pod%man", "c", "/w/r/p/.repo"); !strings.HasPrefix(u, "ext::pod%%man ") {
		t.Errorf("%q", u)
	}
	for _, bad := range [][3]string{{"", "c", "/w"}, {"podman\n", "c", "/w"}, {"podman", "-c", "/w"}, {"podman", "c d", "/w"}, {"podman", "c", "rel"}, {"podman", "c", "/w %G"}} {
		if _, err := BoxURL(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPlanBank(t *testing.T) {
	const a, b, c, d = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd"
	in := BankInput{
		Branches: map[string]string{
			"main":     b, // non-fast-forward or not: accepted
			"same":     a, // unchanged
			"new":      c, // created
			"prot":     c, // protected and different
			"protsame": a, // protected but unchanged: no report
			"-evil":    c, // hostile names
			"a..b":     c, //
			"ünï":      c, //
			"back":     a, // tip recorded before: no new bank ref
			"x.lock":   c, //
			"HEAD":     c, //
			"ok/deep":  c, // slashes are fine
		},
		Heads:        map[string]string{"main": a, "same": a, "protsame": a, "kept": a, "back": b},
		LastBank:     map[string]Stamped{"main": {a, 100}, "back": {a, 200}},
		Protected:    []string{"prot*"},
		WIP:          map[string]string{"main": c, "t1": d, "same": a, "bad name": c},
		LastSnap:     map[string]Stamped{"t1": {a, 50}, "same": {b, 60}},
		Tags:         map[string]string{"v1": a, "v2": b, "v3": c, "bad tag": d},
		UpstreamTags: map[string]string{"v1": a, "v3": a},
		DevcTags:     map[string]map[string]bool{"v2": {b: true}},
		NewestTag:    map[string]int64{"v3": 900},
		Now:          150,
		SameSnapshot: func(x, y string) bool { return x == a && y == b },
	}
	p := PlanBank(in)
	got := map[string]BranchChange{}
	for _, ch := range p.Branches {
		got[ch.Branch] = ch
	}
	if ch := got["main"]; ch.Old != a || ch.New != b || ch.BankRef != "refs/devc/bank/main/150" {
		t.Errorf("main: %+v", ch)
	}
	if ch := got["new"]; ch.Old != "" || ch.BankRef != "refs/devc/bank/new/150" {
		t.Errorf("new: %+v", ch)
	}
	if ch, ok := got["back"]; !ok || ch.BankRef != "" {
		t.Errorf("back (tip recorded before): %+v", ch)
	}
	if _, ok := got["ok/deep"]; !ok {
		t.Error("ok/deep was refused")
	}
	for _, n := range []string{"same", "prot", "protsame", "-evil", "a..b", "ünï", "x.lock", "HEAD", "kept"} {
		if _, ok := got[n]; ok {
			t.Errorf("%s was applied", n)
		}
	}
	skipped := map[string]string{}
	for _, s := range p.Skipped {
		skipped[s.Kind+":"+s.Name] = s.Reason
	}
	for _, n := range []string{"branch:prot", "branch:-evil", "branch:a..b", "branch:ünï", "branch:x.lock", "branch:HEAD", "snapshot:bad name", "tag:bad tag"} {
		if skipped[n] == "" {
			t.Errorf("%s not reported (%v)", n, skipped)
		}
	}
	if !strings.Contains(skipped["branch:prot"], "protected") || skipped["branch:protsame"] != "" {
		t.Errorf("protected reporting: %v", skipped)
	}
	snaps := map[string]Recorded{}
	for _, s := range p.Snapshots {
		snaps[s.Name] = s
	}
	if len(snaps) != 2 || snaps["main"].Ref != "refs/devc/snap/main/150" || snaps["t1"].OID != d {
		t.Errorf("snapshots: %+v", p.Snapshots)
	}
	tags := map[string]Recorded{}
	for _, r := range p.Tags {
		tags[r.Name] = r
	}
	// v1 equals the upstream's, v2 was recorded; v3 differs from the
	// upstream's and is stamped after the newest existing one.
	if len(tags) != 1 || tags["v3"].Ref != "refs/devc/tags/v3/901" {
		t.Errorf("tags: %+v", p.Tags)
	}
}

func TestCheckNameAndMatches(t *testing.T) {
	for _, ok := range []string{"main", "agent/t1", "release-1.2", "a_b", "x\"y"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "HEAD", "@", "a b", "a\tb", "a~1", "a^", "a:b", "a?", "a*", "a[", "a\\b", "/a", "a/", "a//b",
		"a..b", "a@{1}", "a.", ".a", "a/.b", "a.lock", "a/b.lock/c", "ü", "a\x7f", strings.Repeat("x", 201)} {
		if CheckName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if !Matches([]string{"fwd/*"}, "fwd/a") || Matches([]string{"fwd/*"}, "fwd/a/b") || Matches(nil, "x") || !Matches([]string{"x", "rel?"}, "rel1") {
		t.Error("Matches")
	}
}

func TestSortRefs(t *testing.T) {
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	s := SortRefs([]Ref{
		{"refs/heads/main", a}, {"refs/heads/x/y", b}, {"refs/remotes/github/main", a}, {"refs/remotes/github/HEAD", a},
		{"refs/tags/v1", b}, {"refs/devc/bank/x/y/100", a}, {"refs/devc/bank/x/y/200", b}, {"refs/devc/bank/bad", a},
		{"refs/devc/snap/main/5", b}, {"refs/devc/tags/v1/7", a}, {"refs/devc/tags/v1/9", b},
		{"refs/devc/incoming/heads/f/g", a}, {"refs/devc/incoming/wip/main", b}, {"refs/devc/incoming/tags/t", a},
	})
	if s.Heads["x/y"] != b || len(s.Github) != 1 || s.UpstreamTags["v1"] != b || s.LastBank["x/y"] != (Stamped{b, 200}) ||
		len(s.LastBank) != 1 || s.LastSnap["main"] != (Stamped{b, 5}) || !s.DevcTags["v1"][a] || s.NewestTag["v1"] != 9 ||
		s.DevcTagNewest["v1"].OID != b || s.InBranches["f/g"] != a || s.InWIP["main"] != b || s.InTags["t"] != a || len(s.Incoming) != 3 {
		t.Errorf("%+v", s)
	}
	if _, err := parseRefList("zz refs/heads/x\n"); err == nil {
		t.Error("accepted a bad object name")
	}
}

func TestSanitizeAndParsers(t *testing.T) {
	in := "\x1b[33mabc1234\x1b[m evil \x1b]0;title\x07 \x1b[2J done\x9b\n"
	got := Sanitize([]byte(in))
	if !strings.Contains(got, "\x1b[33mabc1234\x1b[m") || strings.Contains(got, "\x1b]") || strings.Contains(got, "\x07") || strings.Contains(got, "\x1b[2J") || strings.Contains(got, "\u009b") {
		t.Errorf("%q", got)
	}
	rs := parsePorcelain("To ext::x\n \trefs/heads/a:refs/remotes/origin/a\t[up to date]\n!\trefs/tags/v1:refs/tags/v1\t[rejected] (already exists)\nDone\n")
	if len(rs) != 2 || rs[1].Flag != '!' || rs[1].To != "refs/tags/v1" || !strings.Contains(rs[1].Summary, "rejected") {
		t.Errorf("%+v", rs)
	}
	out := "ref: refs/heads/trunk\tHEAD\n0123\tHEAD\nref: refs/remotes/origin/dev\trefs/remotes/origin/HEAD\n"
	if symrefTarget(out, "HEAD", HeadsPrefix) != "trunk" || symrefTarget(out, "refs/remotes/origin/HEAD", "refs/remotes/origin/") != "dev" ||
		symrefTarget("ref: refs/heads/-x\tHEAD\n", "HEAD", HeadsPrefix) != "" {
		t.Error("symrefTarget")
	}
	if checkDisplayArgs([]string{"--stat", "--ext-diff"}) == nil || checkDisplayArgs([]string{"-p"}) != nil {
		t.Error("checkDisplayArgs")
	}
}

// recorder runs commands for real and records them.
type recorder struct {
	execx.Real
	mu    sync.Mutex
	calls []execx.Cmd
}

func (r *recorder) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	return r.Real.Run(ctx, c)
}

func (r *recorder) Interactive(ctx context.Context, c execx.Cmd) (int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	return r.Real.Interactive(ctx, c)
}

// world is a project whose "box" is a directory on this machine: a fake
// engine answers box execs by running git there (in-box paths mapped), and
// a script named as the engine serves the ext:: transport.
type world struct {
	t     *testing.T
	dir   string
	box   string // host directory standing in for /w/r/p
	up    string // bare upstream
	rec   *recorder
	svc   *Service
	p     *Project
	hooks string
}

func gitEnv() []string {
	return []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x"}
}

func (w *world) git(dir string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %q in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// mapPath turns an in-box path into the host directory standing in for it.
func (w *world) mapPath(s string) string {
	return strings.ReplaceAll(s, "/w/r/p", w.box)
}

func (w *world) exec(o engine.ExecOptions) (execx.Result, error) {
	var res execx.Result
	cmd := make([]string, len(o.Cmd))
	for i, a := range o.Cmd {
		cmd[i] = w.mapPath(a)
	}
	brepo := &boxside.Repo{ProjectDir: w.box, Git: boxside.ExecGit}
	switch {
	case cmd[0] == "git":
		c := exec.Command("git", cmd[1:]...)
		c.Dir = w.mapPath(o.Workdir)
		c.Env = append(os.Environ(), gitEnv()...)
		var out, errb bytes.Buffer
		c.Stdout, c.Stderr = &out, &errb
		err := c.Run()
		res = execx.Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
		var xe *exec.ExitError
		if errors.As(err, &xe) {
			res.ExitCode = xe.ExitCode()
			return res, &execx.ExitError{Cmd: strings.Join(cmd, " "), Result: res}
		}
		return res, err
	case cmd[0] == paths.InBoxHelper && cmd[1] == "box":
		var v any
		switch cmd[2] {
		case "snapshot":
			v = brepo.Snapshot("", w.dir)
		case "repo-state":
			st := brepo.State()
			v = st
		default:
			return res, errors.New("unexpected helper command " + strings.Join(cmd, " "))
		}
		b, _ := json.Marshal(v)
		res.Stdout = b
		return res, nil
	}
	return res, errors.New("unexpected exec " + strings.Join(cmd, " "))
}

func newWorld(t *testing.T) *world {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	for _, kv := range gitEnv()[:2] {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	w := &world{t: t, dir: dir, box: filepath.Join(dir, "projects", "r", "p"), up: filepath.Join(dir, "up.git"), hooks: filepath.Join(dir, "state", "local", "git", "no-hooks")}
	if err := os.MkdirAll(w.box, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(w.hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	// The upstream with one commit on main.
	w.git(dir, "init", "-q", "--bare", "-b", "main", w.up)
	seed := filepath.Join(dir, "seed")
	w.git(dir, "clone", "-q", "file://"+w.up, seed)
	os.WriteFile(filepath.Join(seed, "f"), []byte("1\n"), 0o644)
	w.git(seed, "add", "f")
	w.git(seed, "commit", "-qm", "U1")
	w.git(seed, "push", "-q", "origin", "HEAD:main")
	// The ext:: transport: run the service on the mapped repository with a
	// clean environment, as podman exec would.
	script := filepath.Join(dir, "fakepodman")
	os.WriteFile(script, []byte("#!/bin/sh\nrepo=$7\ncase \"$repo\" in /w/r/p/*) repo=\""+w.box+"${repo#/w/r/p}\";; *) exit 9;; esac\n"+
		"exec env -i PATH=/usr/bin:/bin:/usr/lib/git-core GIT_CONFIG_NOSYSTEM=1 HOME=/nonexistent \"$6\" \"$repo\"\n"), 0o755)

	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(dir, "config"), StateDir: filepath.Join(dir, "state"), RuntimeDir: filepath.Join(dir, "run"), Home: dir})
	if err != nil {
		t.Fatal(err)
	}
	if p, err = p.WithGlobal("", filepath.Join(dir, "projects")); err != nil {
		t.Fatal(err)
	}
	eng := engine.NewFake()
	eng.Bin = script
	eng.ExecFunc = w.exec
	eng.Containers["devc-r-p"] = engine.ContainerInfo{Name: "devc-r-p", State: engine.ContainerState{Running: true}}
	w.rec = &recorder{}
	w.svc = &Service{Host: &Host{Runner: w.rec, HooksDir: w.hooks}, Engine: eng, Paths: p, Out: &bytes.Buffer{}}
	w.p = w.svc.Project(&config.Resolved{Realm: "r", Project: "p", Upstream: "file://" + w.up,
		Git: config.ResolvedGit{Protected: []string{"prot"}, Forward: []string{"fwd/*"}, UserName: "Box User", UserEmail: "box@x"}}, true)
	return w
}

// checkCalls asserts the two rules on every host git command recorded.
func (w *world) checkCalls() {
	w.t.Helper()
	n := 0
	for _, c := range w.rec.calls {
		if c.Name != "git" {
			continue
		}
		n++
		relay := c.Args[0] == "--git-dir="+w.p.Relay.Dir || strings.HasPrefix(c.Args[0], "--git-dir="+w.p.Relay.Dir+".tmp-")
		if len(c.Args) < 3 || !relay || c.Args[1] != "-c" || c.Args[2] != "core.hooksPath="+w.hooks {
			w.t.Errorf("host git without the relay and hooks path: %q", c.Args)
		}
		if c.Dir != "/" {
			w.t.Errorf("host git in %q: %q", c.Dir, c.Args)
		}
		for _, a := range c.Args {
			if strings.Contains(a, w.box) {
				w.t.Errorf("host git names the box directory: %q", c.Args)
			}
		}
		if slices.Contains(c.Args, "protocol.ext.allow=always") != slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "ext::") }) {
			w.t.Errorf("ext allowed without the box transport or the other way round: %q", c.Args)
		}
	}
	if n < 20 {
		w.t.Errorf("only %d host git commands recorded", n)
	}
}

func TestEndToEnd(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	def, err := w.svc.InitRelay(ctx, w.p)
	if err != nil || def != "main" {
		t.Fatalf("InitRelay: %q %v", def, err)
	}
	if err := w.svc.InitBox(ctx, w.p, def); err != nil {
		t.Fatalf("InitBox: %v", err)
	}
	main := filepath.Join(w.box, "main")
	upMain := w.git(w.up, "rev-parse", "main")
	if got := w.git(main, "rev-parse", "--abbrev-ref", "HEAD", "@{upstream}"); got != "main\norigin/main" {
		t.Errorf("box checkout: %q", got)
	}
	if got := w.git(main, "config", "remote.origin.url"); got != "devc::r/p" {
		t.Errorf("origin %q", got)
	}
	// A second init is a no-op.
	if err := w.svc.InitBox(ctx, w.p, def); err != nil {
		t.Fatalf("second InitBox: %v", err)
	}

	// Commit, amend, protected branch, uncommitted change; then bank.
	os.WriteFile(filepath.Join(main, "f"), []byte("2\n"), 0o644)
	w.git(main, "commit", "-qam", "c1")
	c1 := w.git(main, "rev-parse", "HEAD")
	res, err := w.svc.Bank(ctx, w.p)
	if err != nil || len(res.Branches) != 1 || res.Branches[0].Added != 1 {
		t.Fatalf("bank: %+v %v", res, err)
	}
	w.git(main, "commit", "-q", "--amend", "-m", "c1b")
	w.git(main, "branch", "prot")
	w.git(main, "update-ref", "refs/heads/-evil", "HEAD")
	os.WriteFile(filepath.Join(main, "f"), []byte("dirty\n"), 0o644)
	res, err = w.svc.Bank(ctx, w.p)
	if err != nil {
		t.Fatal(err)
	}
	out := FormatBank(res, false)
	if !strings.Contains(out, "rewrote main: +1, 1 commit left behind") || !strings.Contains(out, `skipped branch "prot"`) ||
		!strings.Contains(out, `skipped branch "-evil"`) || !strings.Contains(out, "snapshot of main") {
		t.Errorf("bank summary:\n%s", out)
	}
	if !res.HasProblems() || !strings.Contains(FormatBank(res, true), "prot") || strings.Contains(FormatBank(res, true), "rewrote") {
		t.Errorf("quiet summary:\n%s", FormatBank(res, true))
	}
	set, err := w.p.Relay.RefSet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c1b := w.git(main, "rev-parse", "HEAD")
	banked := map[string]bool{}
	refs, _ := w.p.Relay.Refs(ctx, BankPrefix+"main/")
	for _, r := range refs {
		banked[r.OID] = true
	}
	if set.Heads["main"] != c1b || !banked[c1] || !banked[c1b] || !banked[upMain] || set.Heads["prot"] != "" || len(set.Incoming) != 0 {
		t.Errorf("relay after bank: heads %v banked %v incoming %v", set.Heads, banked, set.Incoming)
	}
	if w.git(main, "rev-parse", "origin/main") != c1b {
		t.Error("origin/main was not reflected into the box")
	}
	if got := w.git(main, "status", "--porcelain"); got != "M f" {
		t.Errorf("the box worktree changed: %q", got)
	}
	snap := set.LastSnap["main"].OID
	if snap == "" || w.git(w.box, "--git-dir="+w.p.Relay.Dir, "show", snap+":f") != "dirty" {
		t.Errorf("snapshot %q", snap)
	}
	// Nothing new: no new snapshot, no branch change.
	if res, err = w.svc.Bank(ctx, w.p); err != nil || len(res.Branches)+len(res.Snapshots) != 0 {
		t.Errorf("idle bank: %+v %v", res, err)
	}
	w.git(main, "checkout", "-q", "f")

	// Forward, push (fast-forward, then a rewrite needing Force), pull.
	w.git(main, "branch", "fwd/x")
	if res, err = w.svc.Bank(ctx, w.p); err != nil || len(res.Forwards) != 1 || res.Forwards[0].Error != "" {
		t.Fatalf("forward: %+v %v", res, err)
	}
	if w.git(w.up, "rev-parse", "fwd/x") != c1b {
		t.Error("fwd/x was not forwarded")
	}
	err = w.svc.Push(ctx, w.p, PushOptions{Branches: []string{"main"}})
	if !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("push without confirmation: %v", err)
	}
	if err := w.svc.Push(ctx, w.p, PushOptions{Branches: []string{"main"}, Confirm: func(string) bool { return true }}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if w.git(w.up, "rev-parse", "main") != c1b {
		t.Error("the upstream did not receive main")
	}
	w.git(main, "commit", "-q", "--amend", "-m", "c1c")
	if _, err := w.svc.Bank(ctx, w.p); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Push(ctx, w.p, PushOptions{Branches: []string{"main"}, Yes: true}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("push of a rewrite: %v", err)
	}
	if err := w.svc.Push(ctx, w.p, PushOptions{Branches: []string{"main"}, Yes: true, Force: true}); err != nil {
		t.Fatalf("push --force: %v", err)
	}
	c1c := w.git(main, "rev-parse", "HEAD")
	if w.git(w.up, "rev-parse", "main") != c1c {
		t.Error("the upstream did not receive the rewrite")
	}
	seed := filepath.Join(w.dir, "seed")
	w.git(seed, "fetch", "-q", "origin")
	w.git(seed, "reset", "-q", "--hard", "origin/main")
	w.git(seed, "commit", "-q", "--allow-empty", "-m", "U2")
	w.git(seed, "push", "-q", "origin", "HEAD:main")
	pr, err := w.svc.Pull(ctx, w.p)
	if err != nil || len(pr.Branches) != 1 || pr.Branches[0].Branch != "main" {
		t.Fatalf("pull: %+v %v", pr, err)
	}
	if w.git(main, "rev-parse", "github/main") != w.git(w.up, "rev-parse", "main") {
		t.Error("github/main was not handed to the box")
	}
	if _, err := w.svc.Log(ctx, w.p, nil, false); err != nil {
		t.Errorf("log: %v", err)
	}
	if _, err := w.svc.Log(ctx, w.p, []string{"--ext-diff"}, false); err == nil {
		t.Error("log accepted --ext-diff")
	}
	if d, err := w.svc.Diff(ctx, w.p, []string{"--stat", upMain, c1c}, false); err != nil || !strings.Contains(d, "f") {
		t.Errorf("diff: %q %v", d, err)
	}
	w.checkCalls()
}

func TestInitBoxRefusesForeignRepository(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	def, err := w.svc.InitRelay(ctx, w.p)
	if err != nil {
		t.Fatal(err)
	}
	w.git(w.dir, "init", "-q", "--separate-git-dir", filepath.Join(w.box, ".repo"), filepath.Join(w.box, "main"))
	w.git(filepath.Join(w.box, "main"), "remote", "add", "origin", "https://example.org/other.git")
	if err := w.svc.InitBox(ctx, w.p, def); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("foreign repository: %v", err)
	}
}

func TestPlanBankLimit(t *testing.T) {
	const a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	in := BankInput{Branches: map[string]string{}, Tags: map[string]string{}, MaxChanges: 2, Now: 1}
	for _, n := range []string{"b1", "b2", "b3", "b4"} {
		in.Branches[n] = a
		in.Tags["t"+n] = a
	}
	p := PlanBank(in)
	if len(p.Branches) != 2 || len(p.Tags) != 2 || len(p.Skipped) != 2 || p.Skipped[0].Name != "(2 more)" || !strings.Contains(p.Skipped[1].Reason, "next time") {
		t.Errorf("%+v", p)
	}
}
