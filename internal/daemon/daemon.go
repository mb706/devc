// Package daemon is the machine daemon (work order C08): one user service
// per machine that keeps a channel to every running box, ingests session
// events continuously, reconciles lost sessions, runs the housekeeping the
// banking timer used to run, and serves a local JSON-RPC API with event
// streams on <runtime>/daemon.sock. Phase 1 keeps working without it: the
// daemon only adds, and holds nothing that cannot be rebuilt from the
// journals, the engine and the configuration.
//
// Security rules: everything a box sends is hostile input (README rule 2).
// The agent socket lies in a box-writable directory: it is opened without
// following symlinks, must be a socket, is connected through
// /proc/self/fd/<n>, and its peer must be a process of that box's container
// by its credentials and cgroup (DialBoxSocket, CheckPeer). Messages are
// bounded and decoded strictly (internal/proto); box replies are validated
// like the CLI validates `devc box session ps` output, and nothing from a
// box is used in a path or command without validation. Requests from a box
// are attributed by the channel they arrive on. Configuration comes only
// from the config directory (rule 1). The API socket is 0600 in a 0700
// directory: the same user is the only client. Host commands run with an
// argument vector from / (rule 3). The daemon dials out to the hubs listed
// in config.toml (hublink.go); a hub may call only the allowlisted,
// box-scoped methods of hubMethods, with origin hub, and sees no host
// paths. Beside each hub link an uploader keeps the hub's archive of the
// durable state current (archive.go, internal/fleet), reading box-writable
// harness state only through a helper-container listing and safefs.
// Services (services.go, internal/svclogin) hold credentials that never
// enter a box: a box's request is checked on its raw bytes and against the
// service's rules before the daemon forwards it with the credential added.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/fleet"
	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/github"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/session"
)

// File names in the runtime directory.
const (
	SocketName = "daemon.sock"
	LockName   = "daemon.lock"
)

// maxSocketPath is the limit of sun_path (108 bytes with the NUL).
const maxSocketPath = 107

// Intervals of the daemon's jobs; Options may shorten them (tests).
const (
	DefaultWatch        = 3 * time.Second
	DefaultReconcile    = 30 * time.Second
	DefaultHousekeeping = 5 * time.Minute
	DefaultJournalPoll  = time.Second
	idlePoll            = 2 * time.Second
	busyPoll            = 250 * time.Millisecond
	busyFor             = 5 * time.Second
	maxAPIConns         = 64
)

// Options configure a daemon.
type Options struct {
	// Paths are the resolved roots (before config.toml); Load reads the
	// configuration (from Paths' config directory).
	Paths *paths.Paths
	Load  func() (*config.Config, []config.Problem)
	// Runner runs host commands; Engine builds the engine of a
	// configuration (default: Podman through Runner).
	Runner execx.Runner
	Engine func(*config.Config) engine.Engine
	// Executable and Version are this devc; Housekeeping is the argument
	// lists run with Executable at every housekeeping pass.
	Executable   string
	Version      string
	Housekeeping [][]string
	// HousekeepingEnv is added to the environment of those commands.
	HousekeepingEnv []string
	Log             *slog.Logger
	// Intervals (zero: the defaults).
	WatchInterval        time.Duration
	ReconcileInterval    time.Duration
	HousekeepingInterval time.Duration
	JournalPoll          time.Duration
	// ProcDir is /proc (tests point it elsewhere).
	ProcDir string
	// HelloTimeout bounds a box's answer to box.hello (10 s); MaxBackoff
	// is the longest pause between connection attempts (30 s).
	HelloTimeout time.Duration
	MaxBackoff   time.Duration
	// Global are the global flags (directories) of every devc child
	// process: banks after turns and approved requests. They run with
	// HousekeepingEnv too.
	Global []string
	// Secrets is the secret store for host.secret (default: the file store
	// of the configuration's state directory).
	Secrets secrets.Store
	// TurnBankInterval is the least time between banks after turns of one
	// box (DefaultTurnBank).
	TurnBankInterval time.Duration
	// SSHD is the sshd the SSH gate starts (found in PATH and the sbin
	// directories when empty); SSHConfig is DEVC_SSH_CONFIG, handed to
	// gate-exec (work order C10b).
	SSHD      string
	SSHConfig string
	// Resolve replaces the system resolver of the allowlist proxy, and
	// ProxyExempt lists addresses the proxy treats as public. Only tests
	// set them (hidden flags of devc daemon), to reach a local server
	// without the internet; nothing in the configuration does.
	Resolve     func(ctx context.Context, host string) ([]netip.Addr, error)
	ProxyExempt []netip.AddrPort
	// RouterListen overrides router.listen of config.toml (tests).
	RouterListen string
	// GitHub are the GitHub base URLs (DEVC_GITHUB_API, DEVC_GITHUB_URL;
	// zero: github.com's), work order C15.
	GitHub github.Endpoints
	// ArchiveInterval is the pause between archive passes
	// (fleet.DefaultInterval); ArchiveScanEvery how often the shared
	// harness state is listed (fleet.DefaultScanEvery). ArchiveWorkers
	// replaces the container workers (tests).
	ArchiveInterval  time.Duration
	ArchiveScanEvery time.Duration
	ArchiveWorkers   func(*config.Config, engine.Engine) fleet.Workers
	// ServiceTransport reaches the upstreams of services (default: a
	// transport with the host's trust store); tests replace it.
	ServiceTransport http.RoundTripper
}

// env is one loaded configuration and what is built from it.
type env struct {
	cfg   *config.Config
	eng   engine.Engine
	mgr   *box.Manager
	store *session.Store
	refs  []names.ProjectRef
}

// Daemon is a running daemon.
type Daemon struct {
	o        Options
	log      *slog.Logger
	version  string
	instance string
	started  time.Time
	machine  string
	procDir  string
	runtime  string

	ctx  context.Context
	stop context.CancelFunc

	mu       sync.Mutex
	env      *env
	stamp    string
	boxes    map[names.ProjectRef]*boxInfo
	windows  map[names.ProjectRef]map[string]string
	warnings map[names.ProjectRef]*pendingWarn

	hub        *hub
	cmd        *commander
	requests   *requestService
	turns      *turnBanker
	fetchLimit *rateLimit
	hubs       *hubManager
	term       *termManager
	wtCache    worktreeCache
	wg         sync.WaitGroup

	// The network services (netproxy.go, router.go).
	netMu     sync.Mutex
	netLogs   map[names.ProjectRef]*netpolicy.NetLog
	hostAddrs hostAddrCache
	router    *router

	// GitHub (github.go): installation tokens and the per-project budget of
	// facade reads.
	minter   *github.Minter
	ghReads  *readBudget
	ghTokens *readBudget

	// scanner lists the shared harness state for the archive uploaders of
	// all hubs (archive.go).
	scanner *fleet.HarnessScanner

	// svc serves the services' streams (services.go).
	svc *serviceManager
}

// boxInfo is what the last box-watch pass saw of a configured project.
type boxInfo struct {
	state       string // running, stopped, absent
	containerID string
	started     *time.Time
	worker      *boxWorker
}

// pendingWarn collects ingestion problems until a client asks.
type pendingWarn struct {
	invalid int
	text    string
}

func newInstance() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// New prepares a daemon; Run starts it.
func New(o Options) (*Daemon, error) {
	if o.Paths == nil || o.Load == nil {
		return nil, errors.New("daemon: paths and configuration loader are required")
	}
	rt, err := o.Paths.RuntimeDir()
	if err != nil {
		return nil, err
	}
	if len(filepath.Join(rt, SocketName)) > maxSocketPath {
		return nil, fmt.Errorf("the socket path %s is longer than %d bytes: use a shorter runtime directory (%s)", filepath.Join(rt, SocketName), maxSocketPath, paths.EnvRuntimeDir)
	}
	if o.Runner == nil {
		o.Runner = execx.Real{}
	}
	if o.Engine == nil {
		runner := o.Runner
		o.Engine = func(c *config.Config) engine.Engine { return engine.NewPodman(c.Global.Engine, runner) }
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.WatchInterval, DefaultWatch)
	def(&o.ReconcileInterval, DefaultReconcile)
	def(&o.HousekeepingInterval, DefaultHousekeeping)
	def(&o.JournalPoll, DefaultJournalPoll)
	def(&o.HelloTimeout, helloTimeout)
	def(&o.MaxBackoff, maxBackoff)
	def(&o.TurnBankInterval, DefaultTurnBank)
	def(&o.ArchiveInterval, fleet.DefaultInterval)
	def(&o.ArchiveScanEvery, fleet.DefaultScanEvery)
	if o.ProcDir == "" {
		o.ProcDir = "/proc"
	}
	if o.GitHub.API == nil || o.GitHub.Web == nil {
		def := github.DefaultEndpoints()
		if o.GitHub.API == nil {
			o.GitHub.API = def.API
		}
		if o.GitHub.Web == nil {
			o.GitHub.Web = def.Web
		}
	}
	host, _ := os.Hostname()
	instance := newInstance()
	d := &Daemon{o: o, log: o.Log, version: o.Version, instance: instance, started: time.Now().UTC(), machine: host,
		procDir: o.ProcDir, runtime: rt, boxes: map[names.ProjectRef]*boxInfo{}, windows: map[names.ProjectRef]map[string]string{},
		warnings: map[names.ProjectRef]*pendingWarn{}, hub: newHub(o.Log, instance), fetchLimit: newRateLimit(fetchThroughEvery)}
	d.minter = &github.Minter{}
	d.scanner = &fleet.HarnessScanner{Every: o.ArchiveScanEvery}
	d.ghReads = newReadBudget(ghReadsPerMinute)
	d.ghTokens = newReadBudget(ghTokensPerMinute)
	d.requests = newRequestService(d)
	d.hubs = newHubManager(d)
	d.term = newTermManager(d)
	d.turns = newTurnBanker(d, o.TurnBankInterval)
	d.svc = newServiceManager(d)
	d.hub.onTurnEnd = d.turns.turnEnded
	return d, nil
}

// SocketPath is the daemon socket of a runtime directory.
func SocketPath(runtime string) string { return filepath.Join(runtime, SocketName) }

// goSafe runs fn in a goroutine that logs a panic instead of ending the
// daemon.
func (d *Daemon) goSafe(what string, fn func()) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				d.log.Error("panic", "in", what, "panic", fmt.Sprint(r))
			}
		}()
		fn()
	}()
}

func (d *Daemon) current() *env {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.env
}

// Run runs the daemon until ctx ends or a client asks it to stop. It
// fails when another daemon holds the lock.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.o.Paths.Ensure(d.runtime); err != nil {
		return err
	}
	if err := os.Chmod(d.runtime, 0o700); err != nil {
		return err
	}
	unlock, err := flock.Acquire(ctx, filepath.Join(d.runtime, LockName), flock.Options{Timeout: time.Millisecond})
	if err != nil {
		if errors.Is(err, flock.ErrTimeout) {
			return errors.New("another devc daemon is running for this runtime directory ('devc daemon status')")
		}
		return err
	}
	defer unlock()
	sock := SocketPath(d.runtime)
	if err := os.Remove(sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the old socket: %w", err)
	}
	old := syscall.Umask(0o177)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	d.ctx, d.stop = context.WithCancel(ctx)
	defer d.stop()
	if err := d.reload(true); err != nil {
		ln.Close()
		return err
	}
	d.cmd = newCommander(d, d.current().cfg.Paths)
	d.cmd.resume()
	d.requests.recoverInterrupted(d.ctx)
	d.requests.expireAll(d.ctx)
	d.requests.start()
	d.startRouter()
	d.svc.start()
	d.log.Info("daemon started", "version", d.version, "instance", d.instance, "socket", sock)
	d.goSafe("api", func() { d.serveAPI(ln) })
	d.goSafe("watch", d.watchLoop)
	d.goSafe("journals", d.journalLoop)
	d.goSafe("reconcile", d.reconcileLoop)
	d.goSafe("housekeeping", d.housekeepingLoop)
	d.hubs.start(d.ctx)
	<-d.ctx.Done()
	d.log.Info("daemon stopping")
	ln.Close()
	d.turns.stop()
	if d.router != nil {
		d.router.stop()
	}
	d.svc.stop()
	d.hub.closeAll(context.Background())
	d.term.closeAll()
	d.mu.Lock()
	for _, b := range d.boxes {
		if b.worker != nil {
			b.worker.stop()
		}
	}
	d.mu.Unlock()
	waited := make(chan struct{})
	go func() { d.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		d.log.Warn("some tasks did not end in time")
	}
	_ = os.Remove(sock)
	return nil
}

// Stop asks the daemon to stop.
func (d *Daemon) Stop() {
	if d.stop != nil {
		d.stop()
	}
}

// configStamp fingerprints the configuration files whose change requires a
// reload: names, sizes and modification times of the TOML files and of the
// directories that hold them (additions and removals).
func configStamp(dir string) string {
	var b strings.Builder
	add := func(p string) {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s %d %d\n", p, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	glob := func(pattern string) {
		ms, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range ms {
			add(m)
		}
	}
	add(filepath.Join(dir, "config.toml"))
	for _, d := range []string{"realms", "harness", "envs"} {
		add(filepath.Join(dir, d))
	}
	glob("realms/*")
	glob("realms/*/realm.toml")
	glob("realms/*/projects")
	glob("realms/*/projects/*")
	glob("realms/*/projects/*/project.toml")
	glob("realms/*/services")
	glob("realms/*/services/*")
	glob("realms/*/services/*/service.toml")
	glob("harness/*.toml")
	glob("envs/*")
	glob("envs/*/env.toml")
	return b.String()
}

// reload loads the configuration when it changed since the last load (or
// always with force).
func (d *Daemon) reload(force bool) error {
	stamp := configStamp(d.o.Paths.ConfigDir())
	d.mu.Lock()
	same := stamp == d.stamp && d.env != nil
	d.mu.Unlock()
	if same && !force {
		return nil
	}
	c, probs := d.o.Load()
	if c == nil {
		return errors.New("the configuration could not be loaded")
	}
	if len(probs) > 0 {
		d.log.Warn("the configuration has problems; run 'devc config check'", "problems", len(probs))
	}
	eng := d.o.Engine(c)
	mgr := box.NewManager(c, eng, d.o.Runner, d.o.Executable, d.version, io.Discard)
	store := session.NewStore(c.Paths)
	store.LockTimeout = 2 * time.Second
	var refs []names.ProjectRef
	for _, p := range c.Projects("") {
		refs = append(refs, p.Ref())
	}
	e := &env{cfg: c, eng: eng, mgr: mgr, store: store, refs: refs}
	d.mu.Lock()
	d.env, d.stamp = e, stamp
	d.mu.Unlock()
	d.hub.setProjects(c.Paths, refs)
	d.hubs.sync(c)
	if !force {
		d.log.Info("configuration reloaded", "projects", len(refs))
	}
	return nil
}

func (d *Daemon) every(interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fn()
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// watchLoop lists the boxes every few seconds and keeps a worker per
// running box of a configured project.
func (d *Daemon) watchLoop() {
	d.every(d.o.WatchInterval, func() {
		if err := d.reload(false); err != nil {
			d.log.Warn("reloading the configuration", "err", err)
		}
		if err := d.watchOnce(d.ctx); err != nil && d.ctx.Err() == nil {
			d.log.Warn("listing boxes", "err", err)
		}
		// A new `devc join` for a listed hub changes only its credentials.
		d.hubs.sync(nil)
	})
}

func (d *Daemon) watchOnce(ctx context.Context) error {
	e := d.current()
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cs, err := e.eng.Ps(lctx, engine.PsOptions{All: true, Labels: map[string]string{box.LabelRole: box.RoleBox}})
	if err != nil {
		return err
	}
	byName := map[string]engine.Container{}
	for _, c := range cs {
		byName[c.Name()] = c
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := map[names.ProjectRef]bool{}
	for _, ref := range e.refs {
		seen[ref] = true
		bi := d.boxes[ref]
		if bi == nil {
			bi = &boxInfo{}
			d.boxes[ref] = bi
		}
		c, ok := byName[names.BoxName(ref)]
		// A container with the box's name that devc did not create for
		// this project is never acted on (box.Manager.Find).
		if ok && (c.Labels[box.LabelRealm] != ref.Realm || c.Labels[box.LabelProject] != ref.Project) {
			ok = false
		}
		switch {
		case !ok:
			bi.state, bi.containerID, bi.started = "absent", "", nil
		case c.State == "running":
			bi.state = "running"
			if c.StartedAt > 0 {
				t := time.Unix(c.StartedAt, 0).UTC()
				bi.started = &t
			}
		default:
			bi.state, bi.started = "stopped", nil
		}
		if ok {
			bi.containerID = c.ID
		}
		if bi.state == "running" {
			if bi.worker != nil && bi.worker.containerID != c.ID {
				bi.worker.stop()
				bi.worker = nil
				d.forgetRoute(ref)
			}
			if bi.worker == nil {
				bi.worker = d.startWorker(ref, c.ID)
			}
		} else if bi.worker != nil {
			bi.worker.stop()
			bi.worker = nil
			d.forgetRoute(ref)
		}
	}
	for ref, bi := range d.boxes {
		if !seen[ref] {
			if bi.worker != nil {
				bi.worker.stop()
			}
			delete(d.boxes, ref)
		}
	}
	return nil
}

// forgetRoute drops the router's pin of a box that stopped or was
// replaced.
func (d *Daemon) forgetRoute(ref names.ProjectRef) {
	if d.router != nil {
		d.router.forget(ref)
	}
}

func (d *Daemon) worker(ref names.ProjectRef) *boxWorker {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bi := d.boxes[ref]; bi != nil {
		return bi.worker
	}
	return nil
}

func (d *Daemon) boxState(ref names.ProjectRef) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bi := d.boxes[ref]; bi != nil {
		return bi.state
	}
	return ""
}

// journalLoop follows every project's journals, so events written by the
// CLI reach subscribers too.
func (d *Daemon) journalLoop() {
	d.every(d.o.JournalPoll, func() { d.hub.poll(nil) })
}

// ingestLoop polls a box's spool: every 250 ms while events arrive, every
// 2 s when idle, and at once when the box sends a hint. A run is skipped
// when no spool file changed since the last one.
func (w *boxWorker) ingestLoop(ctx context.Context) {
	var last map[string]int64
	more := false
	var busyUntil time.Time
	for {
		interval := idlePoll
		if time.Now().Before(busyUntil) || more {
			interval = busyPoll
		}
		select {
		case <-ctx.Done():
			return
		case <-w.hint:
		case <-time.After(interval):
		}
		e := w.d.current()
		snap, err := e.store.SpoolSnapshot(w.ref)
		if err != nil {
			w.d.log.Debug("spool snapshot", "ref", w.ref.String(), "err", err)
			continue
		}
		if !more && sameSnapshot(snap, last) {
			continue
		}
		rep, err := w.d.ingest(ctx, e, w.ref)
		if err != nil {
			w.d.log.Debug("ingesting", "ref", w.ref.String(), "err", err)
			continue
		}
		last, more = snap, rep.More
		if rep.Events > 0 {
			busyUntil = time.Now().Add(busyFor)
		}
		// A box that floods hints still costs one run per interval.
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func sameSnapshot(a, b map[string]int64) bool {
	if b == nil || len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// ingest runs one ingestion of a box's spool under the per-box ingestion
// lock (shared with the CLI) and publishes what reached the journals.
func (d *Daemon) ingest(ctx context.Context, e *env, ref names.ProjectRef) (session.IngestReport, error) {
	unlock, err := e.store.Lock(ctx, ref)
	if err != nil {
		return session.IngestReport{}, err
	}
	rep, err := e.store.Ingest(ref)
	unlock()
	d.noteInvalid(ref, rep.Invalid, "")
	d.noteRefused(ref, rep.Refused)
	if rep.Events > 0 || rep.Notes > 0 {
		d.hub.poll(&ref)
	}
	return rep, err
}

// recordHost appends an event the host writes itself (seen, answer) to a
// session's journal: under the ingestion lock, after ingesting what the box
// spooled, so it follows what the box reported before it.
func (d *Daemon) recordHost(ctx context.Context, ref names.ProjectRef, sid, typ string, data any) error {
	e := d.current()
	unlock, err := e.store.Lock(ctx, ref)
	if err != nil {
		return err
	}
	rep, ierr := e.store.Ingest(ref)
	d.noteInvalid(ref, rep.Invalid, "")
	d.noteRefused(ref, rep.Refused)
	err = e.store.AppendHostEvent(ref, sid, typ, data)
	unlock()
	d.hub.poll(&ref)
	if err != nil {
		return err
	}
	if ierr != nil {
		d.log.Debug("ingesting before a host event", "ref", ref.String(), "err", ierr)
	}
	return nil
}

// wakeBox asks a box's ingester for a run now.
func (d *Daemon) wakeBox(ref names.ProjectRef) {
	if w := d.worker(ref); w != nil {
		w.wake()
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (d *Daemon) noteInvalid(ref names.ProjectRef, invalid int, text string) {
	if invalid == 0 && text == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.warnings[ref]
	if w == nil {
		w = &pendingWarn{}
		d.warnings[ref] = w
	}
	w.invalid += invalid
	if text != "" {
		w.text = text
	}
	if invalid > 0 {
		d.log.Warn("invalid spool lines ignored", "ref", ref.String(), "lines", invalid)
	}
}

// noteRefused logs and keeps for the next client the spool files of a box
// that ingestion refused because their session belongs to another project
// (finding C1-01).
func (d *Daemon) noteRefused(ref names.ProjectRef, refused []session.Refusal) {
	for _, rf := range refused {
		d.log.Warn("refused a spool file: its session belongs to another project", "ref", ref.String(), "session", rf.Session, "owner", rf.Owner)
		d.noteInvalid(ref, 0, session.RefusalText(rf))
	}
}

// takeWarnings returns and clears the pending warnings of refs.
func (d *Daemon) takeWarnings(refs []names.ProjectRef) []proto.Warning {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []proto.Warning
	for _, ref := range refs {
		w := d.warnings[ref]
		if w == nil {
			continue
		}
		if w.text != "" {
			out = append(out, proto.Warning{Ref: ref.String(), Text: w.text})
		}
		if w.invalid > 0 {
			out = append(out, proto.Warning{Ref: ref.String(), Text: fmt.Sprintf("%d invalid spool line(s) ignored", w.invalid)})
		}
		delete(d.warnings, ref)
	}
	return out
}

// refresh ingests and reconciles one project as `devc ps` does without a
// daemon (session.Store.Refresh), asking a connected box over its channel
// for its live sessions.
func (d *Daemon) refresh(ctx context.Context, e *env, ref names.ProjectRef) (session.RefreshResult, error) {
	observe := func(ctx context.Context) (session.BoxState, error) {
		_, running, ready, err := e.mgr.State(ctx, ref)
		st := session.BoxState{Running: running, Ready: ready, Container: names.BoxName(ref)}
		if w := d.worker(ref); w != nil && w.connected() {
			st.Live = w.live
		}
		return st, err
	}
	res, err := e.store.Refresh(ctx, ref, observe, e.eng)
	d.noteInvalid(ref, res.Report.Invalid, res.Warning)
	d.noteRefused(ref, res.Report.Refused)
	d.mu.Lock()
	if res.Live != nil {
		wins := map[string]string{}
		for _, l := range res.Live {
			if l.Window != "" {
				wins[l.Session] = l.Window
			}
		}
		d.windows[ref] = wins
	}
	d.mu.Unlock()
	d.hub.poll(&ref)
	return res, err
}

// reconcileLoop reconciles every project with a running box or open
// sessions (C04's lost sessions).
func (d *Daemon) reconcileLoop() {
	t := time.NewTicker(d.o.ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
		}
		e := d.current()
		open := d.hub.openRefs()
		for _, ref := range e.refs {
			if d.ctx.Err() != nil {
				return
			}
			if d.boxState(ref) != "running" && !open[ref] {
				continue
			}
			ctx, cancel := context.WithTimeout(d.ctx, 2*time.Minute)
			if _, err := d.refresh(ctx, e, ref); err != nil && d.ctx.Err() == nil {
				d.log.Debug("reconciling", "ref", ref.String(), "err", err)
			}
			cancel()
		}
	}
}

// housekeepingLoop runs the banking timer's work every interval: bank every
// running box and commit every realm's context (work order C08 job 4), as
// separate devc processes so that a failure there cannot touch the daemon.
func (d *Daemon) housekeepingLoop() {
	t := time.NewTicker(d.o.HousekeepingInterval)
	defer t.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
		}
		d.housekeep(d.ctx)
	}
}

func (d *Daemon) housekeep(ctx context.Context) {
	if d.cmd != nil {
		d.cmd.prune()
	}
	d.requests.expireAll(ctx)
	for _, args := range d.o.Housekeeping {
		if ctx.Err() != nil {
			return
		}
		res, err := d.o.Runner.Run(ctx, execx.Cmd{Name: d.o.Executable, Args: args, Env: d.o.HousekeepingEnv, Dir: "/", Timeout: 30 * time.Minute, MaxOutput: 64 << 10})
		out := strings.TrimSpace(string(res.Stdout) + "\n" + string(res.Stderr))
		if err != nil {
			d.log.Warn("housekeeping", "command", strings.Join(args, " "), "err", err, "output", proto.PrintableText(out))
			continue
		}
		d.log.Info("housekeeping", "command", strings.Join(args, " "), "output", proto.PrintableText(out))
	}
}

// serveAPI accepts clients on the daemon socket.
func (d *Daemon) serveAPI(ln *net.UnixListener) {
	sem := make(chan struct{}, maxAPIConns)
	handlers := d.handlers()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if d.ctx.Err() == nil {
				d.log.Error("accepting API clients", "err", err)
				d.stop()
			}
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			c.Close()
			continue
		}
		conn := proto.NewConn(c, proto.Options{Handlers: handlers, ExposeErrors: true, Context: d.ctx, CallTimeout: time.Minute, Log: d.log})
		go func() {
			<-conn.Done()
			<-sem
		}()
	}
}
