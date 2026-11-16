package boxside

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/tmux"
)

// Paths of the harness descriptions and delivered secrets in a box.
const (
	HarnessEtcDir = "/etc/devc/harness"
	SecretsDir    = "/run/devc-secrets"
)

// LaunchSchema is the current version of a launch description.
const LaunchSchema = 1

// Launch is /etc/devc/harness/<id>.json, written by the host at `devc up`
// for every installed harness of the project. It is everything the
// launcher needs; the box never sees the configuration directory.
type Launch struct {
	Schema  int    `json:"schema"`
	ID      string `json:"id"`
	Bin     string `json:"bin"`
	Display string `json:"display"`
	Realm   string `json:"realm"`
	Project string `json:"project"`
	// Exe is the real executable: below /opt/devc/harness/<id>/current, or
	// an absolute path in the image.
	Exe string `json:"exe"`
	// Env is the manifest's [env] with home.env applied over it.
	Env     map[string]string `json:"env"`
	Args    []string          `json:"args"`
	Resume  []string          `json:"resume"`
	Auth    LaunchAuth        `json:"auth"`
	Home    LaunchHome        `json:"home"`
	Seeds   []LaunchSeed      `json:"seeds"`
	Secrets []LaunchSecret    `json:"secrets"`
	Hooks   string            `json:"hooks"`
	Status  LaunchStatus      `json:"status"`
	Input   LaunchInput       `json:"input"`
	Memory  string            `json:"memory"`
	// Transcripts are globs relative to Home.Dir.
	Transcripts []string `json:"transcripts"`
	// Delivery is "daemon" when the credential and granted secrets are
	// asked from the daemon at launch instead of read from SecretsDir
	// (secrets.delivery, work order C10a).
	Delivery string `json:"delivery,omitempty"`
	// GitHubToken is set when the project grants a GitHub token
	// (grants.github "app" or "token:<secret>", work order C15): the
	// launcher asks the daemon for it at every start and exports GH_TOKEN.
	GitHubToken bool `json:"github_token,omitempty"`
	// Path lists directories put first in PATH: the bin directories of the
	// toolbox components the harness needs (install.needs, work order C16),
	// in the concrete version directory current at the last devc up.
	Path []string `json:"path,omitempty"`
}

// DeliveryDaemon is Launch.Delivery for secrets served by the daemon.
const DeliveryDaemon = "daemon"

// LaunchAuth says how the credential reaches the harness.
type LaunchAuth struct {
	Method string `json:"method"`
	Env    string `json:"env,omitempty"`
	Secret string `json:"secret,omitempty"`
	// File is the delivered secret below /run/devc-secrets.
	File string `json:"file,omitempty"`
}

// LaunchHome describes the harness home and how it is shared.
type LaunchHome struct {
	Dir string `json:"dir"` // absolute
	// Also are further state directories (home.also), absolute.
	Also     []string `json:"also,omitempty"`
	Strategy string   `json:"strategy"`
	Shared   []string `json:"shared"`
	PerBox   []string `json:"per_box"`
}

// LaunchSeed is one rendered seed file and where box init puts it.
type LaunchSeed struct {
	Source string `json:"source"` // below /etc/devc/harness/<id>/seed
	Target string `json:"target"` // below Home.Dir
	When   string `json:"when"`   // missing | always
}

// LaunchSecret is a granted secret, exported under Env when that is set.
type LaunchSecret struct {
	Name string `json:"name"`
	File string `json:"file"`
	Env  string `json:"env,omitempty"`
}

type LaunchStatus struct {
	OSC7501 bool `json:"osc7501"`
}

type LaunchInput struct {
	Adapter   string `json:"adapter"`
	SubmitKey string `json:"submit_key"`
	// QuestionWait is how long (seconds) a question hook waits for an
	// answer through the box channel; 0: it never waits.
	QuestionWait int `json:"question_wait,omitempty"`
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// below reports whether p is a clean absolute path strictly below dir.
func below(dir, p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && strings.HasPrefix(p, dir+"/")
}

// Validate checks a description before it is used. The host renders it
// into a read-only mount, so failures mean a devc bug or version skew.
func (l *Launch) Validate() error {
	switch {
	case l.Schema != LaunchSchema:
		return fmt.Errorf("launch description schema %d, this devc understands %d: run 'devc up' on the host", l.Schema, LaunchSchema)
	case !names.Valid(l.ID) || !names.Valid(l.Bin):
		return fmt.Errorf("invalid harness id %q or bin %q", l.ID, l.Bin)
	case !path.IsAbs(l.Exe) || path.Clean(l.Exe) != l.Exe:
		return fmt.Errorf("harness %s: executable %q is not a clean absolute path", l.ID, l.Exe)
	case !below("/home/dev", l.Home.Dir):
		return fmt.Errorf("harness %s: home %q is not below /home/dev", l.ID, l.Home.Dir)
	case l.Delivery != "" && l.Delivery != DeliveryDaemon:
		return fmt.Errorf("harness %s: unknown secret delivery %q", l.ID, l.Delivery)
	}
	for k := range l.Env {
		if !envNameRE.MatchString(k) {
			return fmt.Errorf("harness %s: invalid environment variable %q", l.ID, k)
		}
	}
	for _, d := range l.Path {
		if !below("/opt/devc", d) || strings.ContainsRune(d, ':') {
			return fmt.Errorf("harness %s: PATH entry %q is not below /opt/devc", l.ID, d)
		}
	}
	for _, d := range l.Home.Also {
		if !below("/home/dev", d) {
			return fmt.Errorf("harness %s: home directory %q is not below /home/dev", l.ID, d)
		}
	}
	if l.Auth.Env != "" && !envNameRE.MatchString(l.Auth.Env) {
		return fmt.Errorf("harness %s: invalid credential variable %q", l.ID, l.Auth.Env)
	}
	if l.Auth.File != "" && !below(SecretsDir, l.Auth.File) {
		return fmt.Errorf("harness %s: credential file %q is not below %s", l.ID, l.Auth.File, SecretsDir)
	}
	for _, s := range l.Secrets {
		if !below(SecretsDir, s.File) || (s.Env != "" && !envNameRE.MatchString(s.Env)) {
			return fmt.Errorf("harness %s: invalid granted secret %q", l.ID, s.Name)
		}
	}
	for _, s := range l.Seeds {
		if !below(path.Join(HarnessEtcDir, l.ID, "seed"), s.Source) || !below(l.Home.Dir, s.Target) || (s.When != "missing" && s.When != "always") {
			return fmt.Errorf("harness %s: invalid seed %q -> %q", l.ID, s.Source, s.Target)
		}
	}
	return nil
}

// maxDescription bounds a launch description file.
const maxDescription = 1 << 20

// ReadLaunch reads the description of harness id below root ("/" in a box).
func ReadLaunch(root, id string) (*Launch, error) {
	if !names.Valid(id) {
		return nil, fmt.Errorf("invalid harness name %q", id)
	}
	b, err := readCapped(filepath.Join(root, HarnessEtcDir, id+".json"), maxDescription)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("harness %s is not available in this box: it must be listed in harnesses in project.toml and installed ('devc harness install %s'), then run 'devc up' on the host: %w", id, id, err)
		}
		return nil, err
	}
	var l Launch
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("parsing the description of %s: %w", id, err)
	}
	if l.ID != id {
		return nil, fmt.Errorf("the description of %s names harness %q", id, l.ID)
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return &l, nil
}

// ListLaunches returns every launch description below root, sorted by id.
func ListLaunches(root string) ([]*Launch, error) {
	des, err := os.ReadDir(filepath.Join(root, HarnessEtcDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Launch
	for _, de := range des {
		id, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok || !de.Type().IsRegular() || !names.Valid(id) {
			continue
		}
		l, err := ReadLaunch(root, id)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// FindByBin returns the description whose shim name is bin, or nil.
func FindByBin(root, bin string) (*Launch, error) {
	if !names.Valid(bin) {
		return nil, nil
	}
	ls, err := ListLaunches(root)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if l.Bin == bin {
			return l, nil
		}
	}
	return nil, nil
}

func readCapped(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, max)
	}
	return b, nil
}

// LaunchEnv is what the launcher reads from its process and box.
type LaunchEnv struct {
	// Root prefixes the box paths read (secrets); "/" in a box.
	Root    string
	Environ []string
	// Self is the running devc executable, resolved.
	Self   string
	Stderr io.Writer
	// NewSession generates a session ID.
	NewSession func() string
	// Getwd, Pid and Now describe the process for session.start; nil and
	// zero mean the running process and the clock.
	Getwd func() (string, error)
	Pid   int
	Now   func() time.Time
	// IsTerminal reports whether the launcher's standard input and output
	// are terminals (nil: they are not); Helper is the devc binary that
	// runs the status wrapper (default /opt/devc/bin/devc).
	IsTerminal func() bool
	Helper     string
	// FetchSecret asks the daemon for a secret (Delivery "daemon"; nil:
	// through the box agent).
	FetchSecret func(name string) (string, error)
	// FetchGitHubToken asks the daemon for the project's GitHub token (nil:
	// through the box agent).
	FetchGitHubToken func() (proto.GitHubTokenReply, error)
}

// Prepared is the result of PrepareLaunch: what exec receives.
type Prepared struct {
	Exe     string
	Argv    []string
	Env     []string
	Session string
}

// MaxLaunchDepth bounds nested launches, so a misconfigured executable that
// leads back to devc cannot loop.
const MaxLaunchDepth = 16

// PrepareLaunch performs steps 1-5 of the launcher (work order C03 section
// 4): checks the executable, settles the session ID, adds the credential,
// the granted secrets, the harness environment and the arguments.
func PrepareLaunch(l *Launch, userArgs []string, e LaunchEnv) (*Prepared, error) {
	if err := notSelf(filepath.Join(e.Root, l.Exe), e.Self); err != nil {
		return nil, fmt.Errorf("harness %s: %w", l.ID, err)
	}
	env := newEnvList(e.Environ)
	depth := 0
	fmt.Sscanf(env.get("DEVC_LAUNCH_DEPTH"), "%d", &depth)
	if depth >= MaxLaunchDepth {
		return nil, fmt.Errorf("harness %s: launched %d times in a row; the executable %s seems to lead back to devc", l.ID, depth, l.Exe)
	}

	// The session ID: kept when a session start (C04) or the user provides
	// one, but a harness started from inside another harness gets its own.
	session := env.get("DEVC_SESSION")
	if !names.ValidSessionID(session) || env.get("DEVC_HARNESS") != "" {
		session = e.NewSession()
	}

	for k, v := range l.Env {
		env.set(k, v)
	}
	if len(l.Path) > 0 {
		p := strings.Join(l.Path, ":")
		if cur := env.get("PATH"); cur != "" {
			p += ":" + cur
		}
		env.set("PATH", p)
	}
	env.set("DEVC_SESSION", session)
	env.set("DEVC_HARNESS", l.ID)
	env.set("DEVC_LAUNCH_DEPTH", fmt.Sprint(depth+1))

	if l.Delivery == DeliveryDaemon {
		addDaemonSecrets(l, env, e)
	} else {
		for _, s := range l.Secrets {
			if s.Env == "" {
				continue
			}
			v, err := readSecret(filepath.Join(e.Root, s.File))
			if err == nil {
				env.set(s.Env, v)
			} else if !errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(e.Stderr, "devc: cannot read the granted secret %s: %v\n", s.Name, err)
			}
		}
		if (l.Auth.Method == "env-token" || l.Auth.Method == "api-key") && l.Auth.Env != "" {
			v, err := readSecret(filepath.Join(e.Root, l.Auth.File))
			switch {
			case err == nil:
				env.set(l.Auth.Env, v)
			case errors.Is(err, fs.ErrNotExist):
				fmt.Fprintf(e.Stderr, "devc: no credential for %s in this box; run 'devc login %s %s' on the host, then 'devc up'\n", l.Display, l.Realm, l.ID)
			default:
				fmt.Fprintf(e.Stderr, "devc: cannot read the credential for %s: %v\n", l.Display, err)
			}
		}
	}

	if l.GitHubToken {
		addGitHubToken(l, env, e)
	}

	argv := append([]string{l.Bin}, l.Args...)
	argv = append(argv, userArgs...)
	// A harness that reports its status through OSC 7501 runs under the
	// status wrapper (work order C09), but only on a terminal: without one
	// it would get a pseudo-terminal it did not ask for (and its output
	// carriage returns).
	if l.Status.OSC7501 && env.get(NoPTYEnv) != "1" && e.IsTerminal != nil && e.IsTerminal() {
		helper := e.Helper
		if helper == "" {
			helper = tmux.Helper
		}
		wrapped := append([]string{"devc", "box", "pty", "--argv0", l.Bin, "--", l.Exe}, argv[1:]...)
		return &Prepared{Exe: helper, Argv: wrapped, Env: env.list(), Session: session}, nil
	}
	return &Prepared{Exe: l.Exe, Argv: argv, Env: env.list(), Session: session}, nil
}

// addDaemonSecrets asks the daemon for the credential and the granted
// secrets (Delivery "daemon"). Nothing is stored in the box. When the
// daemon cannot be reached the harness starts without them and the user is
// told how to fix it.
func addDaemonSecrets(l *Launch, env *envList, e LaunchEnv) {
	fetch := e.FetchSecret
	if fetch == nil {
		fetch = FetchSecret
	}
	unreachable := false
	get := func(name, what string) (string, bool) {
		if unreachable {
			return "", false
		}
		v, err := fetch(name)
		var pe *proto.Error
		switch {
		case err == nil:
			return v, true
		case errors.As(err, &pe) && pe.Code == proto.CodeNotFound:
			fmt.Fprintf(e.Stderr, "devc: %s is not set on the host: %s\n", what, proto.Printable(pe.Message))
		case errors.As(err, &pe) && pe.Code != proto.CodeHostUnavailable:
			fmt.Fprintf(e.Stderr, "devc: the host refused %s: %s\n", what, proto.Printable(pe.Message))
		default:
			unreachable = true
			fmt.Fprintf(e.Stderr, "devc: the devc daemon is not reachable from this box, so %s starts without its credential and granted secrets: "+
				"start it on the host ('devc daemon' or 'devc service install'), or set secrets.delivery = \"files\" in config.toml and restart the box\n", l.Display)
		}
		return "", false
	}
	if (l.Auth.Method == "env-token" || l.Auth.Method == "api-key") && l.Auth.Env != "" && l.Auth.Secret != "" {
		if v, ok := get(l.Auth.Secret, "the credential for "+l.Display); ok {
			env.set(l.Auth.Env, v)
		}
	}
	for _, s := range l.Secrets {
		if s.Env == "" {
			continue
		}
		if v, ok := get(s.Name, "the granted secret "+s.Name); ok {
			env.set(s.Env, v)
		}
	}
}

// GitHubTokenEnv is the variable the GitHub token is exported as (gh reads
// it; git uses the credential helper instead).
const GitHubTokenEnv = "GH_TOKEN"

// addGitHubToken asks the daemon for the project's GitHub token. A session
// keeps the token it started with; minted tokens last at most an hour.
func addGitHubToken(l *Launch, env *envList, e LaunchEnv) {
	fetch := e.FetchGitHubToken
	if fetch == nil {
		fetch = func() (proto.GitHubTokenReply, error) { return FetchGitHubToken(AgentAbstract) }
	}
	r, err := fetch()
	var pe *proto.Error
	switch {
	case err == nil:
		env.set(GitHubTokenEnv, r.Token)
	case errors.As(err, &pe) && pe.Code != proto.CodeHostUnavailable:
		fmt.Fprintf(e.Stderr, "devc: %s starts without a GitHub token: %s\n", l.Display, proto.Printable(pe.Message))
	default:
		fmt.Fprintf(e.Stderr, "devc: %s starts without a GitHub token: the devc daemon is not reachable from this box (start it on the host with 'devc daemon' or 'devc service install')\n", l.Display)
	}
}

// secretTimeout bounds one host.secret call at launch.
const secretTimeout = 10 * time.Second

// FetchSecret asks the daemon for a secret through the box agent.
func FetchSecret(name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), secretTimeout)
	defer cancel()
	var r proto.SecretReply
	if err := AgentCall(ctx, proto.MethodHostSecret, proto.SecretParams{Name: name}, &r); err != nil {
		return "", err
	}
	return strings.TrimRight(r.Value, "\r\n"), nil
}

// registerSession records the start of a session: session.start with the
// harness, the worktree, the working directory and the PID (which the
// harness keeps, because the launcher execs it). It is best effort: a
// harness must start even when the spool cannot be written.
func registerSession(l *Launch, p *Prepared, e LaunchEnv) error {
	getwd, now, pid := e.Getwd, e.Now, e.Pid
	if getwd == nil {
		getwd = os.Getwd
	}
	if now == nil {
		now = time.Now
	}
	if pid == 0 {
		pid = os.Getpid()
	}
	cwd, _ := getwd()
	if !path.IsAbs(cwd) || len(cwd) > 4096 {
		cwd = ""
	}
	env := newEnvList(p.Env)
	start := proto.SessionStart{Harness: l.ID, Worktree: worktreeOf(env.get("DEVC_WORKTREE"), cwd, l), Cwd: cwd, PID: pid}
	return AppendEvents(filepath.Join(e.Root, SpoolDir), p.Session, now(), []Payload{{proto.TypeSessionStart, start}})
}

// worktreeOf is the session's worktree: DEVC_WORKTREE when it is a valid
// name (set in windows that devc creates), else the first directory below
// the project directory that the working directory lies in, else "".
func worktreeOf(env, cwd string, l *Launch) string {
	if names.ValidKind(names.KindWorktree, env) {
		return env
	}
	proj := path.Join("/w", l.Realm, l.Project)
	rest, ok := strings.CutPrefix(cwd, proj+"/")
	if !ok {
		return ""
	}
	first, _, _ := strings.Cut(rest, "/")
	if !names.ValidKind(names.KindWorktree, first) || first == ".repo" || first == ".devc" {
		return ""
	}
	return first
}

// RunLaunch prepares and execs the harness; it returns only on failure.
func RunLaunch(l *Launch, userArgs []string, e LaunchEnv) error {
	p, err := PrepareLaunch(l, userArgs, e)
	if err != nil {
		return err
	}
	_ = registerSession(l, p, e)
	err = syscall.Exec(p.Exe, p.Argv, p.Env)
	return fmt.Errorf("starting %s (%s): %w", l.Display, p.Exe, err)
}

// notSelf refuses an executable that resolves to the running devc binary:
// the shim would call itself.
func notSelf(exe, self string) error {
	fi, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("executable %s: %w (reinstall with 'devc harness install' on the host)", exe, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", exe)
	}
	if self == "" {
		return nil
	}
	if si, err := os.Stat(self); err == nil && os.SameFile(fi, si) {
		return fmt.Errorf("the executable %s resolves to devc itself, which would call itself; check install.exe in the manifest", exe)
	}
	return nil
}

// readSecret reads a delivered secret (a small regular file).
func readSecret(p string) (string, error) {
	b, err := readCapped(p, 64<<10)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// envList is an environment with set semantics that keeps the order of
// first appearance.
type envList struct {
	keys []string
	vals map[string]string
}

func newEnvList(environ []string) *envList {
	e := &envList{vals: map[string]string{}}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			e.set(k, v)
		}
	}
	return e
}

func (e *envList) get(k string) string { return e.vals[k] }

func (e *envList) set(k, v string) {
	if _, ok := e.vals[k]; !ok {
		e.keys = append(e.keys, k)
	}
	e.vals[k] = v
}

func (e *envList) list() []string {
	out := make([]string, 0, len(e.keys))
	for _, k := range e.keys {
		out = append(out, k+"="+e.vals[k])
	}
	return out
}

// ApplySeeds writes the rendered seed files of every harness: "missing"
// seeds only where the target does not exist, "always" seeds every time.
// It runs as dev at box init, in dev's own home.
func ApplySeeds(root string) []error {
	ls, err := ListLaunches(root)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, l := range ls {
		for _, s := range l.Seeds {
			target := filepath.Join(root, s.Target)
			if s.When == "missing" {
				if _, err := os.Lstat(target); err == nil {
					continue
				}
			}
			b, err := readCapped(filepath.Join(root, s.Source), maxDescription)
			if err != nil {
				errs = append(errs, fmt.Errorf("seed %s: %w", s.Target, err))
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				errs = append(errs, fmt.Errorf("seed %s: %w", s.Target, err))
				continue
			}
			tmp := target + ".devc-seed"
			_ = os.Remove(tmp)
			if err := os.WriteFile(tmp, b, 0o600); err != nil {
				errs = append(errs, fmt.Errorf("seed %s: %w", s.Target, err))
				continue
			}
			if err := os.Rename(tmp, target); err != nil {
				_ = os.Remove(tmp)
				errs = append(errs, fmt.Errorf("seed %s: %w", s.Target, err))
			}
		}
	}
	return errs
}
