//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/gate"
	"github.com/mb706/devc/test/integration/itest"
)

// rsyncLog puts an rsync wrapper first in PATH that records every
// invocation's arguments (NUL-separated, one line each) and runs the real
// rsync.
func rsyncLog(t *testing.T, e *itest.Env) (log string) {
	t.Helper()
	real, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync is not installed on this host; the data sync tests need it")
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "rsync.log")
	writeFile(t, filepath.Join(dir, "bin", "rsync"), fmt.Sprintf("#!/bin/sh\n{ printf '%%s\\0' \"$@\"; printf '\\n'; } >> '%s'\nexec '%s' \"$@\"\n", log, real))
	e.ExtraEnv = append(e.ExtraEnv, "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"))
	return log
}

// rsyncRuns reads the wrapper's log; it is emptied afterwards.
func rsyncRuns(t *testing.T, log string) [][]string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	os.Remove(log)
	var runs [][]string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		runs = append(runs, strings.Split(strings.TrimSuffix(line, "\x00"), "\x00"))
	}
	return runs
}

func operands(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			out = append(out, a)
		}
	}
	return out
}

// hostTree describes a host directory (the local remote): every entry with
// its type, mode, size, content hash or link target.
func hostTree(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		extra := ""
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			extra, _ = os.Readlink(p)
		case fi.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			s := sha256.Sum256(b)
			extra = hex.EncodeToString(s[:8])
		}
		lines = append(lines, fmt.Sprintf("%s %v %d %d %s", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano(), extra))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

type syncStatus struct {
	Machine string `json:"machine"`
	Entries []struct {
		Path  string `json:"path"`
		State string `json:"state"`
		Files int64  `json:"files"`
		Last  *struct {
			Action string `json:"action"`
			Remote string `json:"remote"`
		} `json:"last"`
		Machines []struct {
			Name string `json:"name"`
			Same bool   `json:"same"`
		} `json:"machines"`
		Remotes []struct {
			Name   string `json:"name"`
			Action string `json:"action"`
			Same   bool   `json:"same"`
			Sent   bool   `json:"sent"`
		} `json:"remotes"`
	} `json:"entries"`
}

func dataStatus(t *testing.T, e *itest.Env, ref, path string) (state string, remotes map[string]bool, last string) {
	t.Helper()
	var st syncStatus
	if err := json.Unmarshal([]byte(e.MustRun("data", "status", ref, "--json").Stdout), &st); err != nil {
		t.Fatal(err)
	}
	for _, en := range st.Entries {
		if en.Path != path {
			continue
		}
		// A remote counts as holding the entry as it is now when a pull
		// received it so or a push sent it so (finding D2-12).
		remotes = map[string]bool{}
		for _, r := range en.Remotes {
			remotes[r.Name] = r.Same || r.Sent
		}
		if en.Last != nil {
			last = en.Last.Action + " " + en.Last.Remote
		}
		return en.State, remotes, last
	}
	t.Fatalf("data status has no entry %s", path)
	return "", nil, ""
}

type manifestLine struct {
	Machine, Action, Path, Remote, Digest, Content string
	Files                                          int64
}

func readManifest(t *testing.T, e *itest.Env, ref string) []manifestLine {
	t.Helper()
	realm, name, _ := strings.Cut(ref, "/")
	b, err := os.ReadFile(filepath.Join(e.StateDir, "durable", "realms", realm, "context", "data", name, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []manifestLine
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m manifestLine
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("manifest line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

// TestDataSync covers work order C14 section 5, integration items 1-6;
// item 7 (SSH with a master connection) runs as a subtest when an OpenSSH
// server is installed.
func TestDataSync(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	log := rsyncLog(t, e)
	e.Timeout = 4 * time.Minute
	ref := itest.RealmA + "/syncp"
	base := t.TempDir()
	nas := filepath.Join(base, "nas")
	outside := filepath.Join(base, "outside")
	writeFile(t, filepath.Join(outside, "secret"), "outside the data root\n")
	os.Chmod(filepath.Join(outside, "secret"), 0o600)
	project(e, ref, fmt.Sprintf("harnesses = []\n[data]\ninputs = [\"raw\"]\nwork = [\"scratch\"]\nsealed = [\"results\"]\n[data.remotes.nas]\npath = %q\npull = [\"scratch\"]\npush = [\"scratch\"]\n", nas))
	cfg, err := os.ReadFile(filepath.Join(e.ConfigDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "\nmachine_name = \"\"") {
		t.Fatal("config.toml has no machine_name setting to replace")
	}
	e.WriteConfig("config.toml", strings.Replace(string(cfg), "\nmachine_name = \"\"", "\nmachine_name = \"itws\"", 1))
	e.MustRun("up", ref)
	pdir := filepath.Join(e.ProjectsRoot, strings.Replace(ref, "/", string(filepath.Separator), 1))
	droot := filepath.Join(pdir, "data")
	bdir := inBox(ref)
	stagingRoot := filepath.Join(e.StateDir, "local", "staging", itest.RealmA, "syncp")

	// The remote: new, differing, identical files, a nested file, and a
	// symlink pointing outside everything.
	writeFile(t, filepath.Join(nas, "scratch", "new.txt"), "new\n")
	writeFile(t, filepath.Join(nas, "scratch", "diff.txt"), "remote\n")
	writeFile(t, filepath.Join(nas, "scratch", "same.txt"), "same\n")
	writeFile(t, filepath.Join(nas, "scratch", "dir", "f.txt"), "f\n")
	writeFile(t, filepath.Join(nas, "scratch", "remote-only.txt"), "stays\n")
	for _, f := range []string{"new.txt", "diff.txt", "same.txt", "dir/f.txt", "remote-only.txt"} {
		os.Chmod(filepath.Join(nas, "scratch", f), 0o644)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(nas, "scratch", "out")); err != nil {
		t.Fatal(err)
	}
	boxSh(t, e, ref, "cd "+bdir+"/data/scratch && echo local > diff.txt && echo same > same.txt && touch -d 2020-01-01 same.txt && echo mine > mine.txt")
	sameBefore := boxSh(t, e, ref, "stat -c '%i %Y' "+bdir+"/data/scratch/same.txt")
	outsideBefore := hostTree(t, outside)

	// 5. Dry runs change nothing on either side.
	boxTree := func() string {
		return boxSh(t, e, ref, "cd "+bdir+"/data && find . -printf '%p %y %s %T@ %u %m %l\\n' | sort")
	}
	remoteBefore, localBefore := hostTree(t, nas), boxTree()
	res := e.MustRun("data", "pull", "nas", "-p", ref, "--dry-run")
	if !strings.Contains(res.Stdout, "scratch/new.txt") || !strings.Contains(res.Stdout, "dry run") {
		t.Errorf("pull --dry-run: %q", res.Stdout)
	}
	res = e.MustRun("data", "push", "nas", "-p", ref, "--dry-run")
	if !strings.Contains(res.Stdout, "scratch/mine.txt") || !strings.Contains(res.Stdout, "dry run") || !strings.Contains(res.Stdout, ".devc-backup/") {
		t.Errorf("push --dry-run: %q", res.Stdout)
	}
	if hostTree(t, nas) != remoteBefore || boxTree() != localBefore {
		t.Error("a dry run changed something")
	}
	if _, err := os.Lstat(filepath.Join(pdir, ".devc-staging")); err == nil {
		t.Error("a dry push left its staging directory")
	}
	if ents, _ := os.ReadDir(stagingRoot); len(ents) != 0 {
		t.Errorf("a dry pull left staging directories: %v", ents)
	}
	// Without --yes and without a terminal, nothing is transferred.
	if res := e.Run("data", "pull", "nas", "-p", ref); res.ExitCode == 0 || !strings.Contains(res.Stderr, "--yes") {
		t.Errorf("pull without a terminal: exit %d %q", res.ExitCode, res.Stderr)
	}
	if hostTree(t, nas) != remoteBefore || boxTree() != localBefore {
		t.Error("an unconfirmed pull changed something")
	}
	rsyncRuns(t, log)

	// 1. Pull into the work zone.
	res = e.MustRun("data", "pull", "nas", "-p", ref, "--yes")
	if !strings.Contains(res.Stdout, "pulled") || !strings.Contains(res.Stdout, "trash entry") {
		t.Errorf("pull: %q %q", res.Stdout, res.Stderr)
	}
	if got := boxSh(t, e, ref, "cd "+bdir+"/data/scratch && cat diff.txt new.txt dir/f.txt && stat -c '%u:%g %a' new.txt dir dir/f.txt diff.txt && readlink out"); got !=
		"remote\nnew\nf\n1000:1000 644\n1000:1000 755\n1000:1000 644\n1000:1000 644\n"+filepath.Join(outside, "secret") {
		t.Errorf("pulled files in the box: %q", got)
	}
	if uid, _ := hostStat(t, filepath.Join(droot, "scratch", "new.txt")); uid != os.Getuid() {
		t.Errorf("pulled file owned by host UID %d", uid)
	}
	if got := boxSh(t, e, ref, "stat -c '%i %Y' "+bdir+"/data/scratch/same.txt"); got != sameBefore {
		t.Errorf("the identical file was touched: %q, was %q", got, sameBefore)
	}
	if hostTree(t, outside) != outsideBefore {
		t.Error("something outside the data root changed")
	}
	l := dataLs(t, e, ref)
	if l.Trash.Entries != 1 || l.Trash.Items[0].Reason != "pull" || !slices.Equal(l.Trash.Items[0].Paths, []string{"scratch/diff.txt"}) {
		t.Errorf("trash after the pull: %+v", l.Trash)
	}
	if ents, _ := os.ReadDir(stagingRoot); len(ents) != 0 {
		t.Errorf("the pull left its staging directory: %v", ents)
	}
	for _, run := range rsyncRuns(t, log) {
		ops := operands(run)
		if !strings.HasPrefix(ops[len(ops)-1], stagingRoot+"/") || slices.Contains(run, "--delete") {
			t.Errorf("pull ran rsync with %q", run)
		}
	}
	var pulls int
	for _, m := range readManifest(t, e, ref) {
		if m.Action == "pull" && m.Remote == "nas" && m.Machine == "itws" {
			pulls++
		}
	}
	if pulls != 6 { // scratch/{diff,dir,new,out,remote-only,same}
		t.Errorf("%d pull records", pulls)
	}

	// 2. A target inside a sealed entry is refused.
	boxSh(t, e, ref, "mkdir -p "+bdir+"/data/results/run1 && echo r > "+bdir+"/data/results/run1/a.txt")
	e.MustRun("seal", "results/run1", ref, "--content")
	writeFile(t, filepath.Join(nas, "results", "run1", "a.txt"), "other\n")
	res = e.Run("data", "pull", "nas", "results/run1/a.txt", "-p", ref, "--yes")
	if res.ExitCode != 1 || !strings.Contains(res.Stderr, "sealed") {
		t.Errorf("pull into a sealed entry: exit %d %q", res.ExitCode, res.Stderr)
	}
	if got := boxSh(t, e, ref, "cat "+bdir+"/data/results/run1/a.txt"); got != "r" {
		t.Errorf("sealed file after the refused pull: %q", got)
	}
	if runs := rsyncRuns(t, log); len(runs) != 0 {
		t.Errorf("rsync ran for a refused pull: %q", runs)
	}
	if res := e.Run("data", "verify", "results/run1", ref, "--content"); res.ExitCode != 0 || !strings.Contains(res.Stdout, "content matches") {
		t.Errorf("verify --content: exit %d %q", res.ExitCode, res.Stdout)
	}

	// 3 and 4. Push: the remote receives the files, keeps what is replaced
	// in its backup directory, deletes nothing; rsync reads only the
	// staging directory.
	boxSh(t, e, ref, "cd "+bdir+"/data/scratch && echo changed > same.txt && rm new.txt")
	res = e.MustRun("data", "push", "nas", "-p", ref, "--yes")
	if !strings.Contains(res.Stdout, "pushed") || !strings.Contains(res.Stdout, ".devc-backup/") {
		t.Errorf("push: %q %q", res.Stdout, res.Stderr)
	}
	for f, want := range map[string]string{"scratch/mine.txt": "mine\n", "scratch/same.txt": "changed\n", "scratch/new.txt": "new\n", "scratch/remote-only.txt": "stays\n"} {
		if b, err := os.ReadFile(filepath.Join(nas, f)); err != nil || string(b) != want {
			t.Errorf("remote %s: %q %v", f, b, err)
		}
	}
	backups, _ := filepath.Glob(filepath.Join(nas, ".devc-backup", "*", "scratch", "same.txt"))
	if len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); string(b) != "same\n" {
		t.Errorf("backup of the replaced file: %q", b)
	}
	if l, err := os.Readlink(filepath.Join(nas, "scratch", "out")); err != nil || l != filepath.Join(outside, "secret") {
		t.Errorf("remote link %q %v", l, err)
	}
	runs := rsyncRuns(t, log)
	if len(runs) != 2 {
		t.Errorf("push ran rsync %d times", len(runs))
	}
	for _, run := range runs {
		ops := operands(run)
		for _, src := range ops[:len(ops)-1] {
			if !strings.HasPrefix(src, pdir+"/.devc-staging/push-") || !strings.HasSuffix(src, "/./scratch") {
				t.Errorf("push source %q is not the staging directory", src)
			}
		}
		if ops[len(ops)-1] != nas+"/" {
			t.Errorf("push destination %q", ops[len(ops)-1])
		}
		for _, a := range run {
			if strings.HasPrefix(a, droot) || strings.Contains(a, "delete") {
				t.Errorf("push argument %q", a)
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(pdir, ".devc-staging")); err == nil {
		t.Error("the push left its staging directory")
	}
	// A staging directory the box made is refused, never used.
	boxSh(t, e, ref, "mkdir "+bdir+"/.devc-staging")
	if res := e.Run("data", "push", "nas", "-p", ref, "--yes"); res.ExitCode != 1 || !strings.Contains(res.Stderr, "did not create") {
		t.Errorf("push with a box-made staging directory: exit %d %q", res.ExitCode, res.Stderr)
	}
	if runs := rsyncRuns(t, log); len(runs) != 0 {
		t.Errorf("rsync ran: %q", runs)
	}
	boxSh(t, e, ref, "rmdir "+bdir+"/.devc-staging")

	// 6. data status shows where an entry was pushed, and a change made
	// in the box afterwards.
	state, remotes, last := dataStatus(t, e, ref, "scratch/same.txt")
	if state != "recorded" || !remotes["nas"] || last != "push nas" {
		t.Errorf("status after the push: %s %v %q", state, remotes, last)
	}
	boxSh(t, e, ref, "echo again >> "+bdir+"/data/scratch/same.txt")
	state, remotes, last = dataStatus(t, e, ref, "scratch/same.txt")
	if state != "changed" || remotes["nas"] || last != "push nas" {
		t.Errorf("status after a change in the box: %s %v %q", state, remotes, last)
	}
	if state, _, _ := dataStatus(t, e, ref, "results/run1"); state != "recorded" {
		t.Errorf("sealed entry: %s", state)
	}
	if res := e.Run("data", "verify", "scratch/same.txt", ref); res.ExitCode != 1 || !strings.Contains(res.Stdout, "DIFFERS") {
		t.Errorf("verify after a change: exit %d %q", res.ExitCode, res.Stdout)
	}
	if out := e.MustRun("data", "status", ref).Stdout; !strings.Contains(out, "machine itws") || !strings.Contains(out, "nas*") {
		t.Errorf("status table: %q", out)
	}
	// The manifest is committed with the context repository.
	if out := e.MustRun("context", "log", itest.RealmA).Stdout; !strings.Contains(out, "data/syncp/manifest.jsonl") {
		t.Errorf("context log: %q", out)
	}

	// 7. A pull through the SSH path, over the master connection of
	// devc ssh login.
	t.Run("ssh", func(t *testing.T) {
		sshd := sshTools(t)
		rem := startRemote(t, sshd)
		e.ExtraEnv = append(e.ExtraEnv, gate.EnvSSHConfig+"="+rem.config, rem.marker)
		remoteData := filepath.Join(rem.files, "remote")
		writeFile(t, filepath.Join(remoteData, "scratch", "ssh.txt"), "over ssh\n")
		os.Chmod(filepath.Join(remoteData, "scratch", "ssh.txt"), 0o644)
		appendConfig(t, e, projectFile(ref), fmt.Sprintf("[data.remotes.cl]\nhost = %q\npath = %q\n", rem.alias, remoteData))
		sock, err := gate.SocketPath(filepath.Join(e.RuntimeDir, "gate"), rem.alias)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cmd := exec.Command("ssh", "-F", rem.config, "-S", sock, "-O", "exit", rem.alias)
			cmd.Dir = "/"
			_ = cmd.Run()
		})
		if res := e.Run("ssh", "login", rem.alias, "--ttl", "10m"); res.ExitCode != 0 {
			t.Fatalf("ssh login: exit %d %s%s\n%s", res.ExitCode, res.Stdout, res.Stderr, rem.output())
		}
		// New connections are refused from now on: only the master works.
		writeFile(t, filepath.Join(rem.dir, "authorized"), "")
		res := e.Run("data", "pull", "cl", "scratch/ssh.txt", "-p", ref, "--yes")
		if res.ExitCode != 0 {
			t.Fatalf("pull over ssh: exit %d %s%s\n%s", res.ExitCode, res.Stdout, res.Stderr, rem.output())
		}
		if got := boxSh(t, e, ref, "cat "+bdir+"/data/scratch/ssh.txt; stat -c %u "+bdir+"/data/scratch/ssh.txt"); got != "over ssh\n1000" {
			t.Errorf("pulled over ssh: %q", got)
		}
		runs := rsyncRuns(t, log)
		if len(runs) != 2 {
			t.Fatalf("rsync runs %q", runs)
		}
		want := "--rsh=ssh -F " + rem.config + " -S " + sock + " -o ControlMaster=no"
		for _, run := range runs {
			if !slices.Contains(run, want) || !slices.Contains(run, rem.alias+":"+remoteData+"/./scratch/ssh.txt") {
				t.Errorf("rsync over the master: %q (want %q)", run, want)
			}
		}
		if out := e.MustRun("ssh", "close", rem.alias).Stdout; !strings.Contains(out, "closed") {
			t.Errorf("ssh close: %q", out)
		}
	})
}
