//go:build integration

package integration

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// TestEscape is the escape suite (work order C07 section 5, plan 3.13).
// The first realm has a project with data zones, a relay and a running
// session, and a second running box; the second realm has one running
// project. Every attempt is made from inside the first box as dev and must
// fail. The last group plants traps for host-side tools in everything the
// box can write and runs the host commands that read box-written data:
// nothing may run on the host, and no host file may leak into the journal
// or the context repository.
func TestEscape(t *testing.T) {
	e := gitSetup(t)
	e.Timeout = 5 * time.Minute
	up, _ := upstream(t)
	ref := itest.RealmA + "/esc"
	bdir := inBox(ref)
	hostDir := filepath.Join(e.ProjectsRoot, itest.RealmA, "esc")
	project(e, ref, "upstream = \"file://"+up+"\"\nharnesses = [\"fake\"]\n[grants]\nsecrets = [\"ESC_GRANTED\"]\n"+
		"[data]\ninputs = [\"raw\"]\nwork = [\"scratch\"]\nsealed = [\"results\"]\n")
	granted, other := "granted-"+itest.Nonce, "withheld-"+itest.Nonce
	for name, value := range map[string]string{"ESC_GRANTED": granted, "ESC_OTHER": other} {
		if res := e.RunIn(e.Repo, value+"\n", "secret", "set", itest.RealmA, name); res.ExitCode != 0 {
			t.Fatalf("secret set %s: %d %s", name, res.ExitCode, res.Stderr)
		}
	}
	project(e, tools, "image = \""+itest.BaseImage+"\"\n[network]\nlevel = \"none\"\n")
	e.MustRun("up", tools)
	e.MustRun("up", hello)
	e.MustRun("project", "init", ref)
	e.MustRun("up", ref)
	boxSh(t, e, ref, "cd "+bdir+"/data/results && mkdir -p run1/sub && echo hello > run1/a.txt && echo b > run1/sub/b.txt")
	e.MustRun("seal", "results/run1", ref)
	sid := strings.TrimSpace(e.MustRun("fake", ref, "--detach").Stdout)
	if !strings.HasPrefix(sid, "s") || len(sid) != 21 {
		t.Fatalf("fake --detach printed %q", sid)
	}
	r1 := bdir + "/data/results/run1"

	t.Run("sealed data and vault", func(t *testing.T) {
		expectAllFail(t, e, ref, "on a sealed entry", sealedEscapes(r1, "a.txt", "sub/b.txt"))
		expectAllFail(t, e, ref, "on the data layout", layoutEscapes(bdir))
		if b, err := os.ReadFile(filepath.Join(hostDir, "data", "results", "run1", "a.txt")); err != nil || string(b) != "hello\n" {
			t.Errorf("sealed file after the attempts: %q %v", b, err)
		}
	})

	t.Run("read-only parts", func(t *testing.T) {
		var scripts []string
		for _, dir := range []string{"/opt/devc", "/opt/devc/bin", "/etc/devc", "/etc/fakeagent", "/run/devc-secrets", bdir + "/data/raw"} {
			scripts = append(scripts, "touch "+dir+"/x", "mount -o remount,rw "+dir, "mount -o remount,rw,bind "+dir)
		}
		scripts = append(scripts, "echo x > /etc/devc/box.json", "echo x >> /etc/fakeagent/settings.json",
			"rm -f /run/devc-secrets/ESC_GRANTED", "rm -f /opt/devc/bin/devc", "umount /opt/devc", "umount -l /etc/devc")
		expectAllFail(t, e, ref, "on a read-only part", scripts)
	})

	t.Run("privilege", func(t *testing.T) {
		res := e.Run("run", ref, "--", "/opt/devc/bin/devc", "box", "selftest", "--json")
		var checks []struct {
			ID     string `json:"id"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(res.Stdout), &checks); err != nil || res.ExitCode != 0 {
			t.Errorf("selftest: exit %d, %v\n%s%s", res.ExitCode, err, res.Stdout, res.Stderr)
		}
		seen := map[string]bool{}
		for _, c := range checks {
			seen[c.ID] = true
			if !c.OK {
				t.Errorf("selftest %s: %s", c.ID, c.Detail)
			}
		}
		for _, id := range []string{"identity", "capabilities", "no-new-privileges", "userns-unshare", "clone3", "nftables"} {
			if !seen[id] {
				t.Errorf("selftest has no item %s", id)
			}
		}
		expectAllFail(t, e, ref, "for privilege", []string{
			"mount -t tmpfs none /tmp", "/bin/mount -t proc proc /mnt", "unshare -r true", "unshare -m true", "unshare -n true",
			"unshare -U true", "nsenter -t 1 -n true", "nsenter -t 1 -m true", "nsenter -t 1 -U true",
			"su -c true root </dev/null", "test \"$(id -u)\" = 0",
		})
		// No setuid program gains anything: no_new_privs (selftest item
		// no-new-privileges) makes the kernel ignore the bit; su and mount
		// above are two of them.
		t.Logf("setuid programs in the image: %q", runOK(t, e, ref, "sh", "-c", "find / -xdev -perm -4000 -type f 2>/dev/null || true"))
	})

	t.Run("other realm", func(t *testing.T) {
		mounts := runOK(t, e, ref, "cat", "/proc/self/mountinfo")
		for _, s := range []string{itest.RealmB, filepath.Join(e.ProjectsRoot, itest.RealmB), "/relay/"} {
			if strings.Contains(mounts, s) {
				t.Errorf("the box's mount table mentions %q", s)
			}
		}
		if code := runCode(e, ref, "test", "-e", "/w/"+itest.RealmB); code == 0 {
			t.Error("the other realm's project path exists in the box")
		}
		if got := runOK(t, e, ref, "ls", "/w"); got != itest.RealmA {
			t.Errorf("/w in the box: %q", got)
		}
		if env := runOK(t, e, ref, "env"); strings.Contains(env, itest.RealmB) {
			t.Errorf("the other realm appears in the environment")
		}
	})

	t.Run("network", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		accepted := make(chan struct{}, 16)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
				accepted <- struct{}{}
			}
		}()
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		var targets []string
		for _, a := range hostAddrs(t) {
			targets = append(targets, net.JoinHostPort(a.String(), port))
		}
		if alias := runOK(t, e, ref, "sh", "-c", "getent hosts host.containers.internal || true"); alias != "" {
			targets = append(targets, net.JoinHostPort(strings.Fields(alias)[0], port))
		}
		// A second box of the same realm with a server on all its
		// addresses.
		startServerOn(t, e, hello, "0.0.0.0", 8790)
		for _, a := range strings.Fields(runOK(t, e, hello, "hostname", "-I")) {
			targets = append(targets, net.JoinHostPort(a, "8790"))
		}
		for _, addr := range targets {
			if ok, _ := connect(e, ref, addr, 10); ok {
				t.Errorf("the box reached %s", addr)
			}
		}
		ln.Close()
		select {
		case <-accepted:
			t.Error("the host accepted a connection from the box")
		default:
		}
		if ok, msg := connect(e, ref, "192.0.2.1:9", 5); ok || !strings.Contains(msg, "refused") {
			t.Errorf("sentinel: %v %q", ok, msg)
		}
		if got := runOK(t, e, ref, "sh", "-c", "cat /proc/sys/net/ipv6/conf/all/disable_ipv6; wc -l < /proc/net/if_inet6"); got != "1\n0" {
			t.Errorf("IPv6: %q", got)
		}
		// The rules cannot be listed or changed from inside (selftest item
		// nftables, above); they are still in place.
		if !rulesInPlace(t, ref) {
			t.Error("rules missing")
		}
	})

	t.Run("secrets", func(t *testing.T) {
		if got := runOK(t, e, ref, "ls", "/run/devc-secrets"); got != "ESC_GRANTED" {
			t.Errorf("secrets in the box: %q", got)
		}
		all := runOK(t, e, ref, "sh", "-c", "env; cat /run/devc-secrets/*")
		if strings.Contains(all, other) || !strings.Contains(all, granted) {
			t.Errorf("secrets: granted present %v, withheld present %v", strings.Contains(all, granted), strings.Contains(all, other))
		}
	})

	t.Run("git", func(t *testing.T) {
		before := relayRefs(t, e, ref)
		main := bdir + "/main"
		for _, push := range []string{
			"git push origin HEAD:refs/heads/evil", "git push origin :main", "git push --force origin HEAD:refs/devc/bank/main/1",
			"git push origin HEAD:refs/remotes/github/main", "git push --force origin HEAD:refs/tags/v1", "git push origin --mirror",
			"git push origin HEAD:refs/devc/incoming/main",
		} {
			e.Run("run", ref, "--", "sh", "-c", "cd "+main+" && git commit -q --allow-empty -m odd && "+push)
		}
		after := relayRefs(t, e, ref)
		// With a daemon (DEVC_ITEST_DAEMON=1) the relay service lets a box
		// create and move branches, which are banked at once (C10a); nothing
		// else may change, and nothing may disappear.
		mayMove := func(name string) bool {
			return itest.DaemonMode() && (strings.HasPrefix(name, "refs/heads/") || strings.HasPrefix(name, "refs/devc/boxtip/"))
		}
		for name, v := range before {
			if after[name] != v && !(mayMove(name) && after[name] != "") {
				t.Errorf("relay ref %s changed: %s -> %s", name, v, after[name])
			}
		}
		for name := range after {
			if before[name] == "" && !(mayMove(name) || (itest.DaemonMode() && strings.HasPrefix(name, "refs/devc/bank/"))) {
				t.Errorf("relay ref %s appeared", name)
			}
		}
		if code := runCode(e, ref, "test", "-e", relayDir(e, ref)); code == 0 {
			t.Error("the relay's host path exists in the box")
		}
		if code := runCode(e, ref, "sh", "-c", "find / -xdev -name '*.git' -path '*relay*' 2>/dev/null | grep -q ."); code == 0 {
			t.Error("a relay is visible in the box")
		}
	})

	t.Run("host side", func(t *testing.T) {
		// Hooks, core.fsmonitor, filters and other configuration in .repo;
		// bank, pull, push, log, diff, down and up must run none of them.
		hostileRepository(t, e, ref, up)

		markers := t.TempDir()
		secret := filepath.Join(t.TempDir(), "hostsecret")
		leak := "HOSTSECRET-" + itest.Nonce
		writeFile(t, secret, leak+"\n")
		trap := filepath.Join(hostDir, "escape-trap")
		writeFile(t, trap, "#!/bin/sh\ntouch "+markers+"/\"$(basename \"$0\")-$$\"\nexit 0\n")
		if err := os.Chmod(trap, 0o755); err != nil {
			t.Fatal(err)
		}
		sid2 := strings.TrimSpace(e.MustRun("fake", ref, "--detach").Stdout)
		// A live session (the first one ended with the box in
		// hostileRepository), with its spool file in place.
		live := strings.TrimSpace(e.MustRun("fake", ref, "--detach").Stdout)
		liveSpool := filepath.Join(e.RuntimeDir, boxID(ref), "spool", live+".jsonl")
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(liveSpool); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		const other = "s0000000000000000000a"
		// The spool links point at valid events of the very sessions their
		// names claim, carrying the host file's marker: a host that followed
		// them would journal the marker (finding B10).
		spoolSecrets := map[string]string{}
		for _, id := range []string{sid2, other} {
			f := filepath.Join(t.TempDir(), "event.jsonl")
			writeFile(t, f, validEvent(t, id, leak))
			spoolSecrets[id] = f
		}
		main := bdir + "/main"
		tasks := `{"version":"2.0.0","tasks":[{"label":"x","type":"shell","command":"` + trap + `","runOptions":{"runOn":"folderOpen"}}]}`
		boxSh(t, e, ref, strings.Join([]string{
			// The trap at the same absolute path in the box.
			"mkdir -p " + hostDir + " && cp " + bdir + "/escape-trap " + trap,
			// direnv and editor tasks.
			"printf '%s\\n' '" + trap + "' > " + main + "/.envrc",
			"cp " + main + "/.envrc " + bdir + "/.envrc",
			"mkdir -p " + main + "/.vscode && printf '%s' '" + tasks + "' > " + main + "/.vscode/tasks.json",
			// New uncommitted work, so the bank below has something to do.
			"echo escape-change >> " + main + "/a",
			// Spool entries pointing at host files.
			"rm -f /run/devc/spool/" + sid2 + ".jsonl",
			"ln -sf " + spoolSecrets[sid2] + " /run/devc/spool/" + sid2 + ".jsonl",
			"ln -sf " + spoolSecrets[other] + " /run/devc/spool/" + other + ".jsonl",
			"ln -s /etc /run/devc/spool/etc",
			// A .git file and a repository in the context notes, and a
			// link to a host file.
			"mkdir -p /ctx/notes/evil/hooks /ctx/notes/evil/objects /ctx/notes/evil/refs",
			"printf 'gitdir: /ctx/notes/evil\\n' > /ctx/notes/.git",
			"printf 'ref: refs/heads/main\\n' > /ctx/notes/evil/HEAD",
			"printf '[core]\\n\\tfsmonitor = " + trap + "\\n\\thooksPath = /ctx/notes/evil/hooks\\n' > /ctx/notes/evil/config",
			"for h in pre-commit post-commit post-checkout reference-transaction fsmonitor-watchman; do cp " + trap + " /ctx/notes/evil/hooks/$h; done",
			"ln -s " + secret + " /ctx/notes/leak",
			"echo notes > /ctx/notes/plain.md",
		}, " && "))
		// A valid event in the live session's own spool file: ingestion
		// must get past the planted entries and journal it.
		ingested := "ESCAPE-INGESTED-" + itest.Nonce
		sf, err := os.OpenFile(liveSpool, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sf.WriteString(validEvent(t, live, ingested))
		sf.Close()
		if err != nil {
			t.Fatal(err)
		}

		// Every command that reads box-written data must succeed and have
		// its effect (finding B11).
		effects := map[string]func(itest.Result) bool{
			"ls":                             func(r itest.Result) bool { return strings.Contains(r.Stdout, ref) },
			"ps --all":                       func(r itest.Result) bool { return strings.Contains(r.Stdout, live[1:13]) },
			"bank " + ref:                    func(r itest.Result) bool { return strings.Contains(r.Stdout, "snapshot of main") },
			"data ls " + ref:                 func(r itest.Result) bool { return strings.Contains(r.Stdout, "results/run1") },
			"context commit " + itest.RealmA: func(r itest.Result) bool { return strings.Contains(r.Stdout, "committed") },
			"down " + ref:                    func(itest.Result) bool { return !inspect(t, ref).State.Running },
			"context log " + itest.RealmA:    func(r itest.Result) bool { return strings.Contains(r.Stdout, "devc context commit") },
			"context diff " + itest.RealmA:   func(itest.Result) bool { return true },
		}
		for _, args := range [][]string{
			{"ls"}, {"ps", "--all"}, {"bank", ref}, {"data", "ls", ref}, {"context", "commit", itest.RealmA}, {"down", ref},
			{"ls"}, {"ps", "--all"}, {"context", "log", itest.RealmA}, {"context", "diff", itest.RealmA},
		} {
			name := strings.Join(args, " ")
			res := e.Run(args...)
			if res.ExitCode != 0 {
				t.Errorf("devc %s: exit %d\n%s%s", name, res.ExitCode, res.Stdout, res.Stderr)
			} else if ok := effects[name]; ok != nil && !ok(res) {
				t.Errorf("devc %s did not do its work:\n%s%s", name, res.Stdout, res.Stderr)
			}
			if strings.Contains(res.Stdout+res.Stderr, leak) {
				t.Errorf("devc %s printed the host file's content", name)
			}
		}
		des, err := os.ReadDir(markers)
		if err != nil {
			t.Fatal(err)
		}
		for _, de := range des {
			t.Errorf("trap ran on the host: %s", de.Name())
		}
		// The journal and the context repository hold nothing of the host
		// file; the valid event was journaled, the session ended with the
		// box, and the planted sessions have no journal.
		jdir := filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "journal", "esc")
		jdes, err := os.ReadDir(jdir)
		if err != nil {
			t.Fatalf("listing the journals: %v", err)
		}
		seen := map[string]bool{}
		for _, de := range jdes {
			seen[de.Name()] = true
			b, err := os.ReadFile(filepath.Join(jdir, de.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), leak) || strings.Contains(string(b), "root:x:0:0") {
				t.Errorf("journal %s holds host file content", de.Name())
			}
		}
		if !seen[live+".jsonl"] || seen[other+".jsonl"] {
			t.Errorf("journals: %v", seen)
		}
		if b, _ := os.ReadFile(filepath.Join(jdir, live+".jsonl")); !strings.Contains(string(b), ingested) || !strings.Contains(string(b), `"box-down"`) {
			t.Errorf("journal of %s lacks the valid event or the end:\n%s", live, b)
		}
		ctx := filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "context", ".git")
		history := hostGit(t, "", "--git-dir="+ctx, "log", "--all", "-p", "--no-ext-diff", "--no-textconv")
		if strings.Contains(history, leak) {
			t.Error("the context repository holds the host file's content")
		}
		tree := hostGit(t, "", "--git-dir="+ctx, "ls-tree", "-r", "--name-only", "main")
		if !regexp.MustCompile(`(?m)^notes/esc/plain\.md$`).MatchString(tree) {
			t.Errorf("the context commit misses the plain note:\n%s", tree)
		}
		for _, bad := range []string{"notes/esc/.git", "notes/esc/leak"} {
			if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(bad) + `$`).MatchString(tree) {
				t.Errorf("the context repository contains %s", bad)
			}
		}
	})
}

// validEvent is one valid spool line of session sid: a note with text.
func validEvent(t *testing.T, sid, text string) string {
	t.Helper()
	ev, err := proto.New(sid, proto.TypeNote, proto.Note{Text: text}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// startServerOn runs startServer's one-line server on any address.
func startServerOn(t *testing.T, e *itest.Env, ref, addr string, port int) {
	t.Helper()
	script := `use IO::Socket::INET; my $s = IO::Socket::INET->new(LocalAddr => "` + addr + `", LocalPort => ` + strconv.Itoa(port) +
		`, Listen => 5, ReuseAddr => 1) or die "listen: $!"; while (my $c = $s->accept) { print $c "hello from the box\n"; close $c }`
	e.MustRun("run", ref, "--", "setsid", "-f", "perl", "-e", script)
	for i := 0; i < 50; i++ {
		if runCode(e, ref, "timeout", "2", "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/"+strconv.Itoa(port)) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the server in %s did not come up", ref)
}

// TestRecreateKeepsState shows the phase 1 "done when" item that needs no
// real login: a box rebuilt or removed and created again keeps the
// harness's stored token, its history in the home and in the realm-shared
// state, and the session journal.
func TestRecreateKeepsState(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	project(e, hello, "harnesses = [\"fake\"]\n")
	install(t, e, "fake")
	const token = "tok-recreate-789"
	if res := e.RunIn(e.Repo, token+"\n", "login", itest.RealmA, "fake", "--token-stdin"); res.ExitCode != 0 {
		t.Fatalf("login: %d %s", res.ExitCode, res.Stderr)
	}
	e.MustRun("up", hello)
	runOK(t, e, hello, "fake", "-p", "touch-state history/h1")
	runOK(t, e, hello, "fake", "-p", "touch-state state/s1")
	sid := startDetached(t, e)
	e.MustRun("stop", sid)
	waitSession(t, e, sid, "ended", func(r psRow) bool { return !r.Live })

	check := func(when string) {
		t.Helper()
		if got := runOK(t, e, hello, "fake", "-p", "env FAKE_TOKEN"); got != "FAKE_TOKEN="+sha(token) {
			t.Errorf("%s: token %q", when, got)
		}
		if got := runOK(t, e, hello, "sh", "-c", "cat ~/.fakeagent/history/h1 ~/.fakeagent/state/s1"); got != "hello\nhello" {
			t.Errorf("%s: harness history %q", when, got)
		}
		if _, ok := ps(t, e, "--all")[sid]; !ok {
			t.Errorf("%s: session %s is gone from the journal", when, sid)
		}
	}
	old := inspect(t, hello).ID
	e.Timeout = 10 * time.Minute
	e.MustRun("rebuild", hello)
	if inspect(t, hello).ID == old {
		t.Fatal("rebuild did not recreate the box")
	}
	check("after rebuild")
	e.MustRun("rm", hello)
	e.MustRun("up", hello)
	check("after rm and up")
}
