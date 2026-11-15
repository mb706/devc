package data

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
)

var fixedArgs = []string{"--recursive", "--links", "--times", "--no-owner", "--no-group", "--no-devices", "--no-specials", "--partial", "--relative", "--protect-args"}

func TestRsyncArgs(t *testing.T) {
	local := config.Remote{Path: "/mnt/nas/p/"}
	ssh := config.Remote{Host: "c1", Path: "scratch/p"}
	master := []string{"ssh", "-F", "/t/my config", "-S", "/r/gate/m-1.sock", "-o", "ControlMaster=no"}
	stage := "/p/r/x/.devc-staging/push-20261115T040500Z"
	cases := []struct {
		name string
		r    Rsync
		want []string
	}{
		{"pull from a directory", Rsync{Remote: local, Local: "/s/stage", Paths: []string{"results/run1", "raw"}},
			[]string{"--perms", "/mnt/nas/p/./results/run1", "/mnt/nas/p/./raw", "/s/stage/"}},
		{"dry pull over a master", Rsync{Remote: ssh, Local: "/s/stage", Paths: []string{"results"}, DryRun: true, Shell: master},
			[]string{"--perms", "--dry-run", "--out-format=%i %l %n", "--rsh=ssh -F '/t/my config' -S /r/gate/m-1.sock -o ControlMaster=no", "c1:scratch/p/./results", "/s/stage/"}},
		{"pull over ordinary ssh", Rsync{Remote: ssh, Local: "/s/stage", Paths: []string{"results"}, Progress: true},
			[]string{"--perms", "--info=progress2", "c1:scratch/p/./results", "/s/stage/"}},
		{"push to a directory", Rsync{Push: true, Remote: local, Local: stage, Paths: []string{"raw", "results/run1"}, BackupDir: ".devc-backup/20261115T040500Z"},
			[]string{"--no-perms", "--executability", "--chmod=D0777", "--backup", "--backup-dir=.devc-backup/20261115T040500Z", stage + "/./raw", stage + "/./results/run1", "/mnt/nas/p/"}},
		{"dry push over a master", Rsync{Push: true, Remote: ssh, Local: stage, Paths: []string{"raw"}, BackupDir: ".devc-backup/20261115T040500Z", DryRun: true, Progress: true, Shell: master},
			[]string{"--no-perms", "--executability", "--chmod=D0777", "--backup", "--backup-dir=.devc-backup/20261115T040500Z", "--dry-run", "--out-format=%i %l %n",
				"--rsh=ssh -F '/t/my config' -S /r/gate/m-1.sock -o ControlMaster=no", stage + "/./raw", "c1:scratch/p/"}},
		{"remote root", Rsync{Remote: config.Remote{Path: "/"}, Local: "/s", Paths: []string{"raw"}}, []string{"--perms", "/./raw", "/s/"}},
	}
	for _, c := range cases {
		got, err := RsyncArgs(c.r)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if want := append(slices.Clone(fixedArgs), c.want...); !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, want)
		}
		for _, a := range got {
			if strings.Contains(a, "delete") || strings.HasPrefix(a, "--del") || a == "-L" || a == "--copy-links" || strings.HasPrefix(a, "--copy-dirlinks") {
				t.Errorf("%s: forbidden argument %q", c.name, a)
			}
			if !strings.HasPrefix(a, "--") && !strings.HasPrefix(a, "/") && !strings.HasPrefix(a, "c1:") {
				t.Errorf("%s: operand %q starts with neither / nor the host", c.name, a)
			}
		}
		// The data root is never an operand: the local side is the staging
		// directory.
		if c.r.Push && !strings.HasPrefix(got[len(got)-2], stage+"/./") {
			t.Errorf("%s: the source is not the staging directory: %q", c.name, got)
		}
	}
	good := Rsync{Remote: local, Local: "/s", Paths: []string{"raw"}}
	for name, change := range map[string]func(*Rsync){
		"option as host":      func(r *Rsync) { r.Remote.Host = "-oProxyCommand=x" },
		"relative local path": func(r *Rsync) { r.Remote.Path = "nas" },
		"remote wildcard":     func(r *Rsync) { r.Remote.Path = "/nas/*" },
		"relative staging":    func(r *Rsync) { r.Local = "s" },
		"unclean staging":     func(r *Rsync) { r.Local = "/s/../t" },
		"no paths":            func(r *Rsync) { r.Paths = nil },
		"wildcard path":       func(r *Rsync) { r.Paths = []string{"raw/*"} },
		"dot-dot path":        func(r *Rsync) { r.Paths = []string{"raw/../x"} },
		"backup dir path":     func(r *Rsync) { r.Paths = []string{".devc-backup/x"} },
		"pull with backup":    func(r *Rsync) { r.BackupDir = ".devc-backup/20261115T040500Z" },
		"push without backup": func(r *Rsync) { r.Push = true },
		"push bad backup":     func(r *Rsync) { r.Push, r.BackupDir = true, "../x" },
		"shell newline":       func(r *Rsync) { r.Shell = []string{"ssh", "a\nb"} },
		"empty shell word":    func(r *Rsync) { r.Shell = []string{"ssh", ""} },
	} {
		r := good
		r.Paths = slices.Clone(good.Paths)
		change(&r)
		if _, err := RsyncArgs(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if s, _ := rshCommand([]string{"ssh", "-S", "/r/it's here"}); s != "ssh -S '/r/it''s here'" {
		t.Errorf("quoting: %q", s)
	}
}

func TestParseItems(t *testing.T) {
	out := "cd+++++++++ 4096 results/\n>f+++++++++ 12 results/a.txt\n>f.st...... 30 results/b\\#033.txt\n<f+++++++++ 5 x y\n" +
		"cL+++++++++ 4 results/link\ngarbage\n>f+++++++++ -1 neg\n\n"
	items := ParseItems([]byte(out))
	if len(items) != 5 || items[3].Name != "x y" || items[2].Name != `results/b\#033.txt` {
		t.Fatalf("items %+v", items)
	}
	files, size, updated := Totals(items)
	if files != 3 || size != 47 || updated != 1 {
		t.Errorf("totals %d %d %d", files, size, updated)
	}
	if bad := ParseItems([]byte(">f+++++++++ 1 a\x1b[31m\n")); len(bad) != 1 || strings.ContainsRune(bad[0].Name, 0x1b) {
		t.Errorf("name not made printable: %+v", bad)
	}
}

func testDigest(n byte) string {
	return "sha256:" + strings.Repeat(string("0123456789abcdef"[n%16]), 64)
}

func TestManifest(t *testing.T) {
	w := newWorld(t, "restic")
	ctx := context.Background()
	p := w.svc.Config.Paths
	ref := w.p.Ref
	if recs, err := ReadManifest(p, ref); err != nil || len(recs) != 0 {
		t.Fatalf("empty manifest: %v %v", recs, err)
	}
	recs := []Record{
		{V: 1, Time: "2026-11-15T04:05:00Z", Machine: "ws", Action: ActionSeal, Path: "results/run1", Files: 2, Size: 3, Digest: testDigest(1), Content: testDigest(2)},
		{V: 1, Time: "2026-11-15T04:06:00Z", Machine: "cluster.node-1", Action: ActionPush, Path: "raw", Digest: testDigest(3), Remote: "nas"},
		{V: 1, Time: "2026-11-15T04:07:00Z", Machine: "ws", Action: ActionSave, Path: ".", Digest: testDigest(4), Savepoint: "20261115T040700Z"},
	}
	if err := AppendManifest(ctx, p, ref, recs); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(p, ref)
	if err != nil || !slices.Equal(got, recs) {
		t.Fatalf("read back %+v %v", got, err)
	}
	file := p.ManifestFile(ref)
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 || !strings.HasSuffix(file, "/durable/realms/r/context/data/p/manifest.jsonl") {
		t.Errorf("manifest file %s: %v %v", file, fi, err)
	}
	// Invalid records are refused on the way in and skipped on the way out.
	for name, r := range map[string]Record{
		"version":         {V: 2, Time: recs[0].Time, Machine: "ws", Action: ActionSeal, Path: "a", Digest: testDigest(1)},
		"time":            {V: 1, Time: "now", Machine: "ws", Action: ActionSeal, Path: "a", Digest: testDigest(1)},
		"machine":         {V: 1, Time: recs[0].Time, Machine: "a b", Action: ActionSeal, Path: "a", Digest: testDigest(1)},
		"action":          {V: 1, Time: recs[0].Time, Machine: "ws", Action: "copy", Path: "a", Digest: testDigest(1)},
		"path":            {V: 1, Time: recs[0].Time, Machine: "ws", Action: ActionSeal, Path: "../a", Digest: testDigest(1)},
		"digest":          {V: 1, Time: recs[0].Time, Machine: "ws", Action: ActionSeal, Path: "a", Digest: "md5:x"},
		"content":         {V: 1, Time: recs[0].Time, Machine: "ws", Action: ActionSeal, Path: "a", Digest: testDigest(1), Content: "x"},
		"pull no remote":  {V: 1, Time: recs[0].Time, Machine: "ws", Action: ActionPull, Path: "a", Digest: testDigest(1)},
		"seal remote":     {V: 1, Time: recs[0].Time, Machine: "ws", Action: ActionSeal, Path: "a", Digest: testDigest(1), Remote: "nas"},
		"remote name":     {V: 1, Time: recs[0].Time, Machine: "ws", Action: ActionPull, Path: "a", Digest: testDigest(1), Remote: "-x"},
		"save path":       {V: 1, Time: recs[0].Time, Machine: "ws", Action: ActionSave, Path: "a", Digest: testDigest(1), Savepoint: "20261115T040700Z"},
		"save no id":      {V: 1, Time: recs[0].Time, Machine: "ws", Action: ActionSave, Path: ".", Digest: testDigest(1)},
		"negative counts": {V: 1, Time: recs[0].Time, Machine: "ws", Action: ActionSeal, Path: "a", Digest: testDigest(1), Files: -1},
	} {
		if err := AppendManifest(ctx, p, ref, []Record{r}); err == nil {
			t.Errorf("%s: appended", name)
		}
	}
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n{\"v\":1,\"extra\":1}\n" + `{"v":1,"time":"2026-11-15T04:05:00Z","machine":"ws","action":"seal","path":"/etc","files":0,"size":0,"digest":"` + testDigest(1) + "\"}\n")
	f.Close()
	if got, err := ReadManifest(p, ref); err != nil || len(got) != 3 {
		t.Errorf("after junk: %d records, %v", len(got), err)
	}
	// A symlink in place of the manifest is neither written nor read.
	os.Rename(file, file+".old")
	os.Symlink(file+".old", file)
	if err := AppendManifest(ctx, p, ref, recs[:1]); err == nil {
		t.Error("appended through a symlink")
	}
	if _, err := ReadManifest(p, ref); err == nil {
		t.Error("read through a symlink")
	}
	os.Remove(file)
	os.Symlink(filepath.Join(w.dir, "elsewhere"), filepath.Dir(file)+"-x")
	os.Rename(filepath.Dir(file), filepath.Dir(file)+"-y")
	os.Symlink(filepath.Join(w.dir), filepath.Dir(file))
	if err := AppendManifest(ctx, p, ref, recs[:1]); err == nil {
		t.Error("appended through a symlinked directory")
	}
	if _, err := os.Stat(filepath.Join(w.dir, "manifest.jsonl")); err == nil {
		t.Error("a file was created through the symlinked directory")
	}
}

// syncWorld is a world with two remotes: a local directory and an SSH
// host.
func syncWorld(t *testing.T) *world {
	w := newWorldExtra(t, "restic", "[data.remotes.nas]\npath = \"@DIR@/nas\"\npull = [\"scratch\"]\npush = [\"scratch\"]\n[data.remotes.c1]\nhost = \"c1\"\npath = \"/scratch/p\"\n")
	if _, err := w.svc.Setup(context.Background(), w.p); err != nil {
		t.Fatal(err)
	}
	return w
}

// rsyncCalls returns the rsync invocations, and whether each was a dry run.
func (w *world) rsyncCalls() (calls [][]string, dry []bool) {
	for _, c := range w.host.Calls() {
		if c.Name == "rsync" {
			calls = append(calls, c.Args)
			dry = append(dry, slices.Contains(c.Args, "--dry-run"))
		}
	}
	return calls, dry
}

func (w *world) put(rel, content string) {
	w.t.Helper()
	p := filepath.Join(w.p.HostRoot(), rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
	os.Lchown(p, 1000, 1000)
	os.Lchown(filepath.Dir(p), 1000, 1000)
}

func countLocks(n *int) func(context.Context) (func(), error) {
	return func(context.Context) (func(), error) { *n++; return func() {}, nil }
}

func TestServicePull(t *testing.T) {
	w := syncWorld(t)
	ctx := context.Background()
	w.put("scratch/old.txt", "local")
	w.put("scratch/same.txt", "same")
	// The fake rsync lists two files in a dry run and, for the transfer,
	// writes them into the staging directory (its last operand).
	var staged []string
	w.host.On("rsync").Stdout(">f+++++++++ 6 scratch/old.txt\n>f+++++++++ 4 scratch/same.txt\n").Do(func(c execx.Cmd) {
		if slices.Contains(c.Args, "--dry-run") {
			return
		}
		dst := strings.TrimSuffix(c.Args[len(c.Args)-1], "/")
		staged = append(staged, dst)
		os.MkdirAll(filepath.Join(dst, "scratch"), 0o755)
		os.WriteFile(filepath.Join(dst, "scratch", "old.txt"), []byte("remote"), 0o644)
		os.WriteFile(filepath.Join(dst, "scratch", "same.txt"), []byte("same"), 0o644)
	})
	var shown []*SyncResult
	var asked int
	locks := 0
	opts := SyncOptions{Remote: "nas", Preview: func(r *SyncResult) { shown = append(shown, r) },
		Confirm: func(*SyncResult) (bool, error) { asked++; return true, nil }, Lock: countLocks(&locks)}
	res, err := w.svc.Pull(ctx, w.p, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(shown) != 1 || asked != 1 || res.Files != 2 || res.Size != 10 || res.Via != "local" || locks != 2 {
		t.Errorf("preview %d, asked %d, result %+v, locks %d", len(shown), asked, res, locks)
	}
	if res.Worker.Same != 1 || len(res.Worker.Displaced) != 1 || res.Worker.Displaced[0] != "scratch/old.txt" {
		t.Errorf("worker %+v", res.Worker)
	}
	if b, _ := os.ReadFile(filepath.Join(w.p.HostRoot(), "scratch", "old.txt")); string(b) != "remote" {
		t.Errorf("pulled file %q", b)
	}
	calls, dry := w.rsyncCalls()
	if len(calls) != 2 || !dry[0] || dry[1] {
		t.Fatalf("rsync calls %q", calls)
	}
	nas := filepath.Join(w.dir, "nas")
	for _, args := range calls {
		if args[len(args)-2] != nas+"/./scratch" || !strings.HasPrefix(args[len(args)-1], w.svc.Config.Paths.SyncStagingDir(w.p.Ref)+"/") {
			t.Errorf("operands %q", args)
		}
		for _, a := range args {
			if strings.HasPrefix(a, w.p.HostDir) {
				t.Errorf("rsync got a path in the project directory: %q", a)
			}
		}
	}
	if w.host.Called("ssh") {
		t.Error("ssh ran for a local remote")
	}
	// The staging directory is gone; the manifest has the pull.
	if ents, _ := os.ReadDir(w.svc.Config.Paths.SyncStagingDir(w.p.Ref)); len(ents) != 0 {
		t.Errorf("staging left: %v", ents)
	}
	recs, _ := ReadManifest(w.svc.Config.Paths, w.p.Ref)
	var paths []string
	for _, r := range recs {
		if r.Action != ActionPull || r.Remote != "nas" || r.Machine != "ws" {
			t.Errorf("record %+v", r)
		}
		paths = append(paths, r.Path)
	}
	if !slices.Equal(paths, []string{"scratch/old.txt", "scratch/same.txt"}) {
		t.Errorf("recorded %v", paths)
	}
	if len(res.Records) != 2 {
		t.Errorf("result records %+v", res.Records)
	}

	// Declined, too large, dry run: nothing changes, nothing is left.
	w.host.Reset()
	opts.Confirm = func(*SyncResult) (bool, error) { return false, nil }
	if res, err := w.svc.Pull(ctx, w.p, opts); !errors.Is(err, ErrDeclined) || !res.Declined {
		t.Errorf("declined: %v", err)
	}
	opts.Confirm = func(*SyncResult) (bool, error) { return true, nil }
	opts.MaxSize = 9
	if _, err := w.svc.Pull(ctx, w.p, opts); err == nil || !strings.Contains(err.Error(), "--max-size") {
		t.Errorf("max size: %v", err)
	}
	opts.MaxSize = 0
	opts.DryRun = true
	if res, err := w.svc.Pull(ctx, w.p, opts); err != nil || !res.DryRun {
		t.Errorf("dry run: %v", err)
	}
	opts.DryRun = false
	if calls, dry := w.rsyncCalls(); len(calls) != 3 || slices.Contains(dry, false) {
		t.Errorf("only dry runs expected: %v", dry)
	}
	if ents, _ := os.ReadDir(w.svc.Config.Paths.SyncStagingDir(w.p.Ref)); len(ents) != 0 {
		t.Errorf("staging left: %v", ents)
	}
	if n := len(mustRead(t, w)); n != 2 {
		t.Errorf("%d records after pulls that changed nothing", n)
	}

	// An interrupted transfer keeps the staging directory; the next pull
	// continues there.
	w.host.Reset()
	w.host.On("rsync").Stdout(">f+++++++++ 6 scratch/old.txt\n").Exit(23).Do(func(c execx.Cmd) {
		if !slices.Contains(c.Args, "--dry-run") {
			dst := strings.TrimSuffix(c.Args[len(c.Args)-1], "/")
			os.MkdirAll(filepath.Join(dst, "scratch"), 0o755)
			os.WriteFile(filepath.Join(dst, "scratch", "part.txt"), []byte("par"), 0o644)
		}
	})
	w.host.On("rsync", fixedArgs...).Stdout(">f+++++++++ 6 scratch/old.txt\n").Times(1)
	opts.Yes = true
	_, err = w.svc.Pull(ctx, w.p, opts)
	if err == nil || !strings.Contains(err.Error(), "continues") {
		t.Fatalf("interrupted pull: %v", err)
	}
	ents, _ := os.ReadDir(w.svc.Config.Paths.SyncStagingDir(w.p.Ref))
	if len(ents) != 1 || !strings.HasSuffix(ents[0].Name(), "-nas") {
		t.Fatalf("staging after the failure: %v", ents)
	}
	kept := filepath.Join(w.svc.Config.Paths.SyncStagingDir(w.p.Ref), ents[0].Name())
	w.host.Reset()
	w.host.On("rsync").Stdout("")
	var out bytes.Buffer
	w.svc.Out = &out
	if _, err := w.svc.Pull(ctx, w.p, opts); err != nil {
		t.Fatal(err)
	}
	w.svc.Out = nil
	calls, _ = w.rsyncCalls()
	if len(calls) != 2 || calls[1][len(calls[1])-1] != kept+"/" || !strings.Contains(out.String(), "continuing") {
		t.Errorf("continued pull: %q %s", calls, out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(w.p.HostRoot(), "scratch", "part.txt")); string(b) != "par" {
		t.Errorf("continued file %q", b)
	}

	// A target inside a sealed entry is refused before any transfer.
	w.put("results/run1/a", "a")
	if _, err := w.svc.Seal(ctx, w.p, "results/run1", false); err != nil {
		t.Fatal(err)
	}
	w.host.Reset()
	opts.Paths = []string{"results/run1/a"}
	if _, err := w.svc.Pull(ctx, w.p, opts); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Errorf("sealed target: %v", err)
	}
	if len(w.host.Calls()) != 0 {
		t.Errorf("commands ran for a refused pull: %v", w.host.Calls())
	}
	// Unknown remotes and paths outside zones.
	for _, o := range []SyncOptions{{Remote: "none"}, {Remote: "-x"}, {Remote: "nas", Paths: []string{"elsewhere"}}, {Remote: "c1"}} {
		if _, err := w.svc.Pull(ctx, w.p, o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func mustRead(t *testing.T, w *world) []Record {
	t.Helper()
	recs, err := ReadManifest(w.svc.Config.Paths, w.p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestServiceSSHRemote(t *testing.T) {
	w := syncWorld(t)
	ctx := context.Background()
	w.put("scratch/a", "a")
	w.host.On("rsync").Stdout("")
	opts := SyncOptions{Remote: "c1", Paths: []string{"scratch"}, DryRun: true}
	// A master answers: rsync goes through it.
	w.host.On("ssh").Exit(0)
	res, err := w.svc.Pull(ctx, w.p, opts)
	if err != nil {
		t.Fatal(err)
	}
	gateDir, _ := w.svc.Config.Paths.GateRuntimeDir()
	calls, _ := w.rsyncCalls()
	args := calls[0]
	sock := filepath.Join(gateDir, "m-")
	if res.Via != "ssh-master" || !slices.ContainsFunc(args, func(a string) bool {
		return strings.HasPrefix(a, "--rsh=ssh -S "+sock) && strings.HasSuffix(a, ".sock -o ControlMaster=no")
	}) || args[len(args)-2] != "c1:/scratch/p/./scratch" {
		t.Errorf("over the master: %q", args)
	}
	check := w.host.Calls()[0]
	if check.Name != "ssh" || !slices.Contains(check.Args, "-O") || check.Args[len(check.Args)-1] != "c1" {
		t.Errorf("master check %v", check)
	}
	// No master: the user's ordinary SSH, with DEVC_SSH_CONFIG when set.
	w.host.Reset()
	w.host.On("ssh").Exit(255)
	if res, err := w.svc.Pull(ctx, w.p, opts); err != nil || res.Via != "ssh" {
		t.Fatalf("without a master: %+v %v", res, err)
	}
	calls, _ = w.rsyncCalls()
	if slices.ContainsFunc(calls[0], func(a string) bool { return strings.HasPrefix(a, "--rsh") }) {
		t.Errorf("a shell was set: %q", calls[0])
	}
	w.host.Reset()
	w.svc.SSHConfig = "/t/ssh_config"
	if _, err := w.svc.Push(ctx, w.p, opts); err != nil {
		t.Fatal(err)
	}
	calls, _ = w.rsyncCalls()
	if !slices.Contains(calls[0], "--rsh=ssh -F /t/ssh_config") || calls[0][len(calls[0])-1] != "c1:/scratch/p/" {
		t.Errorf("with a configuration: %q", calls[0])
	}
}

func TestServicePush(t *testing.T) {
	w := syncWorld(t)
	ctx := context.Background()
	w.put("scratch/w/a.txt", "aa")
	w.put("scratch/b.txt", "b")
	w.svc.Now = func() time.Time { return time.Date(2026, 11, 15, 4, 5, 0, 0, time.UTC) }
	stage := filepath.Join(w.p.HostDir, boxside.PushStagingDir, "push-20261115T040500Z")
	// The fake rsync checks, while the push runs, that its source is the
	// staging directory and holds hard links of the data files.
	var checked int
	w.host.On("rsync").Stdout(">f+++++++++ 2 scratch/w/a.txt\n>f.st...... 1 scratch/b.txt\n").Do(func(c execx.Cmd) {
		if c.Args[len(c.Args)-2] != stage+"/./scratch" || c.Args[len(c.Args)-1] != filepath.Join(w.dir, "nas")+"/" {
			t.Errorf("operands %q", c.Args)
		}
		if !slices.Contains(c.Args, "--backup-dir=.devc-backup/20261115T040500Z") {
			t.Errorf("no backup directory: %q", c.Args)
		}
		var a, b syscall.Stat_t
		syscall.Lstat(filepath.Join(stage, "scratch", "w", "a.txt"), &a)
		syscall.Lstat(filepath.Join(w.p.HostRoot(), "scratch", "w", "a.txt"), &b)
		if a.Ino == 0 || a.Ino != b.Ino {
			t.Error("the staged file is not a hard link of the data file")
		}
		checked++
	})
	locks := 0
	res, err := w.svc.Push(ctx, w.p, SyncOptions{Remote: "nas", Yes: true, Content: true, Lock: countLocks(&locks)})
	if err != nil {
		t.Fatal(err)
	}
	if checked != 2 || res.Files != 2 || res.Updated != 1 || res.BackupDir != ".devc-backup/20261115T040500Z" || locks != 2 {
		t.Errorf("checked %d, result %+v, locks %d", checked, res, locks)
	}
	if _, err := os.Lstat(filepath.Join(w.p.HostDir, boxside.PushStagingDir)); err == nil {
		t.Error("the staging directory is still there")
	}
	recs := mustRead(t, w)
	if len(recs) != 2 || recs[0].Action != ActionPush || recs[0].Remote != "nas" || recs[0].Path != "scratch/b.txt" || recs[0].Content == "" || recs[1].Path != "scratch/w" {
		t.Errorf("records %+v", recs)
	}
	// Engine runs: stage (capabilities to link and own), unstage.
	var ops []string
	for _, o := range w.runs {
		if slices.Equal(o.Cmd, []string{"/bin/devc", "box", "data-worker"}) {
			ops = append(ops, strings.Join(o.CapAdd, ","))
		}
	}
	if len(ops) < 2 || ops[len(ops)-2] != "CHOWN,FOWNER,DAC_OVERRIDE" || ops[len(ops)-1] != "DAC_OVERRIDE,FOWNER" {
		t.Errorf("worker capabilities %v", ops)
	}
	// A dry run sends nothing and leaves no staging directory.
	w.host.Reset()
	checked = 0
	if res, err := w.svc.Push(ctx, w.p, SyncOptions{Remote: "nas", DryRun: true}); err != nil || !res.DryRun {
		t.Fatal(err)
	}
	if _, dry := w.rsyncCalls(); !slices.Equal(dry, []bool{true}) {
		t.Errorf("dry push ran %v", dry)
	}
	if _, err := os.Lstat(filepath.Join(w.p.HostDir, boxside.PushStagingDir)); err == nil {
		t.Error("the staging directory is still there after a dry run")
	}
	if len(mustRead(t, w)) != 2 {
		t.Error("a dry run was recorded")
	}
	// A failed transfer is an error, records nothing and still unstages.
	w.host.Reset()
	w.host.On("rsync").Exit(255)
	if _, err := w.svc.Push(ctx, w.p, SyncOptions{Remote: "nas", Yes: true}); err == nil || !strings.Contains(err.Error(), "exit status 255") {
		t.Errorf("failed push: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(w.p.HostDir, boxside.PushStagingDir)); err == nil {
		t.Error("the staging directory is still there after a failure")
	}
}

func TestStatusAndVerify(t *testing.T) {
	w := syncWorld(t)
	ctx := context.Background()
	w.put("raw/in.csv", "1,2")
	w.put("scratch/w/a", "aa")
	w.put("results/run1/r", "rrr")
	if _, err := w.svc.Seal(ctx, w.p, "results/run1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Seal(ctx, w.p, "results/run1", false); err != nil {
		t.Fatal(err)
	}
	if n := len(mustRead(t, w)); n != 1 {
		t.Fatalf("sealing again was recorded: %d records", n)
	}
	// scratch/w was pushed by another machine and changed since; an entry
	// that is gone was pulled on this one.
	cur, err := w.svc.work(ctx, w.p, boxside.DataRequest{Op: boxside.OpDigest, Paths: []string{"scratch/w"}}, capsRead, "")
	if err != nil {
		t.Fatal(err)
	}
	d := cur.Digests[0]
	err = AppendManifest(ctx, w.svc.Config.Paths, w.p.Ref, []Record{
		{V: 1, Time: "2026-11-15T01:00:00Z", Machine: "laptop", Action: ActionPush, Path: "scratch/w", Files: d.Files, Size: d.Size, Digest: d.Digest, Remote: "nas"},
		{V: 1, Time: "2026-11-15T02:00:00Z", Machine: "ws", Action: ActionPull, Path: "results/gone", Digest: testDigest(5), Remote: "c1"},
		{V: 1, Time: "2026-11-15T02:30:00Z", Machine: "ws", Action: ActionPull, Path: "scratch/w/deep", Digest: testDigest(6), Remote: "c1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := w.svc.Verify(ctx, w.p, "scratch/w", false); err != nil || !v.Matches || v.Failed() {
		t.Errorf("verify before the change: %+v %v", v, err)
	}
	w.put("scratch/w/b", "new")
	st, err := w.svc.Status(ctx, w.p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Machine != "ws" || st.LastSave != nil {
		t.Errorf("status %+v", st)
	}
	got := map[string]StatusEntry{}
	var order []string
	for _, e := range st.Entries {
		got[e.Path] = e
		order = append(order, e.Path)
	}
	if !slices.Equal(order, []string{"raw/in.csv", "scratch/w", "results/gone", "results/run1"}) {
		t.Errorf("entries %v", order)
	}
	if e := got["raw/in.csv"]; e.State != StateNew || !e.Present || e.Files != 1 || e.Kind != boxside.ZoneInputs {
		t.Errorf("raw/in.csv %+v", e)
	}
	if e := got["scratch/w"]; e.State != StateChanged || e.Files != 2 || len(e.Machines) != 1 || e.Machines[0].Name != "laptop" || e.Machines[0].Same ||
		len(e.Remotes) != 1 || e.Remotes[0].Name != "nas" || e.Last.Action != ActionPush {
		t.Errorf("scratch/w %+v", e)
	}
	if e := got["results/run1"]; e.State != StateRecorded || len(e.Machines) != 1 || e.Machines[0].Name != "ws" || !e.Machines[0].Same || len(e.Remotes) != 0 {
		t.Errorf("results/run1 %+v", e)
	}
	if e := got["results/gone"]; e.State != StateAbsent || e.Present || e.Remotes[0].Name != "c1" {
		t.Errorf("results/gone %+v", e)
	}
	v, err := w.svc.Verify(ctx, w.p, "scratch/w", false)
	if err != nil || v.Matches || !v.Failed() || v.Last.Machine != "laptop" {
		t.Errorf("verify after the change: %+v %v", v, err)
	}
	v, err = w.svc.Verify(ctx, w.p, "results/run1", true)
	if err != nil || !v.Matches || v.ContentRecord == nil || !v.ContentMatches || v.Failed() || v.Current.Content == "" {
		t.Errorf("verify with content: %+v %v", v, err)
	}
	if v, err := w.svc.Verify(ctx, w.p, "raw/in.csv", true); err != nil || v.Last != nil || v.ContentRecord != nil || v.Failed() {
		t.Errorf("verify without records: %+v %v", v, err)
	}
	if _, err := w.svc.Verify(ctx, w.p, "scratch/none", false); err == nil {
		t.Error("verify of a missing path worked")
	}
	// A savepoint is recorded with its digest.
	w.installRestic()
	sp, err := w.svc.Save(ctx, w.p, "")
	if err != nil {
		t.Fatal(err)
	}
	st, _ = w.svc.Status(ctx, w.p)
	if st.LastSave == nil || st.LastSave.Savepoint != sp.ID || st.LastSave.Path != "." || st.LastSave.Files != 2 || st.LastSave.Size != 5 {
		t.Errorf("last save %+v", st.LastSave)
	}
}

func TestDecodeDigests(t *testing.T) {
	ok := `{"v":1,"op":"digest","files":0,"size":0,"digests":[{"path":"scratch/a","type":"dir","files":1,"size":2,"digest":"` + testDigest(1) + `"}]}`
	if _, err := DecodeResult([]byte(ok), boxside.OpDigest); err != nil {
		t.Errorf("valid digests: %v", err)
	}
	for _, bad := range []string{
		`{"v":1,"op":"digest","files":0,"size":0,"digests":[{"path":"../a","type":"dir","files":1,"size":2,"digest":"` + testDigest(1) + `"}]}`,
		`{"v":1,"op":"digest","files":0,"size":0,"digests":[{"path":"a","type":"fifo","files":1,"size":2,"digest":"` + testDigest(1) + `"}]}`,
		`{"v":1,"op":"digest","files":0,"size":0,"digests":[{"path":"a","type":"dir","files":-1,"size":2,"digest":"` + testDigest(1) + `"}]}`,
		`{"v":1,"op":"digest","files":0,"size":0,"digests":[{"path":"a","type":"dir","files":1,"size":2,"digest":"x"}]}`,
		`{"v":1,"op":"digest","files":0,"size":0,"digests":[{"path":"a","type":"dir","files":1,"size":2,"digest":"` + testDigest(1) + `","content":"y"}]}`,
		`{"v":1,"op":"push-stage","files":0,"size":0,"stage":"../x"}`,
		`{"v":1,"op":"pull-in","files":0,"size":0,"same":-1}`,
	} {
		op := boxside.OpDigest
		if strings.Contains(bad, "push-stage") {
			op = boxside.OpPushStage
		} else if strings.Contains(bad, "pull-in") {
			op = boxside.OpPullIn
		}
		if _, err := DecodeResult([]byte(bad), op); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
