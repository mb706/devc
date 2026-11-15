package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCheckUpdate(t *testing.T) {
	const z, a, b = "0000000000000000000000000000000000000000", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	prot := []string{"main", "release/*"}
	for _, c := range []struct {
		ref, old, new string
		ok            bool
		msg           string
	}{
		{"refs/heads/feature", z, a, true, ""},                    // create
		{"refs/heads/feature", a, b, true, ""},                    // update, fast-forward or not
		{"refs/heads/deep/name", z, a, true, ""},                  // slashes
		{"refs/heads/feature", a, z, false, "deleting branches"},  // delete
		{"refs/heads/main", a, b, false, "protected"},             // protected
		{"refs/heads/release/1", z, a, false, "protected"},        // protected by a glob
		{"refs/heads/-evil", z, a, false, "invalid branch name"},  // option-like
		{"refs/heads/a..b", z, a, false, "invalid branch name"},   // git's rules
		{"refs/heads/x.lock", z, a, false, "invalid branch name"}, //
		{"refs/heads/HEAD", z, a, false, "invalid branch name"},   // devc's limits
		{"refs/heads/ünï", z, a, false, "invalid branch name"},    //
		{"refs/tags/v1", z, a, true, ""},                          // create a tag
		{"refs/tags/v1", a, b, false, "not moved"},                // move a tag
		{"refs/tags/v1", a, z, false, "deleting tags"},            // delete a tag
		{"refs/tags/-x", z, a, false, "invalid tag name"},         //
		{"refs/devc/x", z, a, false, "only branches"},             // devc's namespace
		{"refs/devc/bank/main/1", z, a, false, "only branches"},   //
		{"refs/remotes/github/main", z, a, false, "only branches"},
		{"refs/notes/commits", z, a, false, "only branches"},
		{"HEAD", z, a, false, "malformed"},
		{"refs/heads/x y", z, a, false, "malformed"},
		{"refs/heads/x", "nothex", a, false, "malformed"},
		{"refs/heads/x", z, a[:39], false, "malformed"},
		{"refs/heads/x", z, strings.Repeat("a", 64), false, "malformed"}, // lengths differ
	} {
		err := CheckUpdate(c.ref, c.old, c.new, prot, nil)
		if c.ok != (err == nil) || (err != nil && (!errors.Is(err, ErrPolicy) || !strings.Contains(err.Error(), c.msg))) {
			t.Errorf("CheckUpdate(%q, %s, %s) = %v", c.ref, c.old[:4], c.new[:min(4, len(c.new))], err)
		}
	}
	// git's own check is consulted for branches.
	if err := CheckUpdate("refs/heads/ok", z, a, nil, func(string) error { return errors.New("no") }); err == nil {
		t.Error("the branch check was not used")
	}
	// A malformed glob protects everything (fail closed).
	if err := CheckUpdate("refs/heads/any", z, a, []string{"["}, nil); err == nil {
		t.Error("a malformed protected pattern let a push through")
	}
}

func TestParseHookInput(t *testing.T) {
	const z, a = "0000000000000000000000000000000000000000", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	got, err := ParseHookInput(strings.NewReader(z + " " + a + " refs/heads/x\n\n" + a + " " + z + " refs/tags/t\n"))
	if err != nil || len(got) != 2 || got[0].Ref != "refs/heads/x" || got[1].New != z {
		t.Errorf("%+v %v", got, err)
	}
	for _, bad := range []string{
		"x y refs/heads/x\n", z + " " + a + "\n", z + " " + a + " refs/heads/x extra\n", z + " " + a + " notaref\n",
		z + " " + a + " refs/heads/" + strings.Repeat("x", 5000) + "\n",
	} {
		if _, err := ParseHookInput(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad[:min(60, len(bad))])
		}
	}
}

func TestServeCall(t *testing.T) {
	h := &Host{HooksDir: "/s/local/git/no-hooks"}
	up, err := ServeCall("/s/relay/p.git", ServiceUploadPack, "/s/local/git/relay-hooks", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := h.Cmd(up)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "core.hooksPath=/s/local/git/no-hooks") || !strings.Contains(joined, "-c uploadpack.hideRefs=refs/devc upload-pack --strict") ||
		!strings.HasSuffix(joined, " /s/relay/p.git") || cmd.Dir != "/" || !slices.Contains(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Errorf("upload-pack: %q in %q env %q", cmd.Args, cmd.Dir, cmd.Env)
	}
	rp, _ := ServeCall("/s/relay/p.git", ServiceReceivePack, "/s/local/git/relay-hooks", []string{EnvRelayProject + "=r/p"}, nil)
	cmd, err = h.Cmd(rp)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(cmd.Args, " ")
	for _, want := range []string{"core.hooksPath=/s/local/git/relay-hooks", "receive.hideRefs=refs/devc", "receive.hideRefs=refs/remotes",
		"receive.denyDeletes=true", "receive.autogc=false", "receive.fsckObjects=true", "receive-pack /s/relay/p.git"} {
		if !strings.Contains(joined, want) {
			t.Errorf("receive-pack lacks %q: %q", want, cmd.Args)
		}
	}
	if strings.Contains(joined, "no-hooks") || !slices.Contains(cmd.Env, EnvRelayProject+"=r/p") || !slices.Contains(cmd.Unset, "GIT_*") {
		t.Errorf("receive-pack: %q env %q", cmd.Args, cmd.Env)
	}
	if _, err := ServeCall("/s/relay/p.git", "upload-archive", "/h", nil, nil); err == nil {
		t.Error("accepted another service")
	}
	if _, err := ServeCall("/s/relay/p.git", ServiceReceivePack, "rel", nil, nil); err == nil {
		t.Error("accepted a relative hooks directory")
	}
	if _, err := h.Cmd(Call{Relay: "/r.git", Args: []string{"log"}, Config: []string{"-x=1"}}); err == nil {
		t.Error("accepted an option as a setting")
	}
	// Background upstream commands: no prompts, no askpass, no terminal.
	bg := &Host{HooksDir: "/h", Background: true, Terminal: true}
	cmd, _ = bg.Cmd(Call{Relay: "/r.git", Args: []string{"fetch", "github"}, Mode: Upstream})
	if !cmd.Setsid || !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") || !slices.Contains(cmd.Unset, "SSH_ASKPASS") || !slices.Contains(cmd.Unset, "GIT_DIR") {
		t.Errorf("background upstream: setsid %v env %q unset %q", cmd.Setsid, cmd.Env, cmd.Unset)
	}
}

func TestEnsureRelayHooks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	if err := EnsureRelayHooks(dir, "/usr/bin/devc"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "pre-receive"), []byte("#!/bin/sh\n"), 0o755)
	os.Remove(filepath.Join(dir, HookUpdate))
	os.Symlink("/elsewhere", filepath.Join(dir, HookUpdate+".tmp"))
	if err := EnsureRelayHooks(dir, "/opt/devc2"); err != nil {
		t.Fatal(err)
	}
	des, _ := os.ReadDir(dir)
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
		if target, err := os.Readlink(filepath.Join(dir, de.Name())); err != nil || target != "/opt/devc2" {
			t.Errorf("%s -> %q %v", de.Name(), target, err)
		}
	}
	if !slices.Equal(names, []string{HookPostReceive, HookUpdate}) {
		t.Errorf("hooks directory holds %q", names)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", fi.Mode())
	}
	if err := EnsureRelayHooks(dir, "devc"); err == nil {
		t.Error("accepted a relative executable")
	}
}

// TestRecordPush checks that what a push through the relay service
// records fits banking (finding B8): a later bank neither reverts nor
// duplicates it, and a push of another local branch is no observation of
// the box's branch.
func TestRecordPush(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	def, err := w.svc.InitRelay(ctx, w.p)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.InitBox(ctx, w.p, def); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(w.box, "main")
	zero := Zero(w.git(main, "rev-parse", "HEAD"))
	// "receive-pack": the objects and the refs reach the relay; then
	// post-receive records.
	push := func(src, dst string) PushedRef {
		old := zero
		if o, err := w.p.Relay.Refs(ctx, dst); err == nil && len(o) == 1 {
			old = o[0].OID
		}
		w.git(main, "push", "-q", w.p.Relay.Dir, "+"+src+":"+dst)
		return PushedRef{Old: old, New: w.git(main, "rev-parse", src), Ref: dst}
	}
	w.git(main, "checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(main, "f"), []byte("feature\n"), 0o644)
	w.git(main, "commit", "-qam", "F1")
	f1 := w.git(main, "rev-parse", "HEAD")
	w.git(main, "tag", "t1")
	rep := w.svc.RecordPush(ctx, w.p, []PushedRef{push("feature", "refs/heads/feature"), push("t1", "refs/tags/t1")})
	if len(rep.Problems) != 0 || len(rep.Branches) != 1 || rep.Branches[0].BankRef == "" || len(rep.Tags) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if s := rep.Summary(); !strings.Contains(s, "feature ("+f1[:12]+")") || !strings.Contains(s, "tag t1") {
		t.Errorf("summary %q", s)
	}
	set, _ := w.p.Relay.RefSet(ctx)
	if set.BoxTips["feature"] != f1 || set.LastBank["feature"].OID != f1 || !set.DevcTags["t1"][f1] {
		t.Errorf("after the push: boxtips %v lastbank %v devc tags %v", set.BoxTips, set.LastBank, set.DevcTags)
	}
	// Recording the same push again adds nothing.
	rep = w.svc.RecordPush(ctx, w.p, []PushedRef{{Old: f1, New: f1, Ref: "refs/heads/feature"}, {Old: zero, New: f1, Ref: "refs/tags/t1"}})
	if len(rep.Branches) != 1 || rep.Branches[0].BankRef != "" || len(rep.Tags) != 0 {
		t.Errorf("repeated push: %+v", rep)
	}
	countBank := func(b string) int {
		refs, _ := w.p.Relay.Refs(ctx, BankPrefix+b+"/")
		return len(refs)
	}
	// A bank after the push changes nothing and records nothing again.
	res, err := w.svc.Bank(ctx, w.p)
	if err != nil || len(res.Branches) != 0 || countBank("feature") != 1 || len(res.Tags) != 0 {
		t.Errorf("bank after the push: %+v %v (%d bank refs)", res, err, countBank("feature"))
	}
	// The relay advances the branch (a pull fast-forwarding it): a bank
	// keeps that, because the box has not moved feature since the push.
	ahead := w.git(w.box, "--git-dir="+w.p.Relay.Dir, "commit-tree", "-p", f1, "-m", "upstream", f1+"^{tree}")
	if err := w.p.Relay.Update(ctx, []RefUpdate{{Ref: HeadsPrefix + "feature", New: ahead, Old: f1}}); err != nil {
		t.Fatal(err)
	}
	if res, err := w.svc.Bank(ctx, w.p); err != nil || len(res.Branches) != 0 {
		t.Errorf("bank after the relay advanced: %+v %v", res, err)
	}
	if set, _ := w.p.Relay.RefSet(ctx); set.Heads["feature"] != ahead {
		t.Errorf("the bank reverted feature to %s", set.Heads["feature"])
	}
	// Another local branch pushed as "other" (the box has no local other):
	// banked, but no box tip is observed for it.
	w.git(main, "checkout", "-q", "-b", "wip")
	w.git(main, "commit", "-q", "--allow-empty", "-m", "W1")
	rep = w.svc.RecordPush(ctx, w.p, []PushedRef{push("wip", "refs/heads/other")})
	set, _ = w.p.Relay.RefSet(ctx)
	if len(rep.Branches) != 1 || rep.Branches[0].BankRef == "" || set.BoxTips["other"] != "" {
		t.Errorf("push of another branch: %+v, box tip %q", rep, set.BoxTips["other"])
	}
	// "wip:feature" while the box's own feature stays behind: no box tip
	// moves, so the next bank does not take the push back.
	w.git(main, "commit", "-q", "--allow-empty", "-m", "W2")
	push("wip", "refs/heads/feature")
	w2 := w.git(main, "rev-parse", "wip")
	w.svc.RecordPush(ctx, w.p, []PushedRef{{Old: ahead, New: w2, Ref: "refs/heads/feature"}})
	if set, _ := w.p.Relay.RefSet(ctx); set.BoxTips["feature"] != f1 {
		t.Errorf("box tip of feature moved to %s", set.BoxTips["feature"])
	}
	if res, err := w.svc.Bank(ctx, w.p); err != nil {
		t.Fatal(err)
	} else if set, _ := w.p.Relay.RefSet(ctx); set.Heads["feature"] != w2 {
		t.Errorf("the bank took the push of wip:feature back: feature %s, bank %+v", set.Heads["feature"], res.Branches)
	}
	// A branch listed in git.forward reaches the upstream.
	w.git(main, "checkout", "-q", "-b", "fwd/a")
	rep = w.svc.RecordPush(ctx, w.p, []PushedRef{push("fwd/a", "refs/heads/fwd/a")})
	if len(rep.Forwards) != 1 || rep.Forwards[0].Error != "" || w.git(w.up, "rev-parse", "fwd/a") != w2 {
		t.Errorf("forward: %+v", rep.Forwards)
	}
	if out := FormatPush(rep); !strings.Contains(out, "forwarded fwd/a to the upstream") {
		t.Errorf("format: %q", out)
	}
}
