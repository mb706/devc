//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/mb706/devc/test/integration/itest"
)

// dataProject is the project definition of the data tests: one zone of
// each kind, restic savepoints with keep = 2, the fake harness.
const dataProject = `harnesses = ["fake"]
[data]
inputs = ["raw"]
work = ["scratch"]
sealed = ["results"]
[data.savepoints]
driver = "restic"
keep = 2
before_session = true
`

type dataListing struct {
	Root     string `json:"root"`
	HostRoot string `json:"host_root"`
	Zones    []struct {
		Kind   string `json:"kind"`
		Path   string `json:"path"`
		Exists bool   `json:"exists"`
		Files  int64  `json:"files"`
	} `json:"zones"`
	Sealed []struct {
		Path  string `json:"path"`
		Time  string `json:"time"`
		Files int64  `json:"files"`
		Size  int64  `json:"size"`
	} `json:"sealed"`
	Unsealed   []string `json:"unsealed"`
	Savepoints []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Driver string `json:"driver"`
	} `json:"savepoints"`
	Trash struct {
		Entries int `json:"entries"`
		Items   []struct {
			ID     string   `json:"id"`
			Reason string   `json:"reason"`
			Paths  []string `json:"paths"`
		} `json:"items"`
	} `json:"trash"`
}

func dataLs(t *testing.T, e *itest.Env, ref string) dataListing {
	t.Helper()
	var l dataListing
	if err := json.Unmarshal([]byte(e.MustRun("data", "ls", ref, "--json").Stdout), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// hostUID maps an in-box UID (keep-id:uid=1000) to the host UID that owns
// its files: in-box 1000 is the user, every other in-box UID n is n in the
// rootless user namespace.
func hostUID(t *testing.T, inBox int) int {
	t.Helper()
	if inBox == 1000 {
		return os.Getuid()
	}
	for _, l := range strings.Split(itest.Podman(t, "unshare", "cat", "/proc/self/uid_map"), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		in, _ := strconv.Atoi(f[0])
		host, _ := strconv.Atoi(f[1])
		n, _ := strconv.Atoi(f[2])
		if inBox >= in && inBox < in+n {
			return host + inBox - in
		}
	}
	t.Fatalf("in-box UID %d is not mapped", inBox)
	return -1
}

func hostStat(t *testing.T, p string) (int, os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return int(fi.Sys().(*syscall.Stat_t).Uid), fi.Mode() & (os.ModePerm | os.ModeSticky)
}

// fails runs a shell command in the box and reports whether it failed.
func fails(e *itest.Env, ref, script string) bool {
	return e.Run("run", ref, "--", "sh", "-c", script).ExitCode != 0
}

// expectAllFail runs each script as dev in the box; every one must fail.
func expectAllFail(t *testing.T, e *itest.Env, ref, what string, scripts []string) {
	t.Helper()
	for _, script := range scripts {
		if !fails(e, ref, script) {
			t.Errorf("from the box, %q worked %s", script, what)
		}
	}
}

// layoutEscapes are box commands against the layout of a data project
// (project directory bdir, data root data/, zones raw, scratch, results):
// moving or removing the vault, the data root or a zone directory, and
// listing or entering the vault. Shared by the data tests and the escape
// suite.
func layoutEscapes(bdir string) []string {
	return []string{
		"mv " + bdir + "/.devc " + bdir + "/x", "rmdir " + bdir + "/.devc", "ls " + bdir + "/.devc", "cd " + bdir + "/.devc",
		"mv " + bdir + "/data " + bdir + "/d2", "rmdir " + bdir + "/data",
		"mv " + bdir + "/data/raw " + bdir + "/data/r2", "mv " + bdir + "/data/scratch " + bdir + "/data/s2",
		"mv " + bdir + "/data/results " + bdir + "/data/r3", "rmdir " + bdir + "/data/scratch",
	}
}

// sealedEscapes are box commands against a sealed entry: delete,
// overwrite, append, truncate, rename, chmod, replace, add and remove
// files. file and nested are a file directly in the entry and one in a
// subdirectory.
func sealedEscapes(entry, file, nested string) []string {
	parent := entry[:strings.LastIndex(entry, "/")]
	return []string{
		"rm -rf " + entry, "mv " + entry + " " + parent + "/x", "chmod u+w " + entry, "chmod u+w " + entry + "/" + file,
		"echo x > " + entry + "/" + file, "truncate -s 0 " + entry + "/" + file, "echo x >> " + entry + "/" + file,
		"touch " + entry + "/new", "rm " + entry + "/" + nested, "mv " + entry + "/" + file + " " + entry + "/moved",
		"mkdir " + parent + "/decoy && { mv -T " + parent + "/decoy " + entry + "; r=$?; rmdir " + parent + "/decoy 2>/dev/null; exit $r; }",
		"ln -sfn /tmp " + entry,
	}
}

// TestData covers work order C06 section 6, items 1-7 and 9-12; item 8
// (reflink) is skipped on filesystems that cannot clone files.
func TestData(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	ref := analysis
	project(e, ref, dataProject)
	e.MustRun("up", ref)
	pdir := filepath.Join(e.ProjectsRoot, strings.Replace(ref, "/", string(filepath.Separator), 1))
	droot := filepath.Join(pdir, "data")
	bdir := inBox(ref)
	vault := hostUID(t, 2000)

	// 1. Setup: owners and modes, seen from the host and from the box.
	for rel, want := range map[string]struct {
		uid  int
		mode os.FileMode
	}{
		".":            {vault, 0o770 | os.ModeSticky},
		".devc":        {vault, 0o700},
		"data":         {vault, 0o770 | os.ModeSticky},
		"data/raw":     {os.Getuid(), 0o755},
		"data/scratch": {vault, 0o775},
		"data/results": {vault, 0o770 | os.ModeSticky},
		"..":           {os.Getuid(), 0o700},
	} {
		if uid, mode := hostStat(t, filepath.Join(pdir, rel)); uid != want.uid || mode != want.mode {
			t.Errorf("host view of %s: uid %d mode %v, want %d %v", rel, uid, mode, want.uid, want.mode)
		}
	}
	if got := boxSh(t, e, ref, "cd "+bdir+" && stat -c '%u:%g %a' . .devc data data/raw data/scratch data/results | tr '\\n' ' '"); got != "2000:1000 1770 2000:2000 700 2000:1000 1770 1000:1000 755 2000:1000 775 2000:1000 1770" {
		t.Errorf("box view: %q", got)
	}
	expectAllFail(t, e, ref, "on the data layout", layoutEscapes(bdir))

	// 2. Input zone: read-only in the box, writable from the host.
	if !fails(e, ref, "touch "+bdir+"/data/raw/x") {
		t.Error("the box wrote the input zone")
	}
	writeFile(t, filepath.Join(droot, "raw", "in.csv"), "1,2,3\n")
	if got := boxSh(t, e, ref, "cat "+bdir+"/data/raw/in.csv"); got != "1,2,3" {
		t.Errorf("host file in the box: %q", got)
	}

	// 10. before_session: two sessions in a row take one savepoint.
	e.MustRun("toolbox", "install", "restic")
	e.MustRun("fake", ref, "--detach")
	e.MustRun("fake", ref, "--detach")
	if sps := dataLs(t, e, ref).Savepoints; len(sps) != 1 || sps[0].Name != "" || sps[0].Driver != "restic" {
		t.Errorf("savepoints after two sessions: %+v", sps)
	}
	// What the daemon runs before it starts a session (finding G4-17):
	// nothing new while the owner's savepoint is recent.
	if res := e.MustRun("save", "--before-session", "--", ref); strings.Contains(res.Stderr, "saved") || len(dataLs(t, e, ref).Savepoints) != 1 {
		t.Errorf("save --before-session with a recent savepoint: %s", res.Stderr)
	}

	// 3. Seal.
	boxSh(t, e, ref, "cd "+bdir+"/data/results && mkdir -p run1/sub && echo hello > run1/a.txt && echo b > run1/sub/b.txt")
	res := e.MustRun("seal", "results/run1", ref)
	if !strings.Contains(res.Stdout, "sealed results/run1: 2 files") {
		t.Errorf("seal: %q", res.Stdout)
	}
	r1 := bdir + "/data/results/run1"
	expectAllFail(t, e, ref, "on a sealed entry", sealedEscapes(r1, "a.txt", "sub/b.txt"))
	if got := boxSh(t, e, ref, "cat "+r1+"/a.txt && mkdir "+bdir+"/data/results/run2 && echo ok"); got != "hello\nok" {
		t.Errorf("reading and a new sibling: %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(droot, "results", "run1", "a.txt")); err != nil || string(b) != "hello\n" {
		t.Errorf("host read: %q %v", b, err)
	}
	if uid, mode := hostStat(t, filepath.Join(droot, "results", "run1")); uid != vault || mode != 0o555 {
		t.Errorf("sealed entry on the host: %d %v", uid, mode)
	}
	seals := itest.Podman(t, "unshare", "cat", filepath.Join(pdir, ".devc", "seals.jsonl"))
	if !strings.Contains(seals, `"op":"seal","path":"results/run1"`) || !strings.Contains(seals, `"files":2`) {
		t.Errorf("seals.jsonl: %s", seals)
	}
	if res := e.MustRun("seal", r1, ref); !strings.Contains(res.Stdout, "sealed already") {
		t.Errorf("sealing again: %q", res.Stdout)
	}

	// 4. Refusals.
	boxSh(t, e, ref, "ln -s run1 "+bdir+"/data/results/link")
	for path, want := range map[string]string{
		"scratch/x":            "work zone",
		"results/run1/sub":     "nested",
		"results/link":         "symlink",
		"results/../scratch/x": "..",
		"../x":                 "..",
		"results/nope":         "does not exist",
	} {
		res := e.Run("seal", path, ref)
		if res.ExitCode != 1 || !strings.Contains(res.Stderr, want) {
			t.Errorf("seal %s: exit %d %q (want %q)", path, res.ExitCode, res.Stderr, want)
		}
	}

	// 5. Hostile tree: nothing outside the tree changes owner or mode.
	marker := filepath.Join(droot, "scratch", "sib", "marker")
	boxSh(t, e, ref, "mkdir -p "+bdir+"/data/scratch/sib && echo m > "+bdir+"/data/scratch/sib/marker && chmod 640 "+bdir+"/data/scratch/sib/marker")
	boxSh(t, e, ref, strings.Join([]string{
		"cd " + bdir + "/data/results && mkdir run3 && cd run3",
		"ln -s " + marker + " host-file", "ln -s " + bdir + "/data/scratch/sib/marker box-file", "ln -s ../../scratch/sib dir-outside",
		"mkfifo fifo", "echo s > secret", "chmod 000 secret"}, " && "))
	res = e.MustRun("seal", "results/run3", ref)
	if !strings.Contains(res.Stderr, "results/run3/fifo") {
		t.Errorf("the FIFO was not reported: %q", res.Stderr)
	}
	if uid, mode := hostStat(t, marker); uid != os.Getuid() || mode != 0o640 {
		t.Errorf("marker changed: %d %v", uid, mode)
	}
	if uid, mode := hostStat(t, filepath.Dir(marker)); uid != os.Getuid() || mode != 0o755 {
		t.Errorf("sibling directory changed: %d %v", uid, mode)
	}
	if l, _ := os.Readlink(filepath.Join(droot, "results", "run3", "dir-outside")); l != "../../scratch/sib" {
		t.Errorf("link target changed: %q", l)
	}
	if got := boxSh(t, e, ref, "stat -c '%u %a' "+bdir+"/data/results/run3/secret"); got != "2000 444" {
		t.Errorf("unreadable file after sealing: %q", got)
	}

	// 6. Unseal, trash, restore from the trash, empty the trash.
	e.MustRun("data", "unseal", "results/run1", ref)
	boxSh(t, e, ref, "echo more >> "+r1+"/a.txt && touch "+r1+"/new")
	e.MustRun("seal", "results/run1", ref)
	res = e.MustRun("data", "rm", "results/run1", ref)
	l := dataLs(t, e, ref)
	if l.Trash.Entries != 1 || !slices.Equal(l.Trash.Items[0].Paths, []string{"results/run1"}) {
		t.Errorf("trash after rm: %+v", l.Trash)
	}
	if !fails(e, ref, "test -e "+r1) {
		t.Error("the removed entry is still there")
	}
	id := l.Trash.Items[0].ID
	if !strings.Contains(res.Stdout, id) {
		t.Errorf("rm output %q lacks %s", res.Stdout, id)
	}
	e.MustRun("data", "trash", "restore", id, ref)
	if got := boxSh(t, e, ref, "cat "+r1+"/a.txt; stat -c %u "+r1); got != "hello\nmore\n2000" {
		t.Errorf("restored from the trash: %q", got)
	}
	e.MustRun("data", "rm", "results/run2", ref)
	if res := e.Run("data", "trash", "empty", ref); res.ExitCode == 0 {
		t.Error("trash empty without a terminal and without --yes worked")
	}
	e.MustRun("data", "trash", "empty", ref, "--yes")
	if l := dataLs(t, e, ref); l.Trash.Entries != 0 {
		t.Errorf("trash after emptying: %+v", l.Trash)
	}

	// 7. restic: save, change, restore a copy, restore in place.
	boxSh(t, e, ref, "cd "+bdir+"/data/scratch && echo v1 > f1 && echo v1 > f2 && echo same > same")
	res = e.MustRun("save", "base", ref)
	if !strings.Contains(res.Stdout, "-base (restic") {
		t.Fatalf("save: %q", res.Stdout)
	}
	sp := strings.Fields(res.Stdout)[1]
	boxSh(t, e, ref, "cd "+bdir+"/data/scratch && echo v2 > f1 && rm f2")
	res = e.MustRun("restore", sp, "--project", ref)
	if got := boxSh(t, e, ref, "cat "+bdir+"/data/scratch/restored-"+sp+"/scratch/f1 "+bdir+"/data/scratch/restored-"+sp+"/scratch/f2"); got != "v1\nv1" {
		t.Errorf("restored copy: %q (%s)", got, res.Stdout)
	}
	if !fails(e, ref, "test -e "+bdir+"/data/scratch/restored-"+sp+"/results/run1") || !fails(e, ref, "test -e "+bdir+"/data/scratch/restored-"+sp+"/raw") {
		t.Error("the savepoint covered a sealed entry or an input zone")
	}
	res = e.MustRun("restore", sp, "--in-place", "-p", ref)
	if got := boxSh(t, e, ref, "cd "+bdir+"/data/scratch && cat f1 f2 && stat -c %u f1"); got != "v1\nv1\n1000" {
		t.Errorf("restored in place: %q (%s)", got, res.Stdout)
	}
	l = dataLs(t, e, ref)
	if l.Trash.Entries != 1 || l.Trash.Items[0].Reason != "restore" || !slices.Contains(l.Trash.Items[0].Paths, "scratch/f1") || slices.Contains(l.Trash.Items[0].Paths, "scratch/same") {
		t.Errorf("trash after an in-place restore: %+v", l.Trash)
	}
	repo := filepath.Join(e.StateDir, "local", "savepoints")
	for _, m := range inspect(t, ref).Mounts {
		if strings.HasPrefix(m.Source, repo) || strings.HasPrefix(m.Source, filepath.Join(e.StateDir, "local", "tmp")) {
			t.Errorf("the box mounts %s", m.Source)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, itest.RealmA, "analysis", "restic", "config")); err != nil {
		t.Errorf("restic repository: %v", err)
	}

	// 9. Retention with keep = 2 and one named savepoint.
	for i := 0; i < 3; i++ {
		e.MustRun("save", ref)
	}
	l = dataLs(t, e, ref)
	var named, unnamed int
	for _, s := range l.Savepoints {
		if s.Name == "base" {
			named++
		} else if s.Name == "" {
			unnamed++
		}
	}
	if named != 1 || unnamed != 2 || len(l.Savepoints) != 3 {
		t.Errorf("savepoints after retention: %+v", l.Savepoints)
	}
	e.MustRun("data", "gc", ref, "--drop", "base")
	if l := dataLs(t, e, ref); len(l.Savepoints) != 2 {
		t.Errorf("savepoints after gc --drop: %+v", l.Savepoints)
	}

	// 11. data ls --json.
	l = dataLs(t, e, ref)
	if l.Root != bdir+"/data" || l.HostRoot != droot || len(l.Zones) != 3 || !l.Zones[0].Exists || l.Zones[0].Files != 1 {
		t.Errorf("listing: %+v", l)
	}
	var sealed []string
	for _, s := range l.Sealed {
		sealed = append(sealed, s.Path)
		if s.Time == "" {
			t.Errorf("sealed entry without time: %+v", s)
		}
	}
	if !slices.Equal(sealed, []string{"results/run1", "results/run3"}) || !slices.Contains(l.Unsealed, "results/link") {
		t.Errorf("sealed %v, unsealed %v", sealed, l.Unsealed)
	}

	// 8. reflink, where the filesystem can clone files.
	t.Run("reflink", func(t *testing.T) {
		project(e, ref, strings.Replace(dataProject, `"restic"`, `"reflink"`, 1))
		res := e.Run("save", ref)
		if res.ExitCode != 0 && strings.Contains(res.Stderr, "cannot clone files") {
			t.Skip("the projects filesystem cannot clone files (reflink); see LEFTOVER.md")
		}
		if res.ExitCode != 0 {
			t.Fatalf("save: %s", res.Stderr)
		}
		id := strings.Fields(res.Stdout)[1]
		if !fails(e, ref, "ls "+bdir+"/.devc/savepoints/"+id) {
			t.Error("the box can read the reflink savepoint")
		}
		boxSh(t, e, ref, "echo v9 > "+bdir+"/data/scratch/f1")
		e.MustRun("restore", id, "scratch/f1", "-p", ref)
		if got := boxSh(t, e, ref, "cat "+bdir+"/data/scratch/restored-"+id+"/scratch/f1"); got != "v1" {
			t.Errorf("reflink restore: %q", got)
		}
	})
	project(e, ref, dataProject)

	// 12. Worktree data links.
	e.MustRun("project", "init", ref)
	boxSh(t, e, ref, "cd "+bdir+"/main && git commit -q --allow-empty -m first")
	e.MustRun("wt", "new", "t1", ref)
	for _, wt := range []string{"main", "t1"} {
		if got := boxSh(t, e, ref, "cd "+bdir+"/"+wt+" && readlink data && test -d data/scratch && git status --porcelain"); got != "../data" {
			t.Errorf("%s: link and status %q", wt, got)
		}
	}
	// up adds links to worktrees that lack them.
	boxSh(t, e, ref, "rm "+bdir+"/t1/data")
	e.MustRun("down", ref)
	e.MustRun("up", ref)
	if got := boxSh(t, e, ref, "readlink "+bdir+"/t1/data"); got != "../data" {
		t.Errorf("link after up: %q", got)
	}
}

// TestDataNone covers item 13: a project without [data] keeps an ordinary
// box-owned project directory, and data commands refuse it.
func TestDataNone(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.MustRun("up", hello)
	pdir := filepath.Join(e.ProjectsRoot, itest.RealmA, "hello")
	if uid, mode := hostStat(t, pdir); uid != os.Getuid() || mode != 0o755 {
		t.Errorf("project directory: %d %v", uid, mode)
	}
	if _, err := os.Lstat(filepath.Join(pdir, ".devc")); !os.IsNotExist(err) {
		t.Errorf(".devc exists: %v", err)
	}
	if got := runOK(t, e, hello, "stat", "-c", "%u", "/w/"+hello); got != "1000" {
		t.Errorf("box view: %q", got)
	}
	if res := e.Run("seal", "x", hello); res.ExitCode != 1 || !strings.Contains(res.Stderr, "no data zones") {
		t.Errorf("seal on a project without data: %d %q", res.ExitCode, res.Stderr)
	}
}
