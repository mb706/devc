package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/requests"
	"github.com/mb706/devc/internal/secrets"
)

func TestRateLimit(t *testing.T) {
	l := newRateLimit(time.Minute)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	other := names.ProjectRef{Realm: "demo", Project: "other"}
	if !l.allow(hello) || l.allow(hello) {
		t.Fatal("the first attempt must pass and a second at once must not")
	}
	if !l.allow(other) {
		t.Error("projects share a limit")
	}
	now = now.Add(59 * time.Second)
	if l.allow(hello) {
		t.Error("allowed again within the interval")
	}
	now = now.Add(2 * time.Second)
	if !l.allow(hello) || l.allow(hello) {
		t.Error("not allowed after the interval, or twice")
	}
	// A clock that went back does not block for ever.
	now = now.Add(-time.Hour)
	if !l.allow(hello) {
		t.Error("a clock change blocked the action")
	}
}

// boxRig is a rig with a connected box channel.
func boxRig(t *testing.T, projectExtra, globalExtra string, logw ...io.Writer) *rig {
	t.Helper()
	r := newRig(t)
	if len(logw) > 0 {
		r.logw = logw[0]
	}
	if projectExtra != "" {
		f := filepath.Join(r.p.ConfigDir(), "realms", "demo", "projects", "hello", "project.toml")
		b, _ := os.ReadFile(f)
		os.WriteFile(f, append(b, []byte("\n"+projectExtra+"\n")...), 0o644)
	}
	if globalExtra != "" {
		f := filepath.Join(r.p.ConfigDir(), "config.toml")
		b, _ := os.ReadFile(f)
		os.WriteFile(f, append(b, []byte("\n"+globalExtra+"\n")...), 0o644)
	}
	r.boxUp(true)
	r.start()
	r.agent(nil)
	r.waitChannel(r.client(nil), proto.ChannelConnected)
	return r
}

func (r *rig) boxCall(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return boxside.AgentCallAt(ctx, r.local, method, params, result)
}

func (r *rig) request(t *testing.T, p proto.RequestParams) (proto.RequestReply, error) {
	t.Helper()
	var reply proto.RequestReply
	err := r.boxCall(proto.MethodHostRequest, p, &reply)
	return reply, err
}

func (r *rig) waitRequest(t *testing.T, id string) proto.RequestStatus {
	t.Helper()
	var st proto.RequestStatus
	if err := r.boxCall(proto.MethodHostRequestStatus, proto.RequestStatusParams{ID: id, Wait: 10}, &st); err != nil {
		t.Fatalf("status of %s: %v", id, err)
	}
	return st
}

func (r *rig) devcCalls(sub string) [][]string {
	var out [][]string
	for _, c := range r.runner.Calls() {
		if c.Name == "/fake/devc" && slices.Contains(c.Args, sub) {
			out = append(out, c.Args)
		}
	}
	return out
}

func TestRequests(t *testing.T) {
	r := boxRig(t, "[requests]\nauto = [\"savepoint\"]\n", "")
	r.d.o.Global = []string{"--config-dir", "/cfg"}
	c := r.client(nil)
	st := &requests.Store{Paths: r.p}
	logSize := func() int64 {
		fi, err := os.Stat(r.p.RequestsFile(hello))
		if err != nil {
			return 0
		}
		return fi.Size()
	}

	// A session with a journal gets request events.
	sid := names.NewSessionID()
	r.spool(sid, spoolLine(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"}))
	deadline := time.Now().Add(10 * time.Second)
	for !fileExists(r.p.JournalFile(hello, sid)) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	// Low risk and listed in auto: approved and carried out at once.
	rep, err := r.request(t, proto.RequestParams{Kind: "savepoint", Name: "before", Session: sid})
	if err != nil || rep.Risk != proto.RiskLow || rep.Status != proto.RequestApproved {
		t.Fatalf("savepoint: %+v %v", rep, err)
	}
	if got := r.waitRequest(t, rep.ID); got.Status != proto.RequestDone || got.Result != "ok" {
		t.Errorf("savepoint outcome: %+v", got)
	}
	if calls := r.devcCalls("save"); len(calls) != 1 || strings.Join(calls[0], " ") != "--config-dir /cfg save --request "+rep.ID+" -- before demo/hello" {
		t.Errorf("save calls: %q", calls)
	}

	// Low risk but not listed: pending until decided; a hub may decide it.
	note, err := r.request(t, proto.RequestParams{Kind: "note", Text: "please look at the plot", Session: sid})
	if err != nil || note.Status != proto.RequestPending {
		t.Fatalf("note: %+v %v", note, err)
	}
	var dec proto.DecideResult
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: note.ID, Approve: true, Origin: proto.OriginHub}, &dec); err != nil ||
		dec.Request.Status != proto.RequestDone || dec.Request.DecidedBy != proto.OriginHub {
		t.Errorf("hub decision on a low-risk request: %+v %v", dec, err)
	}

	// High risk: pending; a hub's decision is refused; the local one runs it.
	pkg, err := r.request(t, proto.RequestParams{Kind: "package", Manager: "apt", Packages: []string{"jq", "curl"}})
	if err != nil || pkg.Status != proto.RequestPending || pkg.Risk != proto.RiskHigh {
		t.Fatalf("package: %+v %v", pkg, err)
	}
	var now proto.RequestStatus
	if err := r.boxCall(proto.MethodHostRequestStatus, proto.RequestStatusParams{ID: pkg.ID}, &now); err != nil || now.Status != proto.RequestPending {
		t.Errorf("status without wait: %+v %v", now, err)
	}
	var list proto.RequestsListResult
	if err := c.Call(context.Background(), proto.MethodRequestsList, nil, &list); err != nil || len(list.Requests) != 1 || list.Requests[0].ID != pkg.ID {
		t.Errorf("pending list: %+v %v", list, err)
	}
	err = c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: pkg.ID, Approve: true, Origin: proto.OriginHub}, &dec)
	if !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "only with the CLI") {
		t.Errorf("hub approval of a high-risk request: %v", err)
	}
	err = c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: pkg.ID, Approve: false, Origin: proto.OriginHub}, &dec)
	if !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("hub denial of a high-risk request: %v", err)
	}
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: pkg.ID, Approve: true, Origin: "local-root"}, &dec); err == nil {
		t.Error("an unknown origin was accepted")
	}
	if len(r.devcCalls("root")) != 0 {
		t.Fatal("a high-risk request ran before the local approval")
	}
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: pkg.ID, Approve: true}, &dec); err != nil || dec.Request.Status != proto.RequestApproved {
		t.Fatalf("local approval: %+v %v", dec, err)
	}
	if got := r.waitRequest(t, pkg.ID); got.Status != proto.RequestDone {
		t.Errorf("package outcome: %+v", got)
	}
	roots := r.devcCalls("root")
	if len(roots) != 2 || strings.Join(roots[0], " ") != "--config-dir /cfg root demo/hello -- env DEBIAN_FRONTEND=noninteractive apt-get update" ||
		strings.Join(roots[1], " ") != "--config-dir /cfg root demo/hello -- env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends jq curl" {
		t.Errorf("root calls: %q", roots)
	}
	for _, call := range r.runner.Calls() {
		if call.Dir != "/" {
			t.Errorf("%s ran in %q", call.String(), call.Dir)
		}
	}
	// A decided request cannot be decided again.
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: pkg.ID, Approve: true}, &dec); !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("second decision: %v", err)
	}

	// Denied with a reason; a failing command marks the request failed.
	deny, _ := r.request(t, proto.RequestParams{Kind: "package", Manager: "apk", Packages: []string{"x"}})
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: deny.ID, Reason: "not needed"}, &dec); err != nil || dec.Request.Status != proto.RequestDenied {
		t.Errorf("deny: %+v %v", dec, err)
	}
	if got := r.waitRequest(t, deny.ID); got.Status != proto.RequestDenied || got.Reason != "not needed" {
		t.Errorf("denied: %+v", got)
	}
	r.runner.On("/fake/devc", "--config-dir", "/cfg", "root").Stderr("E: no such package\n").Exit(100)
	fail, _ := r.request(t, proto.RequestParams{Kind: "package", Manager: "dnf", Packages: []string{"nope"}})
	c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: fail.ID, Approve: true}, &dec)
	if got := r.waitRequest(t, fail.ID); got.Status != proto.RequestFailed || !strings.Contains(got.Result, "no such package") {
		t.Errorf("failed request: %+v", got)
	}
	// A network request runs `devc net allow` with the request as origin
	// (work order C13); kinds that are not available yet answer so.
	netReq, _ := r.request(t, proto.RequestParams{Kind: "net", Target: "example.org:443"})
	c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: netReq.ID, Approve: true}, &dec)
	if got := r.waitRequest(t, netReq.ID); got.Status != proto.RequestDone {
		t.Errorf("net: %+v", got)
	}
	if !r.runner.Called("/fake/devc", "--config-dir", "/cfg", "net", "allow", "--request", netReq.ID, "--for", "1h0m0s", "--", "example.org:443", "demo/hello") {
		t.Errorf("net request ran %v", r.runner.Calls())
	}
	grantReq, _ := r.request(t, proto.RequestParams{Kind: "grant", Grant: "ssh", Name: "c1"})
	c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: grantReq.ID, Approve: true}, &dec)
	if got := r.waitRequest(t, grantReq.ID); got.Status != proto.RequestFailed || !strings.Contains(got.Result, "not available yet") {
		t.Errorf("grant: %+v", got)
	}

	// Hostile requests are refused and stored nowhere.
	before := logSize()
	for _, p := range []proto.RequestParams{
		{Kind: "package", Manager: "apt", Packages: []string{"jq; rm -rf /"}},
		{Kind: "package", Manager: "apt", Packages: []string{"-o=APT::x"}},
		{Kind: "seal", Path: "../../etc"},
		{Kind: "seal", Path: "results/run1"}, // hello has no data zones
		{Kind: "note", Text: strings.Repeat("x", 5000)},
		{Kind: "note", Text: "hi\x1b[2J"},
		{Kind: "rm", Path: "x"},
		{Kind: "note", Text: "x", Session: "../../etc/passwd"},
	} {
		if _, err := r.request(t, p); !proto.IsCode(err, proto.CodeInvalidParams) {
			t.Errorf("hostile %+v: %v", p, err)
		}
	}
	if err := r.boxCall(proto.MethodHostRequest, map[string]any{"kind": "note", "text": "x", "extra": 1}, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("unknown field: %v", err)
	}
	if logSize() != before {
		t.Error("a refused request reached the log")
	}

	// The box sees only its own project's requests.
	other := proto.Request{ID: requests.NewID(), Time: time.Now(), Ref: "demo/analysis", Kind: "note", Risk: "low", Status: proto.RequestPending}
	if err := st.Put(names.ProjectRef{Realm: "demo", Project: "analysis"}, other); err != nil {
		t.Fatal(err)
	}
	if err := r.boxCall(proto.MethodHostRequestStatus, proto.RequestStatusParams{ID: other.ID}, &now); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("another project's request: %v", err)
	}

	// Events in the session's journal.
	evs, _, err := readEvents(r.p.JournalFile(hello, sid), 0, 1000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for _, ev := range evs {
		if ev.Type == proto.TypeRequest {
			statuses = append(statuses, string(ev.Data))
		}
	}
	joined := strings.Join(statuses, "\n")
	if len(statuses) < 4 || !strings.Contains(joined, `"status":"approved"`) || !strings.Contains(joined, `"status":"done"`) || !strings.Contains(joined, "please look at the plot") {
		t.Errorf("request events:\n%s", joined)
	}

	// Expiry after seven days.
	old := proto.Request{ID: requests.NewID(), Time: time.Now().Add(-8 * 24 * time.Hour), Ref: hello.String(), Kind: "note",
		Params: proto.RequestParams{Kind: "note", Text: "old"}, Risk: "low", Status: proto.RequestPending}
	st.Put(hello, old)
	if got := r.waitRequest(t, old.ID); got.Status != proto.RequestExpired {
		t.Errorf("old request: %+v", got)
	}
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: old.ID, Approve: true}, &dec); !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("decision on an expired request: %v", err)
	}
}

func TestRequestFlood(t *testing.T) {
	r := boxRig(t, "[requests]\nauto = []\n", "")
	var last error
	n := 0
	for i := 0; i < maxPerMinute+5; i++ {
		if _, err := r.request(t, proto.RequestParams{Kind: "note", Text: "x"}); err != nil {
			last = err
			break
		}
		n++
	}
	if n != maxPerMinute || !proto.IsCode(last, proto.CodeBusy) {
		t.Errorf("%d requests accepted, then %v", n, last)
	}
}

func TestSecrets(t *testing.T) {
	var logBuf cappedBuffer
	logBuf.max = 1 << 20
	r := boxRig(t, "[grants]\nsecrets = [\"API_TOKEN\", \"UNSET\"]\n", "[secrets]\ndelivery = \"daemon\"\n", &logBuf)
	st := secrets.NewFileStore(r.p)
	st.Set("demo", "API_TOKEN", []byte("s3cret"))
	st.Set("demo", "OTHER", []byte("nope"))
	st.Set("demo", "LATER", []byte("later"))
	var rep proto.SecretReply
	// No record of the box's start: nothing on demand (finding C1-02).
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "API_TOKEN"}, &rep); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "restart it") {
		t.Errorf("without a record: %v", err)
	}
	r.recordStart()
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "API_TOKEN"}, &rep); err != nil || rep.Value != "s3cret" {
		t.Errorf("granted secret: %q %v", rep.Value, err)
	}
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "OTHER"}, &rep); !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("ungranted secret: %v", err)
	}
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "UNSET"}, &rep); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("granted but unset: %v", err)
	}
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "../x"}, &rep); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("hostile name: %v", err)
	}
	// The value never reaches the log.
	if !strings.Contains(logBuf.String(), "secret delivered") || strings.Contains(logBuf.String(), "s3cret") {
		t.Error("the secret's value was logged")
	}
	// A secret granted after the start (say, together with a network
	// change the running box does not have) waits for the next start; a
	// grant removed is gone at once (finding C1-02).
	proj := filepath.Join(r.p.ConfigDir(), "realms", "demo", "projects", "hello", "project.toml")
	pb, _ := os.ReadFile(proj)
	os.WriteFile(proj, []byte(strings.Replace(string(pb), `secrets = ["API_TOKEN", "UNSET"]`, `secrets = ["UNSET", "LATER"]`, 1)), 0o644)
	r.d.reload(true)
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "LATER"}, &rep); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "after its box started") {
		t.Errorf("a secret granted after the start: %v", err)
	}
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "API_TOKEN"}, &rep); !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("a secret no longer granted: %v", err)
	}
	r.recordStart()
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "LATER"}, &rep); err != nil || rep.Value != "later" {
		t.Errorf("after the box was brought up again: %q %v", rep.Value, err)
	}
	// A record of another container counts for nothing.
	caps, _ := box.ReadCapabilities(r.p, hello)
	caps.ContainerID = strings.Repeat("0", 64)
	box.WriteCapabilities(r.p, hello, *caps)
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "LATER"}, &rep); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "another container") {
		t.Errorf("a record of another container: %v", err)
	}
	// With file delivery the daemon serves nothing.
	f := filepath.Join(r.p.ConfigDir(), "config.toml")
	b, _ := os.ReadFile(f)
	os.WriteFile(f, []byte(strings.Replace(string(b), `delivery = "daemon"`, `delivery = "files"`, 1)), 0o644)
	r.d.reload(true)
	if err := r.boxCall(proto.MethodHostSecret, proto.SecretParams{Name: "API_TOKEN"}, &rep); !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("file delivery: %v", err)
	}
}

func TestTurnBanking(t *testing.T) {
	r := newRig(t)
	r.boxUp(true)
	o := r.options()
	o.TurnBankInterval = 400 * time.Millisecond
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	r.d, r.done = d, make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-r.done })
	r.client(nil)
	// The project needs a relay.
	relay := r.p.RelayRepo(hello)
	os.MkdirAll(relay, 0o700)
	os.WriteFile(filepath.Join(relay, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	banks := func() int {
		n := 0
		for _, c := range r.runner.Calls() {
			if c.Name == "/fake/devc" && slices.Equal(c.Args, []string{"bank", "demo/hello", "--quiet"}) {
				n++
			}
		}
		return n
	}
	wait := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for banks() < n && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if banks() < n {
			t.Fatalf("%d banks, want %d: %v", banks(), n, r.runner.Calls())
		}
	}
	sid := names.NewSessionID()
	state := func(s string) string { return spoolLine(sid, proto.TypeState, proto.State{State: s}) }
	r.spool(sid, spoolLine(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"}), state("idle"))
	time.Sleep(300 * time.Millisecond)
	if banks() != 0 {
		t.Fatal("banked without a turn")
	}
	r.spool(sid, state("working"), state("done"))
	wait(1)
	// Two more turns at once: one more bank, after the interval.
	start := time.Now()
	r.spool(sid, state("working"), state("idle"))
	time.Sleep(50 * time.Millisecond)
	r.spool(sid, state("working"), state("done"))
	wait(2)
	time.Sleep(600 * time.Millisecond)
	if banks() != 2 {
		t.Errorf("%d banks after three turns", banks())
	}
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Errorf("the second bank came after %s", el)
	}
	// working -> blocked is no end of a turn.
	r.spool(sid, state("working"), state("blocked"))
	time.Sleep(700 * time.Millisecond)
	if banks() != 2 {
		t.Errorf("banked on blocked: %d", banks())
	}
}

// gitRunner runs git for real and everything else through the fake.
type gitRunner struct {
	*execx.Fake
	real execx.Real
}

func (g gitRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	if c.Name == "git" {
		return g.real.Run(ctx, c)
	}
	return g.Fake.Run(ctx, c)
}

func (g gitRunner) Interactive(ctx context.Context, c execx.Cmd) (int, error) {
	if c.Name == "git" {
		return g.real.Interactive(ctx, c)
	}
	return g.Fake.Interactive(ctx, c)
}

// readAdvertisement reads pkt-lines up to the first flush packet.
func readAdvertisement(t *testing.T, r io.Reader) []string {
	t.Helper()
	var lines []string
	for {
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(r, hdr); err != nil {
			t.Fatalf("reading the advertisement: %v (so far %q)", err, lines)
		}
		n, err := strconv.ParseUint(string(hdr), 16, 16)
		if err != nil {
			t.Fatalf("bad pkt-line header %q", hdr)
		}
		if n == 0 {
			return lines
		}
		buf := make([]byte, n-4)
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(buf))
	}
}

func TestRelayStreams(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r := newRig(t)
	runner := gitRunner{Fake: r.runner}
	r.boxUp(true)
	o := r.options()
	o.Runner = runner
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	r.d, r.done = d, make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-r.done })
	r.agent(nil)
	r.waitChannel(r.client(nil), proto.ChannelConnected)
	dial := boxside.RelayDialerAt(r.local)
	if !dial.Available() {
		t.Fatal("the relay is not available through the agent")
	}
	// No relay yet: refused with a message.
	if _, err := dial.Open("upload-pack", ""); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "no relay yet") {
		t.Fatalf("without a relay: %v", err)
	}
	relay := r.p.RelayRepo(hello)
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %q: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.MkdirAll(filepath.Dir(relay), 0o700)
	git("init", "-q", "--bare", "-b", "main", relay)
	tree := git("--git-dir="+relay, "hash-object", "-t", "tree", "-w", "/dev/null")
	commit := git("--git-dir="+relay, "commit-tree", "-m", "c", tree)
	git("--git-dir="+relay, "update-ref", "refs/heads/main", commit)
	git("--git-dir="+relay, "update-ref", "refs/devc/bank/main/1", commit)
	git("--git-dir="+relay, "update-ref", "refs/remotes/github/main", commit)

	// upload-pack: devc's refs are hidden; a flush ends it.
	conn, err := dial.Open("upload-pack", "")
	if err != nil {
		t.Fatal(err)
	}
	adv := strings.Join(readAdvertisement(t, conn), "\n")
	if !strings.Contains(adv, commit+" refs/heads/main") || !strings.Contains(adv, "refs/remotes/github/main") || strings.Contains(adv, "refs/devc") {
		t.Errorf("upload-pack advertisement:\n%s", adv)
	}
	conn.Write([]byte("0000"))
	conn.CloseWrite()
	if rest, _ := io.ReadAll(conn); len(rest) != 0 {
		t.Errorf("after the flush: %q", rest)
	}
	conn.Close()

	// receive-pack: hooks are set up, refs/remotes is hidden too.
	conn, err = dial.Open("receive-pack", "")
	if err != nil {
		t.Fatal(err)
	}
	adv = strings.Join(readAdvertisement(t, conn), "\n")
	if !strings.Contains(adv, "refs/heads/main") || strings.Contains(adv, "refs/remotes") || strings.Contains(adv, "refs/devc") {
		t.Errorf("receive-pack advertisement:\n%s", adv)
	}
	conn.Write([]byte("0000"))
	conn.CloseWrite()
	io.ReadAll(conn)
	conn.Close()
	for _, h := range []string{"update", "post-receive"} {
		if target, err := os.Readlink(filepath.Join(r.p.RelayHooksDir(), h)); err != nil || target != "/fake/devc" {
			t.Errorf("hook %s -> %q %v", h, target, err)
		}
	}
	// The box lock is free again: it can be taken (finding C1-20).
	lockFree := func(within time.Duration) bool {
		t.Helper()
		unlock, err := box.LockBox(context.Background(), r.p, hello, within, nil)
		if err != nil {
			return false
		}
		unlock()
		return true
	}
	if !lockFree(2 * time.Second) {
		t.Error("the box lock is still held after receive-pack")
	}
	// Garbage on a stream and unknown services do not disturb the channel.
	for _, bad := range []string{"not json\n", `{"jsonrpc":"2.0","id":1,"method":"host.git","params":{"service":"upload-archive"}}` + "\n",
		`{"jsonrpc":"2.0","id":1,"method":"host.git","params":{"service":"upload-pack","session":"../x"}}` + "\n"} {
		c, err := net.Dial("unix", r.local)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(bad))
		b := make([]byte, 4096)
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		n, _ := c.Read(b)
		c.Close()
		if !strings.Contains(string(b[:n]), `"error"`) {
			t.Errorf("answer to %q: %q", bad, b[:n])
		}
	}
	conn, err = dial.Open("upload-pack", "")
	if err != nil {
		t.Fatalf("after garbage: %v", err)
	}
	readAdvertisement(t, conn)
	conn.Close()

	// Too little free space on the state file system: refused before git
	// starts (finding D1-05).
	d.freeSpace = func(string) (int64, error) { return pushMinFree - 1, nil }
	if _, err := dial.Open("receive-pack", ""); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "free") {
		t.Errorf("a push without the free-space reserve: %v", err)
	}
	d.freeSpace = func(string) (int64, error) { return 0, errors.New("statfs failed") }
	if _, err := dial.Open("receive-pack", ""); err == nil {
		t.Error("a push whose free space could not be checked")
	}
	d.freeSpace = nil
	if !lockFree(2 * time.Second) {
		t.Error("the box lock is still held after a refused push")
	}

	// A box that asks receive-pack for a large advertisement and never
	// reads it loses the stream after the stall limit, and the box lock is
	// released (finding C1-09).
	var refs strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&refs, "%s refs/heads/many/branch-with-a-long-name-%05d\n", commit, i)
	}
	if err := os.WriteFile(filepath.Join(relay, "packed-refs"), []byte(refs.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	old := streamStall
	streamStall = time.Second
	t.Cleanup(func() { streamStall = old })
	conn, err = dial.Open("receive-pack", "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(500 * time.Millisecond)
	if lockFree(100 * time.Millisecond) {
		t.Fatal("the box lock is not held during receive-pack")
	}
	if !lockFree(15 * time.Second) {
		t.Error("a box that stopped reading kept the box lock")
	}
}

func TestRequestCommands(t *testing.T) {
	steps, _ := requestCommands(hello, proto.Request{Kind: "seal", Params: proto.RequestParams{Path: "--help"}})
	if len(steps) != 1 || strings.Join(steps[0], " ") != "seal -- --help demo/hello" {
		t.Errorf("seal: %q", steps)
	}
	steps, _ = requestCommands(hello, proto.Request{ID: "r0123456789abcdefghij", Kind: "savepoint"})
	if strings.Join(steps[0], " ") != "save --request r0123456789abcdefghij -- demo/hello" {
		t.Errorf("savepoint: %q", steps)
	}
	steps, _ = requestCommands(hello, proto.Request{ID: "r0123456789abcdefghij", Kind: "savepoint", Params: proto.RequestParams{Name: "--help"}})
	if strings.Join(steps[0], " ") != "save --request r0123456789abcdefghij -- --help demo/hello" {
		t.Errorf("named savepoint: %q", steps)
	}
	for _, m := range []string{"dnf", "apk"} {
		steps, _ = requestCommands(hello, proto.Request{Kind: "package", Params: proto.RequestParams{Manager: m, Packages: []string{"a", "b"}}})
		if len(steps) != 1 || !slices.Equal(steps[0][:3], []string{"root", "demo/hello", "--"}) || !slices.Equal(steps[0][len(steps[0])-2:], []string{"a", "b"}) {
			t.Errorf("%s: %q", m, steps)
		}
	}
	if steps, msg := requestCommands(hello, proto.Request{Kind: "grant"}); steps != nil || !strings.Contains(msg, "not available yet") {
		t.Errorf("grant: %q %q", steps, msg)
	}
	if steps, msg := requestCommands(hello, proto.Request{Kind: "pr", Params: proto.RequestParams{Title: "t"}}); steps != nil || !strings.Contains(msg, "no branch") {
		t.Errorf("pr without a branch: %q %q", steps, msg)
	}
	steps, _ = requestCommands(hello, proto.Request{Kind: "pr", Params: proto.RequestParams{Title: "--yes", Body: "-x\ny", Branch: "feat/a", Base: "dev", Draft: true}})
	if len(steps) != 1 || strings.Join(steps[0], "|") != "pr|create|--yes|--branch=feat/a|--title=--yes|--body=-x\ny|--base=dev|--draft|--|demo/hello" {
		t.Errorf("pr: %q", steps)
	}
	steps, _ = requestCommands(hello, proto.Request{Kind: "comment", Params: proto.RequestParams{Number: 12, Body: "--help"}})
	if len(steps) != 1 || strings.Join(steps[0], "|") != "pr|comment|--body=--help|--|demo/hello|12" {
		t.Errorf("comment: %q", steps)
	}
	steps, _ = requestCommands(hello, proto.Request{ID: "r0123456789abcdefghij", Kind: "net", Params: proto.RequestParams{Target: "host:5432", For: "30m0s"}})
	if len(steps) != 1 || strings.Join(steps[0], " ") != "net allow --request r0123456789abcdefghij --for 30m0s -- host:5432 demo/hello" {
		t.Errorf("net: %q", steps)
	}
}

func TestRequestsInterrupted(t *testing.T) {
	r := newRig(t)
	st := &requests.Store{Paths: r.p}
	q := proto.Request{ID: requests.NewID(), Time: time.Now(), Ref: hello.String(), Kind: "seal", Params: proto.RequestParams{Kind: "seal", Path: "results/x"},
		Risk: "low", Status: proto.RequestApproved}
	if err := st.Put(hello, q); err != nil {
		t.Fatal(err)
	}
	r.boxUp(false)
	r.start()
	list, _ := st.Load(hello)
	if len(list) != 1 || list[0].Status != proto.RequestFailed || !strings.Contains(list[0].Result, "not repeated") {
		t.Errorf("after a restart: %+v", list)
	}
	if len(r.devcCalls("seal")) != 0 {
		t.Error("an interrupted request was run again")
	}
}

// TestRequestExecutor: approved requests run on a bounded executor and
// count against the per-project limit until they finish (finding C1-11).
func TestRequestExecutor(t *testing.T) {
	oldW, oldQ, oldO := requestWorkers, requestQueue, maxOutstanding
	requestWorkers, requestQueue, maxOutstanding = 1, 2, 4
	t.Cleanup(func() { requestWorkers, requestQueue, maxOutstanding = oldW, oldQ, oldO })
	// Seal requests: a box may have only one savepoint request outstanding
	// (C20a).
	r := boxRig(t, "[requests]\nauto = [\"seal\"]\n[data]\nwork = [\"w\"]\n", "")
	r.d.o.Global = []string{"--config-dir", "/cfg"}
	release := make(chan struct{})
	var mu sync.Mutex
	running := 0
	r.runner.On("/fake/devc", "--config-dir", "/cfg", "seal").Do(func(execx.Cmd) {
		mu.Lock()
		running++
		mu.Unlock()
		<-release
	})
	var ids []string
	for i := 0; i < 3; i++ {
		rep, err := r.request(t, proto.RequestParams{Kind: "seal", Path: fmt.Sprintf("w/run%d", i)})
		if err != nil || rep.Status != proto.RequestApproved {
			t.Fatalf("seal %d: %+v %v", i, rep, err)
		}
		ids = append(ids, rep.ID)
	}
	// The executor is full: an automatic approval is refused as busy, and
	// a local approval leaves the request pending.
	if _, err := r.request(t, proto.RequestParams{Kind: "seal", Path: "w/run3"}); !proto.IsCode(err, proto.CodeBusy) || !strings.Contains(err.Error(), "busy carrying out") {
		t.Errorf("seal with the executor full: %v", err)
	}
	pkg, err := r.request(t, proto.RequestParams{Kind: "package", Manager: "apk", Packages: []string{"jq"}})
	if err != nil {
		t.Fatal(err)
	}
	c := r.client(nil)
	var dec proto.DecideResult
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: pkg.ID, Approve: true}, &dec); !proto.IsCode(err, proto.CodeBusy) {
		t.Errorf("approval with the executor full: %v", err)
	}
	if st := r.waitRequest(t, pkg.ID); st.Status != proto.RequestPending {
		t.Errorf("after a busy approval: %+v", st)
	}
	// Three approved and one pending reach the project's limit of four.
	if _, err := r.request(t, proto.RequestParams{Kind: "note", Text: "x"}); !proto.IsCode(err, proto.CodeBusy) || !strings.Contains(err.Error(), "too many requests") {
		t.Errorf("approved requests not counted: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if running != 1 {
		t.Errorf("%d requests run at once with one worker", running)
	}
	mu.Unlock()
	close(release)
	for _, id := range ids {
		if st := r.waitRequest(t, id); st.Status != proto.RequestDone {
			t.Errorf("%s: %+v", id, st)
		}
	}
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: pkg.ID, Approve: true}, &dec); err != nil || dec.Request.Status != proto.RequestApproved {
		t.Errorf("approval with room again: %+v %v", dec, err)
	}
}

// TestRequestLogs: a broken log in one project does not stop decisions in
// another (finding C1-10); a status poll never expires a request decided
// meanwhile (C1-18).
func TestRequestLogs(t *testing.T) {
	r := boxRig(t, "[requests]\nauto = []\n", "")
	c := r.client(nil)
	// demo/analysis sorts before demo/hello; its log is unreadable.
	analysis := names.ProjectRef{Realm: "demo", Project: "analysis"}
	os.MkdirAll(r.p.RequestsFile(analysis), 0o700)
	note, err := r.request(t, proto.RequestParams{Kind: "note", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var dec proto.DecideResult
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: note.ID, Approve: true}, &dec); err != nil || dec.Request.Status != proto.RequestDone {
		t.Errorf("decision beside a broken log: %+v %v", dec, err)
	}
	var list proto.RequestsListResult
	if err := c.Call(context.Background(), proto.MethodRequestsList, proto.RequestsListParams{All: true}, &list); err != nil || len(list.Requests) != 1 {
		t.Errorf("list beside a broken log: %+v %v", list, err)
	}

	// A pending request past its expiry is approved while a status poll
	// holds the old state.
	st := &requests.Store{Paths: r.p}
	old := proto.Request{ID: requests.NewID(), Time: time.Now().Add(-8 * 24 * time.Hour), Ref: hello.String(), Kind: "note",
		Params: proto.RequestParams{Kind: "note", Text: "old"}, Risk: "low", Status: proto.RequestPending}
	st.Put(hello, old)
	statusExpireHook = func() {
		statusExpireHook = nil
		approved := old
		approved.Status, approved.Result = proto.RequestDone, "noted"
		st.Put(hello, approved)
	}
	t.Cleanup(func() { statusExpireHook = nil })
	if got := r.waitRequest(t, old.ID); got.Status != proto.RequestDone {
		t.Errorf("status poll over a decision: %+v", got)
	}
	list2, _ := st.Load(hello)
	for _, q := range list2 {
		if q.ID == old.ID && q.Status != proto.RequestDone {
			t.Errorf("stored: %+v", q)
		}
	}
}

// recordStart writes the capability record a successful start of the
// rig's box writes, from the current configuration.
func (r *rig) recordStart() {
	r.t.Helper()
	c, _ := config.Load(r.p)
	res, err := c.Resolved(hello)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := box.WriteCapabilities(r.p, hello, box.CapabilitiesOf(res, r.cid, time.Now())); err != nil {
		r.t.Fatal(err)
	}
}

// TestPushWatchdog: the watchdog ends a push whose quarantine would take
// the relay above its budget, or the file system below half the reserve,
// and leaves an ordinary push alone (finding D1-05).
func TestPushWatchdog(t *testing.T) {
	old := gitx.WatchEvery
	gitx.WatchEvery = 20 * time.Millisecond
	t.Cleanup(func() { gitx.WatchEvery = old })
	d := &Daemon{freeSpace: func(string) (int64, error) { return 100 << 30, nil }}
	relay := filepath.Join(t.TempDir(), "p.git")
	q := filepath.Join(relay, "objects", gitx.QuarantinePrefix+"abc123", "pack")
	if err := os.MkdirAll(q, 0o700); err != nil {
		t.Fatal(err)
	}
	pack, err := os.Create(filepath.Join(q, "tmp_pack_x"))
	if err != nil {
		t.Fatal(err)
	}
	defer pack.Close()
	watch := func(used, quota int64) chan string {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		ch := make(chan string, 1)
		go func() { ch <- d.watchPush(ctx, relay, used, quota) }()
		return ch
	}
	// Within the budget: nothing happens.
	pack.Write(make([]byte, 1<<20))
	ch := watch(1<<20, 4<<20)
	select {
	case why := <-ch:
		t.Fatalf("a push within its budget was ended: %s", why)
	case <-time.After(300 * time.Millisecond):
	}
	// The pack grows past the budget while git writes it.
	pack.Write(make([]byte, 3<<20))
	select {
	case why := <-ch:
		if !strings.Contains(why, "git.relay_quota") {
			t.Errorf("reason %q", why)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not end a push beyond the budget")
	}
	// Without a quota only the free space counts.
	free := int64(pushMinFree)
	d.freeSpace = func(string) (int64, error) { return free, nil }
	ch = watch(0, 0)
	time.Sleep(100 * time.Millisecond)
	free = pushMinFree/2 - 1
	select {
	case why := <-ch:
		if !strings.Contains(why, "free") {
			t.Errorf("reason %q", why)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not end a push that fills the disk")
	}
	gitx.RemoveQuarantines(relay)
	if _, err := os.Stat(filepath.Dir(q)); !os.IsNotExist(err) {
		t.Errorf("the quarantine is still there: %v", err)
	}
}

// TestSavepointRequestWindow: a box gets at most one savepoint made on
// request per data.savepoints.requested_interval. A savepoint request is
// refused, with the reason, while another one of the project is
// outstanding or when a requested savepoint is younger than the
// interval; the owner's own savepoints do not count, and once the
// interval has passed a request is taken (X9 1).
func TestSavepointRequestWindow(t *testing.T) {
	r := boxRig(t, "[requests]\nauto = []\n[data.savepoints]\nrequested_interval = \"10m\"\n", "")
	first, err := r.request(t, proto.RequestParams{Kind: "savepoint"})
	if err != nil || first.Status != proto.RequestPending {
		t.Fatalf("first savepoint request: %+v %v", first, err)
	}
	if _, err := r.request(t, proto.RequestParams{Kind: "savepoint", Name: "again"}); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), first.ID) {
		t.Errorf("a second savepoint request while one is pending: %v", err)
	}
	c := r.client(nil)
	var dec proto.DecideResult
	if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: first.ID, Approve: false, Reason: "no"}, &dec); err != nil {
		t.Fatal(err)
	}
	// A requested savepoint made five minutes ago, and an owner's one made
	// a minute ago.
	index := r.p.SavepointIndex(hello)
	os.MkdirAll(filepath.Dir(index), 0o700)
	now := time.Now().UTC()
	write := func(lines ...string) {
		if err := os.WriteFile(index, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec := func(id string, at time.Time, class string) string {
		return fmt.Sprintf(`{"v":1,"op":"save","id":%q,"time":%q,"files":0,"size":0,"class":%q}`, id, at.Format(time.RFC3339Nano), class)
	}
	write(rec("20260101T000000Z", now.Add(-5*time.Minute), "requested"), rec("20260101T000001Z", now.Add(-time.Minute), ""))
	if _, err := r.request(t, proto.RequestParams{Kind: "savepoint"}); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "requested_interval") {
		t.Errorf("a savepoint request inside the interval: %v", err)
	}
	// The control: the requested one is eleven minutes old.
	write(rec("20260101T000000Z", now.Add(-11*time.Minute), "requested"), rec("20260101T000001Z", now.Add(-time.Minute), ""))
	if rep, err := r.request(t, proto.RequestParams{Kind: "savepoint"}); err != nil || rep.Status != proto.RequestPending {
		t.Errorf("a savepoint request after the interval: %+v %v", rep, err)
	}
}
