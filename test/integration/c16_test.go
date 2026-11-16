//go:build integration

package integration

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/depsrc/depsrctest"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// sharedEntries lists a host directory (a share's version) with link
// targets.
func sharedEntries(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			l, _ := os.Readlink(p)
			rel += " -> " + l
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

var publishedRE = regexp.MustCompile(`published (\S+) version (\S+) `)

// TestShares covers work order C16 section 4, integration items 1, 2 and
// 4: a skill published from a project's relay appears read-only in a box
// of another realm and is linked into both skill directories; uncommitted
// work is not published; a second publish shows the diff and the running
// box sees the new version at once; a hostile source lands nothing
// outside its version; the built-in dep-source skill is in every box.
func TestShares(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.Timeout = 5 * time.Minute
	up, work := upstream(t)
	writeFile(t, filepath.Join(work, "skills", "review", "SKILL.md"), "---\nname: review\ndescription: Review code.\n---\nReview carefully.\n")
	writeFile(t, filepath.Join(work, "skills", "review", "check.sh"), "#!/bin/sh\necho ok\n")
	hostGit(t, work, "add", ".")
	hostGit(t, work, "commit", "--quiet", "-m", "skill")
	hostGit(t, work, "push", "--quiet", "origin", "main")

	src := itest.RealmA + "/shsrc"
	e.MustRun("project", "add", src, "--from", "file://"+up)
	main := inBox(src) + "/main"
	// Uncommitted, and committed but never banked: neither is in the relay.
	boxSh(t, e, src, "cd "+main+" && echo uncommitted > skills/review/SKILL.md && echo x > skills/review/new.txt && git add skills/review/new.txt && git commit -qm unbanked skills/review/new.txt")

	cons := itest.RealmB + "/shcons"
	project(e, cons, "env = \""+itest.EnvBase+"\"\nharnesses = []\nshares = [\"review\"]\n")

	res := e.MustRun("share", "publish", "review", "--from", src, "--path", "skills/review", "--yes")
	t.Logf("publish:\n%s", res.Stdout)
	if !strings.Contains(res.Stdout, "a new share") || !strings.Contains(res.Stdout, "SKILL.md") || strings.Contains(res.Stdout, "new.txt") {
		t.Errorf("first publish output:\n%s", res.Stdout)
	}
	m := publishedRE.FindStringSubmatch(res.Stdout)
	if m == nil {
		t.Fatalf("no version in %q", res.Stdout)
	}
	v1 := m[2]
	// Without a terminal and without --yes nothing is published.
	if r := e.Run("share", "publish", "other", "--from", src, "--path", "skills"); r.ExitCode == 0 || !strings.Contains(r.Stderr, "--yes") {
		t.Errorf("publish without --yes: %d %s", r.ExitCode, r.Stderr)
	}

	e.MustRun("up", cons)
	got := boxSh(t, e, cons, "cat /opt/shares/review/SKILL.md; ls /opt/shares/review")
	if !strings.Contains(got, "Review carefully.") || strings.Contains(got, "uncommitted") || strings.Contains(got, "new.txt") {
		t.Errorf("the share in the box:\n%s", got)
	}
	got = boxSh(t, e, cons, "touch /opt/shares/review/x 2>/dev/null && echo writable || echo refused; "+
		"touch /opt/shares/.v/review/x 2>/dev/null && echo writable || echo refused; "+
		"rm -f /opt/shares/review/SKILL.md 2>/dev/null || true; test -f /opt/shares/review/SKILL.md && echo kept")
	if got != "refused\nrefused\nkept" {
		t.Errorf("the share is not read-only: %q", got)
	}
	got = boxSh(t, e, cons, "readlink ~/.agents/skills/review ~/.claude/skills/review ~/.agents/skills/dep-source ~/.claude/skills/dep-source")
	if got != "/opt/shares/review\n/opt/shares/review\n/opt/shares/dep-source\n/opt/shares/dep-source" {
		t.Errorf("skill links: %q", got)
	}
	// 4. The built-in skill.
	if got := boxSh(t, e, cons, "head -n 2 ~/.agents/skills/dep-source/SKILL.md"); got != "---\nname: dep-source" {
		t.Errorf("dep-source: %q", got)
	}
	if got := boxSh(t, e, src, "test -f /opt/shares/dep-source/SKILL.md && echo present; ls /opt/shares"); got != "present\ndep-source" {
		t.Errorf("dep-source in the source box: %q", got)
	}

	// A second version: committed and banked; the diff is shown and the
	// running box sees it at once.
	boxSh(t, e, src, "cd "+main+" && git checkout -q skills/review/SKILL.md && echo 'Also check the tests.' >> skills/review/SKILL.md && git commit -qam second")
	e.MustRun("bank", src)
	res = e.MustRun("share", "publish", "review", "--from", src, "--path", "skills/review", "--yes")
	t.Logf("second publish:\n%s", res.Stdout)
	if !strings.Contains(res.Stdout, "changes since version "+v1) || !strings.Contains(res.Stdout, "+Also check the tests.") ||
		!strings.Contains(res.Stdout, "new.txt") || !strings.Contains(res.Stdout, "boxes of "+cons+" see it now") {
		t.Errorf("second publish output:\n%s", res.Stdout)
	}
	if got := boxSh(t, e, cons, "cat /opt/shares/review/SKILL.md"); !strings.Contains(got, "Also check the tests.") {
		t.Errorf("the running box does not see the new version: %q", got)
	}
	if r := e.MustRun("share", "publish", "review", "--from", src, "--path", "skills/review", "--yes"); !strings.Contains(r.Stdout, "nothing to publish") {
		t.Errorf("unchanged publish: %s", r.Stdout)
	}
	logOut := e.MustRun("share", "log", "review").Stdout
	if strings.Count(logOut, src+":skills/review") != 2 || !strings.Contains(logOut, v1) {
		t.Errorf("share log:\n%s", logOut)
	}
	var ls []struct {
		Name   string   `json:"name"`
		UsedBy []string `json:"used_by"`
	}
	if err := json.Unmarshal([]byte(e.MustRun("share", "ls", "--json").Stdout), &ls); err != nil || len(ls) != 1 || ls[0].Name != "review" || !slices.Equal(ls[0].UsedBy, []string{cons}) {
		t.Errorf("share ls: %+v %v", ls, err)
	}

	// 2. A hostile source: links out of the tree and names with "..".
	boxSh(t, e, src, "cd "+main+" && mkdir hostile && cd hostile && echo ok > ok.md && echo dots > ..evil && echo dots > x..y && "+
		"ln -s ../../../../../../etc/passwd out && ln -s /etc abs && ln -s ok.md in && ln -s ../skills up && git add -A . && git commit -qm hostile")
	e.MustRun("bank", src)
	res = e.MustRun("share", "publish", "hostile", "--from", src, "--path", "hostile", "--yes")
	t.Logf("hostile publish:\n%s", res.Stdout)
	if !strings.Contains(res.Stdout, "left out 3 entries") {
		t.Errorf("hostile publish output:\n%s", res.Stdout)
	}
	sharesDir := filepath.Join(e.StateDir, "durable", "shares")
	hv := publishedRE.FindStringSubmatch(res.Stdout)
	if hv == nil {
		t.Fatal("no version for hostile")
	}
	if got := sharedEntries(t, filepath.Join(sharesDir, "hostile", hv[2])); !slices.Equal(got, []string{"..evil", "in -> ok.md", "ok.md", "x..y"}) {
		t.Errorf("hostile version: %v", got)
	}
	var top []string
	des, _ := os.ReadDir(sharesDir)
	for _, d := range des {
		top = append(top, d.Name())
	}
	if !slices.Equal(top, []string{".staging", "hostile", "review"}) {
		t.Errorf("durable/shares holds %v", top)
	}
	if des, _ := os.ReadDir(filepath.Join(sharesDir, ".staging")); len(des) != 0 {
		t.Errorf("staging left behind: %v", des)
	}
	if des, _ := os.ReadDir(filepath.Join(sharesDir, "hostile")); len(des) != 2 {
		t.Errorf("share directory: %v", des)
	}

	// rm: the versions go; the box keeps a valid, empty mount.
	if r := e.MustRun("share", "rm", "review", "--yes"); !strings.Contains(r.Stdout, "removed 2 versions") {
		t.Errorf("share rm: %s", r.Stdout)
	}
	if got := boxSh(t, e, cons, "ls -A /opt/shares/.v/review | wc -l; test -e /opt/shares/review || echo gone"); got != "0\ngone" {
		t.Errorf("after rm: %q", got)
	}
	e.MustRun("share", "rm", "hostile", "--yes")
}

// TestDepSources covers integration item 3: devc src for each ecosystem
// against local stand-ins for the registries (reached from the box through
// a host exception, as the box would reach the registries over the
// internet), the installed version found in the box, a second call served
// from the cache, and a box of another realm not seeing it.
func TestDepSources(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	e.Timeout = 5 * time.Minute
	reg := depsrctest.New()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	ref := itest.RealmA + "/srcp"
	other := itest.RealmB + "/srcq"
	project(e, ref, "env = \""+itest.EnvBase+"\"\nharnesses = []\n")
	project(e, other, "env = \""+itest.EnvBase+"\"\nharnesses = []\n")
	e.MustRun("up", ref)
	e.MustRun("up", other)
	itest.StartDaemon(t, e)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	allowID(t, e, "host:"+u.Port(), ref, "--for", "1h")

	env := strings.Join(depsrctest.Env("http://127.0.0.1:"+u.Port()), " ")
	// What is installed in the box: an R library, a virtual environment
	// and a node_modules tree.
	boxSh(t, e, ref, "mkdir -p ~/rlib/mypkg ~/venv/lib/python3.11/site-packages/my_lib-2.0.0.dist-info ~/proj/sub ~/proj/node_modules/@scope/thing && "+
		"printf 'Package: mypkg\\nVersion: 1.1.0\\n' > ~/rlib/mypkg/DESCRIPTION && "+
		"printf 'Name: my-lib\\nVersion: 2.0.0\\n' > ~/venv/lib/python3.11/site-packages/my_lib-2.0.0.dist-info/METADATA && "+
		`printf '{"name":"@scope/thing","version":"3.1.4"}' > ~/proj/node_modules/@scope/thing/package.json`)
	src := func(spec string, json bool) itest.Result {
		flag := ""
		if json {
			flag = " --json"
		}
		return boxRun(e, ref, "cd ~/proj/sub && env "+env+" R_LIBS=$HOME/rlib VIRTUAL_ENV=$HOME/venv devc src "+spec+flag)
	}
	cases := []struct{ spec, path, file, eco string }{
		{"r:mypkg", "/src/r/mypkg/1.1.0", "R/code.R", "cran"},
		{"pypi:my_lib", "/src/pypi/my-lib/2.0.0", "my_lib/__init__.py", "pypi"},
		{"npm:@scope/thing", "/src/npm/@scope/thing/3.1.4", "src/index.js", "npm"},
		{"github:owner/repo@main", "/src/github/owner/repo/" + depsrctest.Commit, "README.md", "github"},
	}
	for _, c := range cases {
		res := src(c.spec, false)
		if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != c.path {
			t.Errorf("devc src %s: exit %d\nstdout: %s\nstderr: %s", c.spec, res.ExitCode, res.Stdout, res.Stderr)
			continue
		}
		if got := boxSh(t, e, ref, "test -f "+c.path+"/"+c.file+" && echo present"); got != "present" {
			t.Errorf("%s: %s/%s missing", c.spec, c.path, c.file)
		}
	}
	// Nothing was built: the sdist's setup.py is only a file.
	if got := boxSh(t, e, ref, "cat /src/pypi/my-lib/2.0.0/setup.py"); !strings.Contains(got, "never run") {
		t.Errorf("setup.py: %q", got)
	}
	for _, c := range cases {
		if n := reg.Downloads(c.eco); n != 1 {
			t.Errorf("%s: %d downloads", c.eco, n)
		}
		res := src(c.spec, true)
		var r struct {
			Path   string `json:"path"`
			Cached bool   `json:"cached"`
		}
		if res.ExitCode != 0 || json.Unmarshal([]byte(res.Stdout), &r) != nil || !r.Cached || r.Path != c.path {
			t.Errorf("second devc src %s: %+v\n%s%s", c.spec, r, res.Stdout, res.Stderr)
		}
		if n := reg.Downloads(c.eco); n != 1 {
			t.Errorf("%s after the second call: %d downloads", c.eco, n)
		}
	}
	// The cache belongs to the realm: the other realm's box sees nothing.
	if got := boxSh(t, e, other, "ls -A /src | wc -l"); got != "0" {
		t.Errorf("the other realm's /src: %q", got)
	}
	if _, err := os.Stat(filepath.Join(e.StateDir, "cache", "realms", itest.RealmA, "src", "r", "mypkg", "1.1.0", "DESCRIPTION")); err != nil {
		t.Errorf("host cache: %v", err)
	}
}

// TestNodeAndPi covers integration item 5: the toolbox component node runs
// in a box, and the built-in pi manifest, pointed at the fake harness,
// installs and launches it with node first in PATH and its baseline in
// place.
func TestNodeAndPi(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.Timeout = 10 * time.Minute
	res := e.MustRun("toolbox", "install", "node")
	t.Logf("toolbox install node: %s", res.Stdout)
	b, err := os.ReadFile(filepath.Join(e.Repo, "assets", "harness", "pi.toml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := regexp.MustCompile(`(?s)\[install\].*?\n\[home\]`).ReplaceAllString(string(b),
		"[install]\nmethod = \"local\"\npath = \""+itest.FakeAgent(t)+"\"\nexe = \"bin/fakeagent\"\nneeds = [\"node\"]\nversion_cmd = [\"--version\"]\n\n[home]")
	e.WriteConfig("harness/pi.toml", manifest)
	install(t, e, "pi")
	ref := itest.RealmA + "/pip"
	project(e, ref, "env = \""+itest.EnvBase+"\"\nharnesses = [\"pi\"]\n")
	e.MustRun("up", ref)
	if got := runOK(t, e, ref, "/opt/devc/node/current/bin/node", "-e", "console.log(6*7)"); got != "42" {
		t.Errorf("node in the box: %q", got)
	}
	got := runOK(t, e, ref, "pi", "-p", "sh node --version; echo PATH=$PATH; echo DIR=$PI_CODING_AGENT_DIR; "+
		"test -s /etc/pi/devc-instructions.md && echo instructions; grep -q box.input.register /etc/pi/devc-input.ts && echo extension")
	for _, want := range []string{"v24.22.3", "PATH=/opt/devc/node/24.22.3/bin:", "DIR=/home/dev/.pi/agent", "instructions", "extension"} {
		if !strings.Contains(got, want) {
			t.Errorf("pi -p: %q lacks %q", got, want)
		}
	}
	// The shared home comes from the realm's state.
	runOK(t, e, ref, "pi", "-p", "sh touch $PI_CODING_AGENT_DIR/marker")
	if _, err := os.Stat(filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "harness", "pi", "home", "marker")); err != nil {
		t.Errorf("pi's shared home: %v", err)
	}
}
