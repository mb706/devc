//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// fleetHub is one hub of the fleet test.
type fleetHub struct {
	dir, url, ca string
	h            *itest.Hub
}

func startFleetHub(t *testing.T, e *itest.Env, name string) *fleetHub {
	t.Helper()
	port := freePort(t)
	dir := itest.InitHub(t, e, name, "127.0.0.1:"+strconv.Itoa(port))
	setHubPassword(t, e, dir)
	return &fleetHub{dir: dir, url: "https://127.0.0.1:" + strconv.Itoa(port), ca: hub.Layout{Dir: dir}.CACert(),
		h: itest.StartHub(t, e, dir, "--listen", "127.0.0.1:"+strconv.Itoa(port))}
}

// stored reads a file of a machine's archive on the hub ("" when missing).
func (f *fleetHub) stored(machine, realm, p string) string {
	b, err := os.ReadFile(filepath.Join(f.dir, "archive", machine, realm, filepath.FromSlash(p)))
	if err != nil {
		return ""
	}
	return string(b)
}

// grep reports whether any file below dir contains s.
func grepTree(t *testing.T, dir, s string) []string {
	t.Helper()
	var hits []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(s)) {
				hits = append(hits, p)
			}
		}
		return nil
	})
	return hits
}

func replaceConfigLine(t *testing.T, e *itest.Env, rel, old, new string) {
	t.Helper()
	p := filepath.Join(e.ConfigDir, rel)
	b, err := os.ReadFile(p)
	if err != nil || !bytes.Contains(b, []byte(old)) {
		t.Fatalf("%s has no line %q", rel, old)
	}
	os.WriteFile(p, bytes.Replace(b, []byte(old), []byte(new), 1), 0o644)
}

func archiveStatus(t *testing.T, e *itest.Env) proto.ArchiveStatusResult {
	t.Helper()
	var res proto.ArchiveStatusResult
	out := e.MustRun("sync", "status", "--json").Stdout
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("sync status: %v\n%s", err, out)
	}
	return res
}

func sha256Of(b string) string {
	s := sha256.Sum256([]byte(b))
	return hex.EncodeToString(s[:])
}

// TestFleet covers work order C17 section 6, integration items 1 to 7: two
// hubs, machine A with a daemon and a box, machine B and C restoring.
func TestFleet(t *testing.T) {
	e := itest.NewNoDaemon(t)
	e.Timeout = 5 * time.Minute
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	realm := itest.RealmA
	ref := realm + "/fleet"
	up, _ := upstream(t)
	e.MustRun("project", "add", ref, "--from", "file://"+up, "--harness", "fake")
	// Slow enough to interrupt an upload; files of shared harness state
	// above 1 MiB are left out.
	replaceConfigLine(t, e, "config.toml", `upload_rate = "4m"`, `upload_rate = "128k"`)
	replaceConfigLine(t, e, "realms/"+realm+"/realm.toml", `max_file_size = "256m"`, `max_file_size = "1m"`)

	mA, mB := "ma"+itest.Nonce, "mb"+itest.Nonce
	A := startFleetHub(t, e, "hubA")
	B := startFleetHub(t, e, "hubB")
	certA, keyA := issueCert(t, e, A.dir, "machine", mA)
	certAB, keyAB := issueCert(t, e, B.dir, "machine", mA)
	e.MustRun("join", A.url, "--cert", certA, "--key", keyA, "--ca", A.ca)
	e.MustRun("join", B.url, "--cert", certAB, "--key", keyAB, "--ca", B.ca)
	daemonArgs := []string{"--archive-interval", "1s", "--archive-scan", "2s"}
	d := itest.StartDaemon(t, e, daemonArgs...)
	waitBox(t, e, ref, proto.ChannelConnected, "")

	durable := filepath.Join(e.StateDir, "durable", "realms", realm)
	jrel := func(sid string) string { return "journal/fleet/" + sid + ".jsonl" }
	local := func(rel string) string {
		b, _ := os.ReadFile(filepath.Join(durable, rel))
		return string(b)
	}
	inBoth := func(what, rel string) {
		t.Helper()
		hubWait(t, 90*time.Second, what+" byte for byte on both hubs", func() bool {
			want := local(rel)
			return want != "" && A.stored(mA, realm, archive.FilePath(rel, 0)) == want && B.stored(mA, realm, archive.FilePath(rel, 0)) == want
		})
	}

	// 1. Journal lines and a transcript written during a session reach both
	// hubs byte for byte.
	res := e.MustRun("fake", ref, "--detach")
	sid := strings.TrimSpace(res.Stdout)
	waitState(t, e, sid, "idle")
	transcript := "harness/fake/state/transcript.jsonl"
	e.MustRun("send", sid, `sh echo '{"turn":1}' >> "$FAKEAGENT_DIR/state/transcript.jsonl"`, "--wait")
	hubWait(t, 30*time.Second, "the transcript written", func() bool { return local(transcript) != "" })
	waitState(t, e, sid, "done")
	inBoth("the journal", jrel(sid))
	inBoth("the transcript", transcript)

	// An interrupted upload resumes without duplication: a large transcript
	// at 128 KiB/s, the daemon killed while hub A holds part of it.
	big := strings.Repeat(`{"line":"0123456789abcdefghijklmnopqrstuvwxyz"}`+"\n", 900<<10/48)
	bigRel := "harness/fake/state/big-transcript.jsonl"
	if err := os.WriteFile(filepath.Join(durable, bigRel), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	hubWait(t, 60*time.Second, "part of the large transcript on hub A", func() bool {
		n := len(A.stored(mA, realm, archive.FilePath(bigRel, 0)))
		return n > 0 && n < len(big)
	})
	d.Kill()
	part := A.stored(mA, realm, archive.FilePath(bigRel, 0))
	if !strings.HasPrefix(big, part) {
		t.Fatalf("hub A holds %d bytes that are not the file's beginning", len(part))
	}
	d = itest.StartDaemon(t, e, daemonArgs...)
	inBoth("the large transcript after the restart", bigRel)
	if got := A.stored(mA, realm, archive.FilePath(bigRel, 1)); got != "" {
		t.Error("the resumed upload started a new generation")
	}

	// 3. Hostile shared harness state: a link to a host file, a FIFO, a file
	// above the limit. Nothing of them reaches a hub.
	secret := "HOSTSECRET-" + itest.Nonce
	hostFile := filepath.Join(t.TempDir(), "host.txt")
	os.WriteFile(hostFile, []byte(secret+"\n"), 0o600)
	state := filepath.Join(durable, "harness", "fake", "state")
	if err := os.Symlink(hostFile, filepath.Join(state, "evil.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(state, "fifo.jsonl"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(state, "huge.jsonl"), bytes.Repeat([]byte(secret[:4]), (2<<20)/4), 0o644)
	os.WriteFile(filepath.Join(state, "after.jsonl"), []byte("after\n"), 0o644)
	inBoth("a file written after the hostile ones", "harness/fake/state/after.jsonl")
	hubWait(t, 30*time.Second, "the hostile entries counted", func() bool {
		for _, h := range archiveStatus(t, e).Hubs {
			for _, r := range h.Realms {
				if r.Realm == realm && r.Skipped >= 3 {
					return true
				}
			}
		}
		return false
	})
	for _, h := range []*fleetHub{A, B} {
		if hits := grepTree(t, filepath.Join(h.dir, "archive"), secret); len(hits) > 0 {
			t.Errorf("host file content reached a hub: %v", hits)
		}
		for _, n := range []string{"evil.jsonl", "fifo.jsonl", "huge.jsonl"} {
			if h.stored(mA, realm, archive.FilePath("harness/fake/state/"+n, 0)) != "" {
				t.Errorf("%s reached %s", n, h.url)
			}
		}
	}
	if st := e.MustRun("sync", "status").Stdout; !strings.Contains(st, "evil.jsonl") {
		t.Errorf("sync status does not name a skipped entry:\n%s", st)
	}
	os.Remove(filepath.Join(state, "huge.jsonl"))

	// 2. A commit banked on machine A, the context repository and the
	// journal are recoverable on machine B.
	commit := boxSh(t, e, ref, "cd "+inBox(ref)+"/main && echo banked >> a && git commit -qam banked && git rev-parse HEAD")
	e.MustRun("bank", ref)
	notes := filepath.Join(durable, "context", "notes", "fleet")
	os.MkdirAll(notes, 0o755)
	os.WriteFile(filepath.Join(notes, "plan.md"), []byte("the plan\n"), 0o644)
	e.MustRun("context", "commit", realm)
	hubWait(t, 90*time.Second, "the commit in hub A's ref log", func() bool {
		return strings.Contains(A.stored(mA, realm, archive.RefsPath("relay/fleet")), commit)
	})
	hubWait(t, 90*time.Second, "the context repository on hub A", func() bool {
		return strings.Contains(A.stored(mA, realm, archive.RefsPath(archive.RepoContext)), "refs/heads/main")
	})
	inBoth("the journal", jrel(sid))

	eB := itest.NewNoDaemon(t)
	eB.Timeout = 5 * time.Minute
	eB.InstallFakeManifests()
	certB, keyB := issueCert(t, e, A.dir, "machine", mB)
	eB.MustRun("join", A.url, "--cert", certB, "--key", keyB, "--ca", A.ca)
	if out := eB.MustRun("state", "pull", realm, "--from", A.url, "--list").Stdout; !strings.Contains(out, mA) {
		t.Errorf("state pull --list:\n%s", out)
	}
	res = eB.Run("state", "pull", realm, "--from", A.url, "--machine", mA)
	if res.ExitCode != 0 {
		t.Fatalf("state pull: %s%s", res.Stdout, res.Stderr)
	}
	t.Logf("state pull:\n%s%s", res.Stdout, res.Stderr)
	if got := relayRefs(t, eB, ref)["refs/heads/main"]; got != commit {
		t.Errorf("machine B's relay main is %q, want %s", got, commit)
	}
	durableB := filepath.Join(eB.StateDir, "durable", "realms", realm)
	for _, rel := range []string{jrel(sid), transcript, bigRel} {
		b, _ := os.ReadFile(filepath.Join(durableB, rel))
		if want := A.stored(mA, realm, archive.FilePath(rel, 0)); string(b) != want || want == "" {
			t.Errorf("machine B's %s differs from the archive", rel)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(durableB, "context", "notes", "fleet", "plan.md")); string(b) != "the plan\n" {
		t.Errorf("machine B's context note: %q", b)
	}
	if log := eB.MustRun("context", "log", realm).Stdout; !strings.Contains(log, "plan.md") {
		t.Errorf("machine B's context history:\n%s", log)
	}
	if res := eB.Run("state", "pull", realm, "--from", A.url, "--machine", mA); res.ExitCode == 0 || !strings.Contains(res.Stderr, "--merge") {
		t.Errorf("a second pull without --merge: %d %s", res.ExitCode, res.Stderr)
	}
	eB.MustRun("state", "pull", realm, "--from", A.url, "--machine", mA, "--merge")

	// 4. A machine cannot overwrite or delete archived data.
	cl := tlsClient(t, A.ca, certA, keyA)
	jpath := archive.FilePath(jrel(sid), 0)
	before := sha256Of(A.stored(mA, realm, jpath))
	attempt := func(method, path string, body string) int {
		req, _ := http.NewRequest(method, A.url+path, strings.NewReader(body))
		req.Header.Set(hub.RequestHeader, "1")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, a := range []struct{ method, path string }{
		{"PUT", archive.UploadPrefix + realm + "/" + jpath + "?offset=0"},
		{"PUT", archive.UploadPrefix + realm + "/" + jpath + "?offset=1"},
		{"PUT", archive.UploadPrefix + realm + "/files/journal/fleet/..%2F..%2F..%2F..%2F" + mB + "%2Fx.jsonl?offset=0"},
		{"PUT", archive.UploadPrefix + realm + "/files/../../" + jpath + "?offset=0"},
		{"PUT", archive.UploadPrefix + realm + "/bundles/relay/fleet/00000000-0000000000000000.bundle?offset=0"},
		{"DELETE", archive.UploadPrefix + realm + "/" + jpath},
		{"POST", archive.UploadPrefix + realm + "/" + jpath + "?offset=0"},
	} {
		if code := attempt(a.method, a.path, "OVERWRITTEN"); code == 200 {
			t.Errorf("%s %s succeeded", a.method, a.path)
		}
	}
	if after := sha256Of(A.stored(mA, realm, jpath)); after != before {
		t.Error("the archived journal changed")
	}
	if hits := grepTree(t, filepath.Join(A.dir, "archive"), "OVERWRITTEN"); len(hits) > 0 {
		t.Errorf("a refused upload left data: %v", hits)
	}

	// 6. Secrets between machines; the hub stores only ciphertext.
	value := "value-" + itest.Nonce + "-secret"
	if res := e.RunIn("/", value+"\n", "secret", "set", realm, "FLEET_TOKEN"); res.ExitCode != 0 {
		t.Fatalf("secret set: %s", res.Stderr)
	}
	if res := e.RunIn("/", "a long passphrase\n", "secret", "push", realm, "--to", A.url); res.ExitCode != 0 {
		t.Fatalf("secret push: %s%s", res.Stdout, res.Stderr)
	}
	if hits := grepTree(t, A.dir, value); len(hits) > 0 {
		t.Errorf("the hub holds the secret in clear: %v", hits)
	}
	if res := eB.RunIn("/", "the wrong passphrase\n", "secret", "pull", realm, "--from", A.url); res.ExitCode == 0 || !strings.Contains(res.Stderr, "passphrase is wrong") {
		t.Errorf("a wrong passphrase: %d %s", res.ExitCode, res.Stderr)
	}
	if res := eB.RunIn("/", "a long passphrase\n", "secret", "pull", realm, "--from", A.url); res.ExitCode != 0 {
		t.Fatalf("secret pull: %s%s", res.Stdout, res.Stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(durableB, "secrets", "FLEET_TOKEN")); string(b) != value {
		t.Errorf("machine B's secret: %q", b)
	}

	// 5. state export, then state import on a fresh state directory.
	tarFile := filepath.Join(t.TempDir(), "realm.tar")
	e.MustRun("state", "export", realm, "--out", tarFile)
	if b, _ := os.ReadFile(tarFile); bytes.Contains(b, []byte(value)) {
		t.Error("the export holds a secret")
	}
	eC := itest.NewNoDaemon(t)
	eC.Timeout = 5 * time.Minute
	eC.InstallFakeManifests()
	if res := eC.Run("state", "import", tarFile); res.ExitCode != 0 {
		t.Fatalf("state import: %s%s", res.Stdout, res.Stderr)
	}
	durableC := filepath.Join(eC.StateDir, "durable", "realms", realm)
	for _, rel := range []string{jrel(sid), transcript, bigRel, "harness/fake/state/after.jsonl"} {
		b, _ := os.ReadFile(filepath.Join(durableC, rel))
		if string(b) != local(rel) {
			t.Errorf("imported %s differs", rel)
		}
	}
	if !sameMaps(relayRefs(t, e, ref), relayRefs(t, eC, ref)) {
		t.Errorf("imported relay refs differ:\n%v\n%v", relayRefs(t, e, ref), relayRefs(t, eC, ref))
	}
	if log := eC.MustRun("context", "log", realm).Stdout; !strings.Contains(log, "plan.md") {
		t.Errorf("imported context history:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(durableC, "secrets")); err == nil {
		t.Error("secrets were imported")
	}
	if res := eC.Run("state", "import", tarFile); res.ExitCode == 0 {
		t.Error("an import into a realm with state succeeded")
	}

	// 7. With hub A stopped, events, the archive and the notifier role go
	// on through hub B.
	devB, devBKey := issueCert(t, e, B.dir, "device", "phone")
	api := (&hubClient{t: t, base: B.url, c: tlsClient(t, B.ca, devB, devBKey)}).mustLogin()
	A.h.Stop()
	hubWait(t, 60*time.Second, "hub B the notifier", func() bool { return api.machines()[mA].Notifier })
	if st := e.MustRun("hub", "status").Stdout; !strings.Contains(st, B.url) || !(strings.Contains(st, "disconnected") || strings.Contains(st, "connecting")) {
		t.Errorf("hub status:\n%s", st)
	}
	e.MustRun("send", sid, "say through hub B", "--wait")
	hubWait(t, 30*time.Second, "the new message on hub B", func() bool {
		det, ok := api.detail(mA, sid)
		if !ok {
			return false
		}
		for _, ev := range det.Events {
			var m proto.Message
			if ev.Type == proto.TypeMessage && json.Unmarshal(ev.Data, &m) == nil && m.Text == "through hub B" {
				return true
			}
		}
		return false
	})
	hubWait(t, 90*time.Second, "the journal on hub B after hub A stopped", func() bool {
		return B.stored(mA, realm, archive.FilePath(jrel(sid), 0)) == local(jrel(sid)) && strings.Contains(local(jrel(sid)), "through hub B")
	})
	res = e.MustRun("sync", "status")
	if !strings.Contains(res.Stdout, A.url) || !strings.Contains(res.Stdout, B.url) {
		t.Errorf("sync status:\n%s", res.Stdout)
	}
	e.MustRun("down", ref)
}

func sameMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
