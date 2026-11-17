//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// spoolMessages is a script that appends n message events of about
// size bytes to the spool file of sid in the box (as any process in the
// box can).
func spoolMessages(sid string, n, size int, end bool) string {
	s := fmt.Sprintf(`pad=$(head -c %d /dev/zero | tr '\0' m); for i in $(seq 1 %d); do printf '{"v":1,"ts":"2026-11-17T00:00:00Z","session":"%s","type":"message","data":{"text":"%%04d %%s"}}\n' "$i" "$pad"; done >> /run/devc/spool/%s.jsonl`,
		size, n, sid, sid)
	if end {
		s += fmt.Sprintf(` && printf '{"v":1,"ts":"2026-11-17T00:00:00Z","session":"%s","type":"session.end","data":{"reason":"exit"}}\n' >> /run/devc/spool/%s.jsonl`, sid, sid)
	}
	return s
}

type jline struct {
	Type  string          `json:"type"`
	Spool *int64          `json:"spool"`
	Data  json.RawMessage `json:"data"`
}

func readJournal(t *testing.T, file string) ([]jline, []byte) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, nil
	}
	var out []jline
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var j jline
		if err := json.Unmarshal([]byte(l), &j); err != nil {
			t.Fatalf("journal line %q: %v", l, err)
		}
		out = append(out, j)
	}
	return out, b
}

// TestJournalBudgets: a box cannot make the host's journals grow without
// bound (C20a, X6 3). With small budgets set in project.toml, one large
// session stops at sessions.session_journal_max and many sessions stop at
// sessions.journal_quota; each stop is noted once, the spool keeps the
// rest, `devc ps` and `devc doctor` show it, and nothing in a journal is
// lost. A session below its budget is ingested whole. `devc sessions
// prune` needs --before, removes only ended sessions, and ingestion goes
// on afterwards; raising the session budget lets the large session go on.
func TestJournalBudgets(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	ref := itest.RealmA + "/jbudget"
	conf := "harnesses = [\"fake\"]\n[sessions]\nsession_journal_max = \"%s\"\njournal_quota = \"160k\"\n"
	project(e, ref, fmt.Sprintf(conf, "32k"))
	e.MustRun("up", ref)
	jdir := filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "journal", "jbudget")
	jfile := func(sid string) string { return filepath.Join(jdir, sid+".jsonl") }
	sid := func(i int) string { return fmt.Sprintf("s0000000000%010d", i) }
	// Two live sessions of the fake harness; the box appends to their
	// spool files as any process in it can.
	detached := func() string {
		t.Helper()
		id := strings.TrimSpace(e.MustRun("fake", ref, "--detach").Stdout)
		if !strings.HasPrefix(id, "s") || len(id) != 21 {
			t.Fatalf("fake --detach printed %q", id)
		}
		return id
	}
	big, small := detached(), detached()
	boxSh(t, e, ref, spoolMessages(big, 300, 250, false)+" && "+spoolMessages(small, 20, 200, false))
	e.MustRun("ingest", ref)
	js, before := readJournal(t, jfile(big))
	if len(before) > 32<<10+1024 || boxMessages(js) < 50 {
		t.Errorf("the large session's journal: %d bytes, %d messages", len(before), boxMessages(js))
	}
	if last := js[len(js)-1]; last.Type != "note" || last.Spool != nil || !strings.Contains(string(last.Data), "sessions.session_journal_max") {
		t.Errorf("the journal does not end with the budget note: %+v", last)
	}
	if sj, _ := readJournal(t, jfile(small)); boxMessages(sj) != 20 {
		t.Errorf("the small session has %d of 20 messages", boxMessages(sj))
	}
	// Ingesting again changes nothing and writes no second note.
	e.MustRun("ingest", ref)
	if _, again := readJournal(t, jfile(big)); string(again) != string(before) {
		t.Error("a stopped session's journal changed")
	}
	if res := e.MustRun("ps", ref); !strings.Contains(res.Stderr, "session "+big) || !strings.Contains(res.Stderr, "sessions.session_journal_max") {
		t.Errorf("ps does not show the stopped session: %q", res.Stderr)
	}

	// Many sessions instead of one large one. They exist only in the
	// spool, so reconciliation ends them (lost) once they are ingested.
	var script []string
	for i := 20; i < 40; i++ {
		script = append(script, spoolMessages(sid(i), 20, 200, false))
	}
	boxSh(t, e, ref, strings.Join(script, " && "))
	e.MustRun("ingest", ref)
	var total int64
	journals := 0
	filepath.WalkDir(jdir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			fi, _ := d.Info()
			total += (fi.Size()+4095)/4096*4096 + 4096
			journals++
		}
		return nil
	})
	if total > 160<<10+16<<10 || journals >= 22 {
		t.Errorf("the project's journals count %d bytes in %d journals with a budget of 160 KiB", total, journals)
	}
	withoutJournal := 0
	for i := 20; i < 40; i++ {
		if _, err := os.Stat(jfile(sid(i))); err != nil {
			withoutJournal++
		}
	}
	if withoutJournal == 0 {
		t.Error("every session got a journal")
	}
	if res := e.MustRun("ps", ref); !strings.Contains(res.Stderr, "sessions.journal_quota") || !strings.Contains(res.Stderr, "devc sessions prune") {
		t.Errorf("ps does not show the project's budget: %q", res.Stderr)
	}
	var checks []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Hint   string `json:"hint"`
	}
	res := e.Run("doctor", "--json")
	if err := json.Unmarshal([]byte(res.Stdout), &checks); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, res.Stdout)
	}
	found := false
	for _, c := range checks {
		if c.ID == "journals" {
			found = true
			if c.Status != "warn" || !strings.Contains(c.Hint, "sessions.journal_quota") {
				t.Errorf("doctor's journals check: %+v", c)
			}
		}
	}
	if !found {
		t.Error("doctor has no journals check")
	}

	// Prune: only with --before, and only sessions that ended.
	if res := e.Run("sessions", "prune", ref); res.ExitCode != 2 {
		t.Errorf("prune without --before: exit %d", res.ExitCode)
	}
	res = e.MustRun("sessions", "prune", ref, "--before", time.Now().Add(24*time.Hour).Format("2006-01-02"))
	if !strings.Contains(res.Stdout, fmt.Sprintf("removed the journals of %d session(s)", 20-withoutJournal)) || !strings.Contains(res.Stdout, "kept 2 live") {
		t.Errorf("prune: %q", res.Stdout)
	}
	for _, id := range []string{big, small} {
		if _, err := os.Stat(jfile(id)); err != nil {
			t.Errorf("a live session's journal was removed: %v", err)
		}
	}
	// Ingestion goes on with the room the prune made: sessions that had
	// no journal get theirs.
	e.MustRun("ingest", ref)
	now := 0
	for i := 20; i < 40; i++ {
		if _, err := os.Stat(jfile(sid(i))); err == nil {
			now++
		}
	}
	if now == 0 {
		t.Error("ingestion did not go on after the prune")
	}

	// The session budget is raised: the large session goes on, and what
	// its journal held is unchanged.
	project(e, ref, strings.Replace(fmt.Sprintf(conf, "1m"), "160k", "16m", 1))
	e.MustRun("ingest", ref)
	js, after := readJournal(t, jfile(big))
	if !strings.HasPrefix(string(after), string(before)) || boxMessages(js) != 300 {
		t.Errorf("after raising the budget: %d messages, prefix kept %v", boxMessages(js), strings.HasPrefix(string(after), string(before)))
	}
}

// boxMessages counts the message events spoolMessages wrote.
func boxMessages(js []jline) int {
	n := 0
	for _, j := range js {
		if j.Type == "message" && strings.Contains(string(j.Data), "mmmm") {
			n++
		}
	}
	return n
}

// TestBankBudget: a bank takes from a box only what fits (C20a, X7 1 and
// X7 2). A box with more changed branches than a bank takes is refused
// before anything is fetched, and banked once it has fewer. A commit that
// would take the relay over git.relay_quota ends the bank's fetch: no
// temporary pack or lock is left, earlier bank refs stay, the relay
// passes fsck, and with a larger budget the next bank banks it.
func TestBankBudget(t *testing.T) {
	e := gitSetup(t)
	e.Timeout = 5 * time.Minute
	up, _ := upstream(t)
	ref := itest.RealmA + "/bankbudget"
	e.MustRun("project", "add", ref, "--from", "file://"+up, "--harness", "fake")
	appendConfig(t, e, projectFile(ref), "[git]\nrelay_quota = \"8m\"")
	e.MustRun("up", ref)
	relay := relayDir(e, ref)
	main := inBox(ref) + "/main"
	boxSh(t, e, ref, "cd "+main+" && echo one > one && git add one && git commit -qm one")
	e.MustRun("bank", ref)
	banked := relayRefs(t, e, ref)
	if len(withPrefix(banked, "refs/devc/bank/main/")) == 0 {
		t.Fatalf("no bank ref: %v", banked)
	}

	// More changed branches than a bank takes: refused, nothing fetched.
	boxSh(t, e, ref, "cd "+main+" && tip=$(git rev-parse HEAD) && for i in $(seq 1 1001); do echo \"create refs/heads/many$i $tip\"; done | git update-ref --stdin")
	res := e.Run("bank", ref)
	if res.ExitCode == 0 || !strings.Contains(res.Stdout+res.Stderr, "1001 changed branches, more than the 1000 a bank takes") {
		t.Errorf("a bank of 1001 changed branches: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := relayRefs(t, e, ref); len(got) != len(banked) {
		t.Errorf("a refused bank changed the relay: %d refs, %d before", len(got), len(banked))
	}
	boxSh(t, e, ref, "cd "+main+" && git for-each-ref --format='delete %(refname)' 'refs/heads/many*' | git update-ref --stdin")
	e.MustRun("bank", ref)
	banked = relayRefs(t, e, ref)

	// A commit of 40 MiB that does not compress, with a budget of 8 MiB.
	boxSh(t, e, ref, "cd "+main+" && head -c 41943040 /dev/urandom > big && git add big && git commit -qm big")
	res = e.Run("bank", ref)
	if res.ExitCode == 0 || !strings.Contains(res.Stdout+res.Stderr, "git.relay_quota") {
		t.Fatalf("a bank beyond the relay's budget: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var left []string
	filepath.WalkDir(relay, func(p string, d fs.DirEntry, err error) error {
		if err == nil && (strings.HasPrefix(d.Name(), "tmp_") || strings.HasSuffix(d.Name(), ".lock")) {
			left = append(left, p)
		}
		return nil
	})
	if len(left) > 0 {
		t.Errorf("left in the relay: %q", left)
	}
	if got := relayRefs(t, e, ref); fmt.Sprint(got) != fmt.Sprint(banked) {
		t.Errorf("an ended bank changed the relay's refs:\n%v\n%v", banked, got)
	}
	hostGit(t, "", "--git-dir="+relay, "fsck", "--no-progress")
	var size int64
	filepath.WalkDir(relay, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			fi, _ := d.Info()
			size += fi.Size()
		}
		return nil
	})
	if size > 16<<20 {
		t.Errorf("the relay holds %d bytes after the ended bank (budget 8 MiB)", size)
	}

	// The control: with a larger budget the next bank banks it.
	project(e, ref, strings.Replace(readConfig(t, e, projectFile(ref)), "relay_quota = \"8m\"", "relay_quota = \"1g\"", 1))
	e.MustRun("bank", ref)
	tip := boxSh(t, e, ref, "cd "+main+" && git rev-parse HEAD")
	if got := relayRefs(t, e, ref); got["refs/heads/main"] != tip || !valuesWithPrefix(got, "refs/devc/bank/main/")[tip] {
		t.Errorf("the next bank did not bank the commit: %v", got)
	}
}

// readConfig reads a file below the configuration directory.
func readConfig(t *testing.T, e *itest.Env, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.ConfigDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRequestedSavepoints: savepoints a box requests are a class of their
// own (C20a, X9 1). A request inside requested_interval is refused with
// the reason; only the newest requested_keep requested savepoints stay,
// whatever name the box gave; the owner's savepoints, named and unnamed,
// are never dropped by them, and the owner's retention does not count
// them.
func TestRequestedSavepoints(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	e.Timeout = 5 * time.Minute
	ref := itest.RealmA + "/reqsave"
	project(e, ref, "harnesses = [\"fake\"]\n[data]\nwork = [\"scratch\"]\n[data.savepoints]\ndriver = \"restic\"\nkeep = 2\nrequested_keep = 2\nrequested_interval = \"15s\"\n[requests]\nauto = [\"savepoint\"]\n")
	e.MustRun("toolbox", "install", "restic")
	e.MustRun("up", ref)
	d := itest.StartDaemon(t, e, "--housekeeping-interval", "1h")
	waitBox(t, e, ref, proto.ChannelConnected, "")
	type sp struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Class string `json:"class"`
	}
	list := func() []sp {
		t.Helper()
		var l struct {
			Savepoints []sp `json:"savepoints"`
		}
		if err := json.Unmarshal([]byte(e.MustRun("data", "ls", ref, "--json").Stdout), &l); err != nil {
			t.Fatal(err)
		}
		return l.Savepoints
	}
	e.MustRun("save", "mine", ref)
	e.MustRun("save", ref)
	owner := list()
	if len(owner) != 2 {
		t.Fatalf("the owner's savepoints: %+v", owner)
	}
	request := func(args ...string) string {
		t.Helper()
		res := e.Run(append([]string{"run", ref, "--", "devc", "request", "savepoint"}, args...)...)
		if res.ExitCode != 0 {
			t.Fatalf("request savepoint: %s%s\ndaemon:\n%s", res.Stdout, res.Stderr, d.Log())
		}
		id := requestID(t, res.Stdout)
		if res := e.Run("run", ref, "--", "devc", "request", "status", id, "--wait"); res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, id+" done") {
			t.Fatalf("status of %s: exit %d %q %q\ndaemon:\n%s", id, res.ExitCode, res.Stdout, res.Stderr, d.Log())
		}
		return id
	}
	request()
	// Inside the interval (counted from the start of the last requested
	// savepoint): refused with the reason.
	if res := e.Run("run", ref, "--", "devc", "request", "savepoint"); res.ExitCode == 0 || !strings.Contains(res.Stdout+res.Stderr, "requested_interval") {
		t.Errorf("a request inside the interval: exit %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}
	for _, name := range []string{"keepme", "", ""} {
		time.Sleep(15 * time.Second)
		if name != "" {
			request(name)
		} else {
			request()
		}
	}
	got := list()
	var requested, kept []string
	for _, s := range got {
		if s.Class == "requested" {
			requested = append(requested, s.Name)
		} else {
			kept = append(kept, s.ID)
		}
	}
	if len(requested) != 2 || strings.Join(requested, ",") != "," {
		t.Errorf("requested savepoints left: %q (the named one is the third newest and goes)", requested)
	}
	if strings.Join(kept, ",") != owner[0].ID+","+owner[1].ID {
		t.Errorf("the owner's savepoints changed: %q, want %+v", kept, owner)
	}
	// The owner's retention (keep = 2) does not count requested ones: a
	// second unnamed savepoint of the owner keeps the first (counting the
	// two unnamed requested ones, it would drop it); a third drops it.
	e.MustRun("save", ref)
	got = list()
	if len(got) != 5 || got[1].ID != owner[1].ID {
		t.Errorf("after the owner's second unnamed save: %+v", got)
	}
	time.Sleep(time.Second)
	e.MustRun("save", ref)
	got = list()
	n := 0
	for _, s := range got {
		if s.Class == "requested" {
			n++
		}
		if s.ID == owner[1].ID {
			t.Errorf("the owner's oldest unnamed savepoint stayed beside two newer ones: %+v", got)
		}
	}
	if n != 2 || len(got) != 5 {
		t.Errorf("after the owner's third unnamed save: %+v", got)
	}
}
