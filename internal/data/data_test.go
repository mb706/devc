package data

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/toolbox"
)

func TestNormalize(t *testing.T) {
	root := t.TempDir()
	p := &Project{Ref: names.ProjectRef{Realm: "r", Project: "p"}, HostDir: filepath.Join(root, "r", "p"),
		Layout: boxside.DataLayout{Root: "data", Work: []string{"scratch"}, Sealed: []string{"results"}}}
	for arg, want := range map[string]string{
		"results/run1":                 "results/run1",
		"./results/run1/":              "results/run1",
		"/w/r/p/data/results/run1":     "results/run1",
		root + "/r/p/data/scratch/a/b": "scratch/a/b",
		"results//run1":                "results/run1",
	} {
		if got, err := Normalize(p, root, arg); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", arg, got, err, want)
		}
	}
	for arg, want := range map[string]string{
		"":                       "empty",
		"../x":                   "..",
		"results/../scratch/x":   "..",
		"/w/r/p/main/x":          "not inside the data root",
		"/w/r/p/data":            "data root itself",
		"/etc/passwd":            "not inside",
		"/w/r/p/data/a/../../..": "..",
		".":                      "empty",
		"a\x00b":                 "NUL",
	} {
		if _, err := Normalize(p, root, arg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Normalize(%q) = %v, want %q", arg, err, want)
		}
	}
}

func TestIndexAndRetention(t *testing.T) {
	file := filepath.Join(t.TempDir(), "idx.jsonl")
	base := time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC)
	used := map[string]bool{}
	var recs []Savepoint
	for i, name := range []string{"", "keep", "", "", ""} {
		id := NewID(base.Add(time.Duration(i)*time.Minute), name, used)
		used[id] = true
		recs = append(recs, Savepoint{V: 1, Op: opSave, ID: id, Name: name, Time: base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano), Driver: DriverRestic})
	}
	if err := appendIndex(file, recs...); err != nil {
		t.Fatal(err)
	}
	if err := appendIndex(file, Savepoint{V: 1, Op: opDrop, ID: recs[2].ID}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file+".x", nil, 0o600)
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n{\"v\":1,\"op\":\"save\",\"id\":\"../evil\"}\n")
	f.Close()
	sps, all, err := ReadIndex(file)
	if err != nil || len(sps) != 4 || len(all) != 5 {
		t.Fatalf("index: %d live, %d used, %v", len(sps), len(all), err)
	}
	drop := Retention(sps, 2)
	if len(drop) != 1 || drop[0].ID != recs[0].ID {
		t.Errorf("retention dropped %+v", drop)
	}
	if Retention(sps, 0) != nil || Retention(sps, 3) != nil {
		t.Error("retention with keep 0 or enough room dropped something")
	}
	// IDs: a taken one moves on by a second; names are appended.
	id := NewID(base, "", map[string]bool{"20261115T040000Z": true})
	if id != "20261115T040001Z" {
		t.Errorf("NewID = %q", id)
	}
	if got := NewID(base, "x", nil); got != "20261115T040000Z-x" {
		t.Errorf("named ID %q", got)
	}
	if sps, _, err := ReadIndex(filepath.Join(t.TempDir(), "none")); err != nil || len(sps) != 0 {
		t.Errorf("missing index: %v %v", sps, err)
	}
}

func TestTrashAgeAndPatterns(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	items := []boxside.TrashInfo{
		{ID: "20261001T000000Z-a", Time: "2026-10-01T00:00:00Z"},
		{ID: "20261114T000000Z-b"},
		{ID: "20261113T000000Z-c", Time: "garbage"},
	}
	got := TrashOlderThan(items, 30*24*time.Hour, now)
	if len(got) != 1 || got[0].ID != "20261001T000000Z-a" {
		t.Errorf("older than 30d: %+v", got)
	}
	if got := TrashOlderThan(items, 36*time.Hour, now); len(got) != 2 {
		t.Errorf("older than 36h: %+v", got)
	}
	if got := TrashOlderThan(items, 0, now); len(got) != 3 {
		t.Errorf("all: %+v", got)
	}
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "12h": 12 * time.Hour, "0d": 0} {
		if d, err := ParseAge(in); err != nil || d != want {
			t.Errorf("ParseAge(%q) = %v, %v", in, d, err)
		}
	}
	for _, bad := range []string{"x", "-1d", "1.5d", "d", "-3h"} {
		if _, err := ParseAge(bad); err == nil {
			t.Errorf("ParseAge(%q) accepted", bad)
		}
	}
	if got := resticPattern(`/project/data/results/*[x]?\`); got != `/project/data/results/\*\[x\]\?\\` {
		t.Errorf("pattern %q", got)
	}
}

func TestDecodeResult(t *testing.T) {
	ok := `{"v":1,"op":"seal","files":2,"size":3}`
	if r, err := DecodeResult([]byte(ok), boxside.OpSeal); err != nil || r.Files != 2 {
		t.Errorf("valid result: %+v %v", r, err)
	}
	for _, bad := range []string{
		`{"v":1,"op":"seal","files":2,"size":3,"extra":true}`,
		`{"v":1,"op":"rm","files":0,"size":0}`,
		`{"v":2,"op":"seal","files":0,"size":0}`,
		`{"v":1,"op":"seal","files":-1,"size":0}`,
		`{"v":1,"op":"seal","files":0,"size":0}{}`,
		`{"v":1,"op":"seal","files":0,"size":0,"trash_id":"../x"}`,
		`{"v":1,"op":"seal","files":0,"size":0,"paths":["/etc"]}`,
		`{"v":1,"op":"seal","files":0,"size":0,"displaced":["a/../b"]}`,
		`{"v":1,"op":"seal","files":0,"size":0,"trash":[{"id":"x","time":"","reason":"","paths":[],"files":0,"size":0}]}`,
	} {
		if _, err := DecodeResult([]byte(bad), boxside.OpSeal); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	r, err := DecodeResult([]byte(`{"v":1,"op":"seal","files":0,"size":0,"skipped":["a\u001b[31m (fifo)"]}`), boxside.OpSeal)
	if err != nil || strings.ContainsRune(r.Skipped[0], 0x1b) {
		t.Errorf("skipped text not made printable: %q %v", r.Skipped, err)
	}
}

// world is a configuration with one data project, a fake engine whose
// helper containers run the real data worker in this process, and a fake
// restic.
type world struct {
	t       *testing.T
	svc     *Service
	eng     *engine.Fake
	p       *Project
	restics [][]string
	runs    []engine.RunOptions
	// host runs rsync and ssh (data pull and push).
	host *execx.Fake
	dir  string
}

func newWorld(t *testing.T, driver string) *world {
	t.Helper()
	return newWorldExtra(t, driver, "")
}

// newWorldExtra is newWorld with more project.toml (data remotes).
func newWorldExtra(t *testing.T, driver, extra string) *world {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to change owners")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	for file, content := range map[string]string{
		"config.toml":                      "projects_root = \"" + filepath.Join(dir, "projects") + "\"\nmachine_name = \"ws\"\n",
		"realms/r/realm.toml":              "[network]\ndefault = \"open\"\n",
		"realms/r/projects/p/project.toml": "image = \"docker.io/library/debian:bookworm-slim\"\nharnesses = []\n[data]\ninputs = [\"raw\"]\nwork = [\"scratch\"]\nsealed = [\"results\"]\n[data.savepoints]\ndriver = \"" + driver + "\"\nkeep = 2\n" + strings.ReplaceAll(extra, "@DIR@", dir),
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(cfg, file)), 0o755)
		os.WriteFile(filepath.Join(cfg, file), []byte(content), 0o644)
	}
	p, err := paths.Resolve(paths.Options{ConfigDir: cfg, StateDir: filepath.Join(dir, "state"), RuntimeDir: filepath.Join(dir, "run"), Home: dir})
	if err != nil {
		t.Fatal(err)
	}
	c, probs := config.Load(p)
	if len(probs) > 0 {
		t.Fatalf("config problems: %v", probs)
	}
	exe := filepath.Join(dir, "devc-exe")
	os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	w := &world{t: t, eng: engine.NewFake(), host: execx.NewFake(), dir: dir}
	w.svc = &Service{Config: c, Engine: w.eng, Toolbox: toolbox.New(c.Paths.ToolboxDir()), Executable: exe, Version: "test",
		Out: io.Discard, HostRoot: dir, Commit: func(context.Context, names.ProjectRef) string { return "c0ffee" }, Runner: w.host}
	w.eng.RunFunc = w.run
	if w.p, err = w.svc.Project(names.ProjectRef{Realm: "r", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	return w
}

func mountOf(o engine.RunOptions, target string) string {
	for _, m := range o.Mounts {
		if m.Target == target {
			return m.Source
		}
	}
	return ""
}

func (w *world) run(o engine.RunOptions) (execx.Result, error) {
	w.runs = append(w.runs, o)
	switch {
	case slices.Equal(o.Cmd, []string{"/bin/devc", "box", "data-worker"}):
		req, err := boxside.ReadDataRequest(o.Stdin)
		if err != nil {
			return execx.Result{}, err
		}
		dw := boxside.NewDataWorker()
		dw.Target, dw.Staging = mountOf(o, boxside.HelperTarget), mountOf(o, boxside.HelperStaging)
		res, err := dw.Run(req)
		if err != nil {
			return execx.Result{ExitCode: 1, Stderr: []byte("devc: " + err.Error() + "\n")}, &execx.ExitError{Result: execx.Result{ExitCode: 1}}
		}
		// As the worker prints it; the capture limit applies as in the
		// real engine.
		b, _ := boxside.MarshalReport(res)
		if o.MaxOutput > 0 && len(b) > o.MaxOutput {
			return execx.Result{Stdout: b[:o.MaxOutput], Truncated: true}, nil
		}
		return execx.Result{Stdout: b}, nil
	case slices.Equal(o.Cmd, []string{"/bin/devc", "box", "rm-worker"}):
		return execx.Result{}, boxside.ClearDir(mountOf(o, boxside.HelperTarget))
	case strings.HasSuffix(o.Cmd[0], "/restic"):
		args := o.Cmd[1:]
		if len(args) > 6 && args[0] == "--repo" {
			args = args[6:]
		}
		w.restics = append(w.restics, args)
		switch args[0] {
		case "init":
			os.WriteFile(filepath.Join(mountOf(o, "/repo"), "config"), nil, 0o600)
		case "backup":
			return execx.Result{Stdout: []byte(`{"message_type":"status"}` + "\n" + `{"message_type":"summary","snapshot_id":"0123456789abcdef","total_files_processed":3,"total_bytes_processed":7}` + "\n")}, nil
		case "restore":
			dst := filepath.Join(mountOf(o, "/restore"), "project", "data", "scratch")
			os.MkdirAll(dst, 0o755)
			os.WriteFile(filepath.Join(dst, "f"), []byte("old"), 0o644)
		case "version":
			return execx.Result{Stdout: []byte("restic 0.19.1 compiled with go\n")}, nil
		}
		return execx.Result{}, nil
	}
	w.t.Fatalf("unexpected container %v", o.Cmd)
	return execx.Result{}, nil
}

func (w *world) installRestic() {
	dir := filepath.Join(w.svc.Toolbox.ResticDir(), toolbox.ResticVersion)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "restic"), []byte("#!/bin/sh\n"), 0o755)
	os.Symlink(toolbox.ResticVersion, filepath.Join(w.svc.Toolbox.ResticDir(), "current"))
}

func TestServiceWorkers(t *testing.T) {
	w := newWorld(t, "restic")
	ctx := context.Background()
	if _, err := w.svc.Setup(ctx, w.p); err != nil {
		t.Fatal(err)
	}
	o := w.runs[0]
	if o.Rootfs != w.svc.Toolbox.HelperRootfs() || o.Network != "none" || o.UserNS != "keep-id:uid=1000,gid=1000" || o.User != "0:0" ||
		!o.CapDropAll || !slices.Equal(o.CapAdd, []string{"CHOWN", "FOWNER", "DAC_OVERRIDE"}) || len(o.Mounts) != 1 ||
		o.Mounts[0].Source != w.p.HostDir || !slices.Contains(o.SecurityOpts, "no-new-privileges") || o.Labels["devc.role"] != "helper" || !o.Interactive {
		t.Errorf("helper options %+v", o)
	}
	// The realm directory is private.
	if fi, err := os.Stat(filepath.Dir(w.p.HostDir)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("realm directory: %v %v", fi.Mode(), err)
	}
	os.MkdirAll(filepath.Join(w.p.HostRoot(), "results", "run1"), 0o755)
	os.WriteFile(filepath.Join(w.p.HostRoot(), "results", "run1", "f"), []byte("x"), 0o644)
	res, err := w.svc.Seal(ctx, w.p, "results/run1", false)
	if err != nil || res.Files != 1 {
		t.Fatalf("seal: %+v %v", res, err)
	}
	if _, err := w.svc.Seal(ctx, w.p, "scratch/x", false); err == nil {
		t.Error("sealing outside a sealed zone was sent to a worker")
	}
	if _, err := w.svc.Seal(ctx, w.p, "results/none", false); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the worker's message did not come through: %v", err)
	}
	l, err := w.svc.Ls(ctx, w.p)
	if err != nil || len(l.Sealed) != 1 || l.Sealed[0].Commit != "c0ffee" || l.Sealed[0].Files != 1 || len(l.Zones) != 3 {
		t.Errorf("ls: %+v %v", l, err)
	}
	if last := w.runs[len(w.runs)-1]; !slices.Equal(last.CapAdd, []string{"DAC_READ_SEARCH"}) {
		t.Errorf("ls capabilities %v", last.CapAdd)
	}
	rm, err := w.svc.Rm(ctx, w.p, "results/run1")
	if err != nil || !boxside.ValidTrashID(rm.TrashID) {
		t.Fatalf("rm: %+v %v", rm, err)
	}
	if _, err := w.svc.TrashRestore(ctx, w.p, rm.TrashID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Rm(ctx, w.p, "scratch"); err == nil {
		t.Error("removing a zone directory was accepted")
	}
}

func TestServiceRestic(t *testing.T) {
	w := newWorld(t, "restic")
	ctx := context.Background()
	if _, err := w.svc.Save(ctx, w.p, ""); err == nil || !strings.Contains(err.Error(), "toolbox install restic") {
		t.Fatalf("save without restic: %v", err)
	}
	w.installRestic()
	if v, err := w.svc.CheckRestic(ctx); err != nil || !strings.HasPrefix(v, "restic 0.19.1") {
		t.Errorf("check: %q %v", v, err)
	}
	os.MkdirAll(filepath.Join(w.p.HostRoot(), "results", "*odd"), 0o755)
	if _, err := w.svc.Setup(ctx, w.p); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Seal(ctx, w.p, "results/*odd", false); err != nil {
		t.Fatal(err)
	}
	w.restics = nil
	sp, err := w.svc.Save(ctx, w.p, "first")
	if err != nil || sp.Driver != DriverRestic || sp.Snapshot != "0123456789abcdef" || sp.Files != 3 || sp.Commit != "c0ffee" || !strings.HasSuffix(sp.ID, "-first") {
		t.Fatalf("save: %+v %v", sp, err)
	}
	if len(w.restics) != 2 || w.restics[0][0] != "init" {
		t.Fatalf("restic calls %v", w.restics)
	}
	backup := strings.Join(w.restics[1], " ")
	for _, want := range []string{"backup --json --quiet --host devc --tag " + sp.ID, `--exclude=/project/data/raw`, `--exclude=/project/data/results/\*odd`, " /project/data"} {
		if !strings.Contains(backup, want) {
			t.Errorf("backup %q lacks %q", backup, want)
		}
	}
	var ro engine.RunOptions
	for _, o := range w.runs {
		if strings.HasSuffix(o.Cmd[0], "/restic") {
			ro = o
		}
	}
	if ro.User != "1000:1000" || len(ro.CapAdd) != 0 || !ro.CapDropAll || ro.Network != "none" || ro.UserNS != boxUserNS {
		t.Errorf("restic container %+v", ro)
	}
	for _, m := range ro.Mounts {
		if m.Target == "/project" && (!m.ReadOnly || m.Source != w.p.HostDir) || m.Target == "/opt/devc" && !m.ReadOnly {
			t.Errorf("mount %+v", m)
		}
	}
	// Retention with keep = 2: the named savepoint stays.
	for i := 0; i < 3; i++ {
		w.svc.Now = func() time.Time { return time.Now().Add(time.Duration(i+1) * time.Hour) }
		if _, err := w.svc.Save(ctx, w.p, ""); err != nil {
			t.Fatal(err)
		}
	}
	sps, _ := w.svc.Savepoints(w.p)
	if len(sps) != 3 || sps[0].Name != "first" {
		t.Errorf("after retention: %+v", sps)
	}
	forgets := 0
	for _, a := range w.restics {
		if a[0] == "forget" {
			forgets++
		}
	}
	if forgets != 1 {
		t.Errorf("%d forget calls", forgets)
	}

	// A restic restore goes through a staging directory, which is removed.
	os.WriteFile(filepath.Join(w.p.HostRoot(), "scratch", "f"), []byte("new"), 0o644)
	res, err := w.svc.Restore(ctx, w.p, sp, []string{"scratch/f"}, true)
	if err != nil || !slices.Equal(res.Displaced, []string{"scratch/f"}) {
		t.Fatalf("restore: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(w.p.HostRoot(), "scratch", "f")); string(b) != "old" {
		t.Errorf("restored %q", b)
	}
	if ents, _ := os.ReadDir(w.svc.Config.Paths.TmpDir()); len(ents) != 0 {
		t.Errorf("staging left behind: %v", ents)
	}
	if _, err := w.svc.Restore(ctx, w.p, sp, []string{"results/*odd/x"}, false); err == nil || !strings.Contains(err.Error(), "sealed when the savepoint") {
		t.Errorf("restoring an excluded path: %v", err)
	}
	if _, err := w.svc.Restore(ctx, w.p, sp, []string{"raw/x"}, false); err == nil {
		t.Error("restoring an input path was accepted")
	}
	// gc drops the named one on request and prunes.
	w.restics = nil
	ids, err := w.svc.GC(ctx, w.p, []string{"first"})
	if err != nil || len(ids) != 1 || ids[0] != sp.ID {
		t.Errorf("gc: %v %v", ids, err)
	}
	if got := w.restics[len(w.restics)-1]; got[0] != "prune" {
		t.Errorf("gc did not prune: %v", w.restics)
	}
}

func TestServiceAutoAndBeforeSession(t *testing.T) {
	w := newWorld(t, "auto")
	ctx := context.Background()
	// The worker's FICLONE fails on the test filesystem (or the fake engine
	// makes it fail): auto falls back to restic, which is not installed.
	_, err := w.svc.Save(ctx, w.p, "")
	if err == nil {
		sps, _ := w.svc.Savepoints(w.p)
		if len(sps) != 1 || sps[0].Driver != DriverReflink {
			t.Fatalf("save: %+v", sps)
		}
		t.Skip("this filesystem can clone files; the fallback is not exercised")
	}
	if !strings.Contains(err.Error(), "no savepoint driver is available") {
		t.Fatalf("auto without reflink or restic: %v", err)
	}
	w.installRestic()
	w.p.Resolved.Data.Savepoints.BeforeSession = true
	sp, err := w.svc.BeforeSession(ctx, w.p)
	if err != nil || sp == nil || sp.Driver != DriverRestic {
		t.Fatalf("before session: %+v %v", sp, err)
	}
	if sp2, err := w.svc.BeforeSession(ctx, w.p); err != nil || sp2 != nil {
		t.Errorf("a second savepoint within 30 minutes: %+v %v", sp2, err)
	}
	w.p.Resolved.Data.Savepoints.Driver = DriverBorg
	if _, err := w.svc.Save(ctx, w.p, ""); err == nil || !strings.Contains(err.Error(), "borg") {
		t.Errorf("borg: %v", err)
	}
	w.p.Resolved.Data.Savepoints.Driver = DriverNone
	if _, err := w.svc.Save(ctx, w.p, ""); err == nil {
		t.Error("driver none saved")
	}
}

func TestBoxHooksContribute(t *testing.T) {
	w := newWorld(t, "restic")
	// A second project mounting p read-write gets p's input zone read-only.
	q := filepath.Join(w.svc.Config.Paths.ProjectDefsDir("r"), "q")
	os.MkdirAll(q, 0o755)
	os.WriteFile(filepath.Join(q, "project.toml"), []byte("image = \"docker.io/library/debian:bookworm-slim\"\nharnesses = []\n[[mounts]]\nproject = \"p\"\nmode = \"rw\"\n"), 0o644)
	c, probs := config.Load(w.svc.Config.Paths)
	if len(probs) > 0 {
		t.Fatal(probs)
	}
	w.svc.Config = c
	h := &BoxHooks{Service: w.svc}
	for ref, want := range map[string]int{"p": 1, "q": 1} {
		r, err := c.Resolved(names.ProjectRef{Realm: "r", Project: ref})
		if err != nil {
			t.Fatal(err)
		}
		s := &box.Spec{}
		if err := h.Contribute(&box.Inputs{Resolved: r}, s); err != nil {
			t.Fatal(err)
		}
		if len(s.Mounts) != want || s.Mounts[0].Target != "/w/r/p/data/raw" || !s.Mounts[0].ReadOnly || s.Mounts[0].Source != filepath.Join(w.p.HostDir, "data", "raw") {
			t.Errorf("%s: mounts %+v", ref, s.Mounts)
		}
		// Prepare sets up p's layout for both.
		if err := h.Prepare(context.Background(), &box.Inputs{Resolved: r}, s); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(filepath.Join(w.p.HostDir, ".devc")); err != nil || !fi.IsDir() {
			t.Errorf("%s: no .devc after Prepare: %v", ref, err)
		}
	}
}
