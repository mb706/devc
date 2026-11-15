//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/daemon"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// boxRun runs a shell script as dev in a box and returns its result.
func boxRun(e *itest.Env, ref, script string) itest.Result {
	return e.Run("run", ref, "--", "sh", "-c", script)
}

// appendConfig appends text to a file below the configuration directory.
func appendConfig(t *testing.T, e *itest.Env, rel, text string) {
	t.Helper()
	p := filepath.Join(e.ConfigDir, rel)
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	e.WriteConfig(rel, string(b)+"\n"+text+"\n")
}

func projectFile(ref string) string {
	realm, name, _ := strings.Cut(ref, "/")
	return filepath.Join("realms", realm, "projects", name, "project.toml")
}

// pollFor polls cond for up to d.
func pollFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return cond()
}

// TestRelayService covers work order C10a section 4, items 1-8.
func TestRelayService(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	e.Timeout = 5 * time.Minute
	up, work := upstream(t)
	upQ, workQ := upstream(t)
	hostGit(t, workQ, "checkout", "-q", "-b", "qonly")
	hostGit(t, workQ, "commit", "-q", "--allow-empty", "-m", "Q1")
	hostGit(t, workQ, "push", "-q", "origin", "qonly")
	ref := itest.RealmA + "/relayp"
	refQ := itest.RealmA + "/relayq"
	e.MustRun("project", "add", ref, "--from", "file://"+up, "--harness", "fake")
	appendConfig(t, e, projectFile(ref), "[git]\nprotected = [\"prot\"]\nforward = [\"fwd/*\"]")
	e.MustRun("project", "add", refQ, "--from", "file://"+upQ, "--harness", "fake")
	d := itest.StartDaemon(t, e, "--housekeeping-interval", "1h")
	waitBox(t, e, ref, proto.ChannelConnected, "")
	waitBox(t, e, refQ, proto.ChannelConnected, "")
	main := inBox(ref) + "/main"
	relay := relayDir(e, ref)
	inMain := func(script string) itest.Result { return boxRun(e, ref, "cd "+main+" && "+script) }
	mustMain := func(script string) string {
		t.Helper()
		res := inMain(script)
		if res.ExitCode != 0 {
			t.Fatalf("in %s: %s: exit %d\nstdout: %s\nstderr: %s", ref, script, res.ExitCode, res.Stdout, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}

	// 1. git push lands in the relay with a bank ref; the branch is in sync.
	res := inMain("git checkout -q -b feature && echo feature > f && git add f && git commit -qm F1 && git push -u origin feature")
	if res.ExitCode != 0 {
		t.Fatalf("push: exit %d\n%s%s\ndaemon:\n%s", res.ExitCode, res.Stdout, res.Stderr, d.Log())
	}
	f1 := mustMain("git rev-parse HEAD")
	refs := relayRefs(t, e, ref)
	if refs["refs/heads/feature"] != f1 || !valuesWithPrefix(refs, "refs/devc/bank/feature/")[f1] || refs["refs/devc/boxtip/feature"] != f1 {
		t.Errorf("relay after the push: %v", refs)
	}
	if got := mustMain("git status -sb | head -1"); got != "## feature...origin/feature" {
		t.Errorf("status after the push: %q", got)
	}
	// A bank afterwards neither moves nor records anything again.
	e.MustRun("bank", ref)
	if again := relayRefs(t, e, ref); len(withPrefix(again, "refs/devc/bank/feature/")) != 1 || again["refs/heads/feature"] != f1 {
		t.Errorf("bank after the push: %v", withPrefix(again, "refs/devc/bank/feature/"))
	}

	// 4 (with the fetch half of 1). A commit pushed to the upstream by a
	// third clone and a commit added to the relay on the host both reach
	// the box with git fetch, without devc pull.
	writeFile(t, filepath.Join(work, "a"), "two\n")
	hostGit(t, work, "commit", "-qam", "U2")
	hostGit(t, work, "push", "-q", "origin", "main")
	u2 := hostGit(t, work, "rev-parse", "HEAD")
	tree := hostGit(t, "", "--git-dir="+relay, "rev-parse", f1+"^{tree}")
	hostC := hostGit(t, "", "--git-dir="+relay, "commit-tree", "-p", f1, "-m", "from the host", tree)
	hostGit(t, "", "--git-dir="+relay, "update-ref", "refs/heads/hostbr", hostC)
	mustMain("git fetch -q origin")
	if got := mustMain("git rev-parse origin/hostbr origin/main github/main"); got != hostC+"\n"+u2+"\n"+u2 {
		t.Errorf("after fetch: %q, want hostbr %s and main %s\ndaemon:\n%s", got, hostC, u2, d.Log())
	}
	if got := mustMain("git tag -l v1"); got != "v1" {
		t.Errorf("tags after fetch: %q", got)
	}
	if out := mustMain("git for-each-ref refs/devc refs/remotes/origin/devc"); out != "" {
		t.Errorf("devc's refs reached the box: %q", out)
	}

	// 2. Policy: refusals with a message, tag creation works.
	mustMain("git branch prot")
	for _, c := range []struct{ script, msg, gone string }{
		{"git push origin --delete feature", "deletion prohibited", ""},
		{"git push origin prot", "protected", "refs/heads/prot"},
		{"git push origin HEAD:refs/devc/x", "hidden ref", "refs/devc/x"},
		{"git push origin HEAD:refs/remotes/github/main", "hidden ref", ""},
	} {
		res := inMain(c.script)
		if res.ExitCode == 0 || !strings.Contains(res.Stderr, c.msg) {
			t.Errorf("%s: exit %d, stderr %q", c.script, res.ExitCode, res.Stderr)
		}
		if c.gone != "" && relayRefs(t, e, ref)[c.gone] != "" {
			t.Errorf("%s reached the relay", c.gone)
		}
	}
	if relayRefs(t, e, ref)["refs/heads/feature"] != f1 {
		t.Error("feature was deleted")
	}
	mustMain("git tag t1 && git push -q origin t1")
	refs = relayRefs(t, e, ref)
	if refs["refs/tags/t1"] != f1 || !valuesWithPrefix(refs, "refs/devc/tags/t1/")[f1] {
		t.Errorf("tag t1: %v", refs)
	}
	res = inMain("git tag -f t1 HEAD~1 >/dev/null && git push -f origin t1")
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "not moved") || relayRefs(t, e, ref)["refs/tags/t1"] != f1 {
		t.Errorf("moving a tag: exit %d, stderr %q", res.ExitCode, res.Stderr)
	}

	// 3. A branch matching git.forward reaches the upstream; the pusher
	// sees the result.
	res = inMain("git checkout -q -b fwd/a && git commit -q --allow-empty -m FWD && git push origin fwd/a")
	fwd := mustMain("git rev-parse HEAD")
	if res.ExitCode != 0 || !strings.Contains(res.Stderr, "forwarded fwd/a to the upstream") || hostGit(t, "", "--git-dir="+up, "rev-parse", "fwd/a") != fwd {
		t.Errorf("forward: exit %d, stderr %q", res.ExitCode, res.Stderr)
	}

	// 5. Each box reaches only its own relay, whatever URL origin has.
	mainQ := inBox(refQ) + "/main"
	res = boxRun(e, refQ, "cd "+mainQ+" && git remote set-url origin devc::"+ref+" && git ls-remote origin")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "refs/heads/qonly") || strings.Contains(res.Stdout, "refs/heads/feature") {
		t.Errorf("q's ls-remote with p's URL: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res := boxRun(e, refQ, "cd "+mainQ+" && git push -q origin HEAD:refs/heads/fromq"); res.ExitCode != 0 {
		t.Errorf("q's push: %s", res.Stderr)
	}
	if relayRefs(t, e, refQ)["refs/heads/fromq"] == "" || relayRefs(t, e, ref)["refs/heads/fromq"] != "" {
		t.Error("q's push did not land in q's relay only")
	}

	// 6. Garbage on git streams disturbs neither the daemon nor the boxes.
	for _, svc := range []string{"receive-pack", "upload-pack"} {
		boxRun(e, ref, "head -c 300000 /dev/urandom | (printf 'capabilities\\nconnect git-"+svc+"\\n'; cat) | /opt/devc/bin/git-remote-devc origin x >/dev/null 2>&1; true")
	}
	if !d.Running() {
		t.Fatalf("the daemon died:\n%s", d.Log())
	}
	if res := boxRun(e, refQ, "cd "+mainQ+" && git fetch -q origin && git ls-remote origin"); res.ExitCode != 0 {
		t.Errorf("q after the garbage: %s", res.Stderr)
	}
	if res := inMain("git ls-remote origin"); res.ExitCode != 0 || !strings.Contains(res.Stdout, "refs/heads/feature") {
		t.Errorf("p after the garbage: %s", res.Stderr)
	}

	// 8. A turn that edits a file without committing: a snapshot reaches
	// the relay after the turn.
	mustMain("git checkout -q feature")
	if res := e.Run("run", ref, "--", "fake", "-p", "write "+main+"/turn.txt from the turn"); res.ExitCode != 0 {
		t.Fatalf("fake turn: %s", res.Stderr)
	}
	snapped := pollFor(90*time.Second, func() bool {
		for _, name := range withPrefix(relayRefs(t, e, ref), "refs/devc/snap/main/") {
			if code, out, _ := hostGitCode("", "--git-dir="+relay, "ls-tree", "--name-only", name); code == 0 && strings.Contains(out, "turn.txt") {
				return true
			}
		}
		return false
	})
	if !snapped {
		t.Errorf("no snapshot with turn.txt after the turn: %v\ndaemon:\n%s", withPrefix(relayRefs(t, e, ref), "refs/devc/snap/"), d.Log())
	}

	// 7. With the daemon stopped the stub is back and nothing hangs.
	d.Stop()
	start := time.Now()
	res = inMain("git push origin HEAD:refs/heads/stubbed && git fetch origin")
	if res.ExitCode != 0 || !strings.Contains(res.Stderr, "banked on the host by devc") || time.Since(start) > 60*time.Second {
		t.Errorf("stub: exit %d after %s, stderr %q", res.ExitCode, time.Since(start), res.Stderr)
	}
	if relayRefs(t, e, ref)["refs/heads/stubbed"] != "" {
		t.Error("the stub reached the relay")
	}
}

// requestID finds the request ID in what `devc request` printed.
func requestID(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^(r[0-9a-v]{20}) `).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no request ID in %q", out)
	}
	return m[1]
}

// TestRequests covers work order C10a section 4, items 9-12.
func TestRequests(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	e.Timeout = 5 * time.Minute
	ref := itest.RealmA + "/reqp"
	project(e, ref, "harnesses = [\"fake\"]\n[data]\ninputs = [\"raw\"]\nwork = [\"scratch\"]\nsealed = [\"results\"]\n[requests]\nauto = [\"seal\"]\n")
	e.MustRun("up", ref)
	d := itest.StartDaemon(t, e, "--housekeeping-interval", "1h")
	waitBox(t, e, ref, proto.ChannelConnected, "")
	data := inBox(ref) + "/data"
	listAll := func() []proto.Request {
		t.Helper()
		var list []proto.Request
		if err := json.Unmarshal([]byte(e.MustRun("requests", "--all", "--json").Stdout), &list); err != nil {
			t.Fatal(err)
		}
		return list
	}

	// 9. A seal request is approved automatically and carried out; status
	// --wait returns done.
	boxSh(t, e, ref, "mkdir -p "+data+"/results/run1 && echo out > "+data+"/results/run1/out.txt")
	res := e.Run("run", ref, "--", "devc", "request", "seal", "results/run1")
	if res.ExitCode != 0 {
		t.Fatalf("request seal: %s%s\ndaemon:\n%s", res.Stdout, res.Stderr, d.Log())
	}
	id := requestID(t, res.Stdout)
	if res := e.Run("run", ref, "--", "devc", "request", "status", id, "--wait"); res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, id+" done") {
		t.Errorf("status --wait: exit %d %q %q\ndaemon:\n%s", res.ExitCode, res.Stdout, res.Stderr, d.Log())
	}
	if res := boxRun(e, ref, "touch "+data+"/results/run1/new"); res.ExitCode == 0 {
		t.Error("the sealed run is still writable")
	}
	if out := e.MustRun("data", "ls", ref).Stdout; !strings.Contains(out, "run1") {
		t.Errorf("data ls: %q", out)
	}
	// Through a worktree's data link, given as an absolute path.
	// (The project has no repository; the link is made as devc's data links
	// would.)
	boxSh(t, e, ref, "mkdir -p "+data+"/results/run2 "+inBox(ref)+"/main && ln -sfn ../data "+inBox(ref)+"/main/data")
	res = e.Run("run", ref, "--", "devc", "request", "seal", inBox(ref)+"/main/data/results/run2")
	if res.ExitCode != 0 {
		t.Fatalf("seal through the data link: %s", res.Stderr)
	}
	if res := e.Run("run", ref, "--", "devc", "request", "status", requestID(t, res.Stdout), "--wait"); res.ExitCode != 0 {
		t.Errorf("seal through the data link: %q %q", res.Stdout, res.Stderr)
	}

	// 10. A package request waits; approve runs it through devc root (a
	// stub manager) and the root log records it; deny changes nothing.
	e.MustRun("root", ref, "--", "sh", "-c", "printf '#!/bin/sh\\necho \"$@\" >> /var/tmp/apk-args\\n' > /usr/local/bin/apk && chmod 755 /usr/local/bin/apk")
	res = e.Run("run", ref, "--", "devc", "request", "package", "apk", "testpkg")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, " pending") {
		t.Fatalf("request package: %q %q", res.Stdout, res.Stderr)
	}
	pkg := requestID(t, res.Stdout)
	if out := e.MustRun("requests").Stdout; !strings.Contains(out, pkg) || !strings.Contains(out, "high") {
		t.Errorf("requests: %q", out)
	}
	if res := boxRun(e, ref, "cat /var/tmp/apk-args 2>/dev/null"); strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("the package request ran before approval: %q", res.Stdout)
	}
	if res := e.Run("requests", "approve", pkg, "--wait"); res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, pkg+" done") {
		t.Errorf("approve: exit %d %q %q\ndaemon:\n%s", res.ExitCode, res.Stdout, res.Stderr, d.Log())
	}
	if got := boxSh(t, e, ref, "cat /var/tmp/apk-args"); got != "add --no-progress testpkg" {
		t.Errorf("the stub manager got %q", got)
	}
	rootLog, _ := os.ReadFile(filepath.Join(e.ConfigDir, "envs", itest.EnvBase, "root-log.jsonl"))
	if !strings.Contains(string(rootLog), `"apk","add","--no-progress","testpkg"`) {
		t.Errorf("root log: %s", rootLog)
	}
	res = e.Run("run", ref, "--", "devc", "request", "package", "apk", "otherpkg")
	other := requestID(t, res.Stdout)
	if res := e.Run("requests", "deny", other, "--reason", "not now"); res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, other+" denied") {
		t.Errorf("deny: %q %q", res.Stdout, res.Stderr)
	}
	if res := e.Run("run", ref, "--", "devc", "request", "status", other); res.ExitCode != 1 || !strings.Contains(res.Stdout, "denied: not now") {
		t.Errorf("status of the denied request: %d %q", res.ExitCode, res.Stdout)
	}
	if got := boxSh(t, e, ref, "cat /var/tmp/apk-args"); got != "add --no-progress testpkg" {
		t.Errorf("deny changed the box: %q", got)
	}

	// 11. A high-risk decision with a non-local origin is refused; a
	// low-risk one is accepted.
	high := requestID(t, e.MustRun("run", ref, "--", "devc", "request", "package", "apk", "third").Stdout)
	low := requestID(t, e.MustRun("run", ref, "--", "devc", "request", "note", "please", "look").Stdout)
	cl, err := daemon.Dial(context.Background(), e.RuntimeDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	var dec proto.DecideResult
	err = cl.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: high, Approve: true, Origin: proto.OriginHub}, &dec)
	if !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("hub approval of a package request: %v", err)
	}
	if err := cl.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: low, Approve: true, Origin: proto.OriginHub}, &dec); err != nil || dec.Request.Status != proto.RequestDone {
		t.Errorf("hub approval of a note: %+v %v", dec, err)
	}
	for _, r := range listAll() {
		if r.ID == high && r.Status != proto.RequestPending {
			t.Errorf("the refused decision changed the request: %+v", r)
		}
	}

	// 12. Hostile requests are rejected and stored nowhere, also when the
	// box bypasses devc's own client.
	before := len(listAll())
	fake := "/opt/devc/harness/fake/current/bin/fakeagent"
	for _, params := range []string{
		`{"kind":"package","manager":"apt","packages":["jq;id"]}`,
		`{"kind":"package","manager":"apt","packages":["$(touch /tmp/x)"]}`,
		`{"kind":"package","manager":"apt","packages":["-o","APT::Get::x=1"]}`,
		`{"kind":"seal","path":"../../../etc"}`,
		`{"kind":"seal","path":"results/../../x"}`,
		`{"kind":"note","text":"` + strings.Repeat("x", 3000) + `"}`,
		`{"kind":"note","text":"bell\u0007"}`,
		`{"kind":"exec","text":"id"}`,
		`{"kind":"note","text":"x","session":"../../../etc/passwd"}`,
	} {
		res := e.Run("run", ref, "--", fake, "--agent-call", "host.request", params)
		if !strings.Contains(res.Stdout, `"code":-32602`) {
			t.Errorf("hostile %s: %q %q", params[:min(60, len(params))], res.Stdout, res.Stderr)
		}
	}
	if res := e.Run("run", ref, "--", "devc", "request", "package", "apt", "jq;id"); res.ExitCode != 2 {
		t.Errorf("devc request with a hostile name: exit %d", res.ExitCode)
	}
	if after := len(listAll()); after != before {
		t.Errorf("hostile requests were stored: %d -> %d", before, after)
	}
}

// TestSecretsDaemon covers work order C10a section 4, item 13.
func TestSecretsDaemon(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	cfg, err := os.ReadFile(filepath.Join(e.ConfigDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), `delivery = "files"`) {
		t.Fatalf("the example config.toml has no secrets.delivery to change:\n%s", cfg)
	}
	e.WriteConfig("config.toml", strings.Replace(string(cfg), `delivery = "files"`, `delivery = "daemon"`, 1))
	ref := itest.RealmA + "/secp"
	project(e, ref, "harnesses = [\"fake\"]\n[grants]\nsecrets = [\"ITEST_GRANT\"]\n")
	const token, grant = "tok-daemon-123", "grant-value-456"
	if res := e.RunIn(e.Repo, token+"\n", "login", itest.RealmA, "fake", "--token-stdin"); res.ExitCode != 0 {
		t.Fatal(res.Stderr)
	}
	for name, v := range map[string]string{"ITEST_GRANT": grant, "ITEST_OTHER": "other-value"} {
		if res := e.RunIn(e.Repo, v+"\n", "secret", "set", itest.RealmA, name); res.ExitCode != 0 {
			t.Fatal(res.Stderr)
		}
	}
	e.MustRun("up", ref)
	d := itest.StartDaemon(t, e, "--housekeeping-interval", "1h")
	waitBox(t, e, ref, proto.ChannelConnected, "")
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return "sha256:" + hex.EncodeToString(h[:])
	}
	res := e.Run("run", ref, "--", "fake", "-p", "env FAKE_TOKEN ITEST_GRANT")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "FAKE_TOKEN="+sum(token)) || !strings.Contains(res.Stdout, "ITEST_GRANT="+sum(grant)) {
		t.Errorf("harness environment: %q %q\ndaemon:\n%s", res.Stdout, res.Stderr, d.Log())
	}
	if got := boxSh(t, e, ref, "ls -A /run/devc-secrets"); got != "" {
		t.Errorf("/run/devc-secrets holds %q", got)
	}
	if res := e.Run("run", ref, "--", "devc", "box", "secret", "ITEST_OTHER"); res.ExitCode == 0 || !strings.Contains(res.Stderr, "not granted") || strings.Contains(res.Stdout, "other-value") {
		t.Errorf("ungranted secret: %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res := e.Run("run", ref, "--", "devc", "box", "secret", "ITEST_GRANT"); res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != grant {
		t.Errorf("granted secret: %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if strings.Contains(d.Log(), token) || strings.Contains(d.Log(), grant) {
		t.Error("a secret's value reached the daemon's log")
	}
	// Without the daemon the harness starts without its credential and
	// says how to fix it.
	d.Stop()
	res = e.Run("run", ref, "--", "fake", "-p", "env FAKE_TOKEN")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "FAKE_TOKEN=<unset>") || !strings.Contains(res.Stderr, "not reachable") {
		t.Errorf("without the daemon: %q %q", res.Stdout, res.Stderr)
	}
}
