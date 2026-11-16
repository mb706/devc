// Package tmux builds every tmux command line devc uses (plan 3.5): the
// in-box session start, the pane-died hook, the listing used for
// reconciliation, and the attach command run from the host. Nothing else
// in devc assembles a tmux command.
//
// Model: one tmux server per box on the socket "devc" with devc's own
// configuration file, one tmux session "devc", one window per devc session
// named <worktree>/<harness> and tagged with the window option
// @devc_session=<id>. Every attaching client gets its own grouped session
// that is destroyed when the client detaches, so two clients can look at
// different windows.
//
// Security rules: commands are argument vectors (README rule 3). tmux
// reads an argument that ends in ";" as a command separator, so arguments
// that are not devc's own constants are escaped (Arg). Window names,
// directories and environment values are validated names and paths that
// contain no format characters. The tmux server's environment is cleaned of
// devc's per-session variables (Env), so a window never inherits another
// session's identity.
package tmux

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Fixed paths and names in the box.
const (
	Binary       = "/opt/devc/bin/tmux"
	Socket       = "devc"
	ConfigPath   = "/etc/devc/tmux.conf"
	SessionName  = "devc"
	TerminfoDirs = "/opt/devc/terminfo:"
	// SessionOption is the window option that carries the devc session ID.
	SessionOption = "@devc_session"
	// Helper is the in-box devc binary the pane-died hook runs.
	Helper = "/opt/devc/bin/devc"
)

var (
	windowIDRE = regexp.MustCompile(`^@[0-9]{1,9}$`)
	paneIDRE   = regexp.MustCompile(`^%[0-9]{1,9}$`)
	clientRE   = regexp.MustCompile(`^devc-[0-9a-f]{4,32}$`)
	sessionRE  = regexp.MustCompile(`^s[0-9a-v]{20}$`)
	nameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// ValidWindowID reports whether s is a tmux window ID ("@3").
func ValidWindowID(s string) bool { return windowIDRE.MatchString(s) }

// ValidPaneID reports whether s is a tmux pane ID ("%3").
func ValidPaneID(s string) bool { return paneIDRE.MatchString(s) }

// ValidClientName reports whether s is a name ClientName could return.
func ValidClientName(s string) bool { return clientRE.MatchString(s) }

// base is the start of every invocation.
func base() []string {
	return []string{"-L", Socket, "-f", ConfigPath}
}

// Argv is the full argument vector (binary first) for args.
func Argv(args []string) []string {
	return append([]string{Binary}, args...)
}

// Arg escapes one argument that tmux must take literally: a trailing ";"
// would end the command, and a trailing "\;" would become ";".
func Arg(s string) string {
	if strings.HasSuffix(s, ";") {
		return s[:len(s)-1] + `\;`
	}
	return s
}

// sep is tmux's command separator in an argument vector.
const sep = ";"

// Window describes a window to create for a devc session.
type Window struct {
	Session  string // devc session ID
	Harness  string
	Worktree string
	Dir      string // working directory in the box
	// Command is the argument vector of the pane's process; it must have
	// at least two elements, so tmux executes it directly instead of
	// through a shell.
	Command []string
}

// Name is the window name <worktree>/<harness>.
func (w Window) Name() string { return w.Worktree + "/" + w.Harness }

func (w Window) check() error {
	switch {
	case !sessionRE.MatchString(w.Session):
		return fmt.Errorf("invalid session ID %q", w.Session)
	case !nameRE.MatchString(w.Harness) || !nameRE.MatchString(w.Worktree):
		return fmt.Errorf("invalid harness %q or worktree %q", w.Harness, w.Worktree)
	case !path.IsAbs(w.Dir) || path.Clean(w.Dir) != w.Dir || strings.ContainsAny(w.Dir, "#\x00\n"):
		return fmt.Errorf("invalid directory %q", w.Dir)
	case len(w.Command) < 2:
		return fmt.Errorf("the window command needs at least two arguments")
	}
	return nil
}

// createdFormat is what new-session and new-window print: the window and
// pane IDs.
const createdFormat = "#{window_id} #{pane_id}"

func (w Window) common() []string {
	return []string{
		"-n", w.Name(), "-c", w.Dir,
		"-e", "DEVC_SESSION=" + w.Session, "-e", "DEVC_WORKTREE=" + w.Worktree,
		"-P", "-F", createdFormat,
	}
}

func (w Window) command() []string {
	out := []string{"--"}
	for _, a := range w.Command {
		out = append(out, Arg(a))
	}
	return out
}

// NewSession creates the tmux session "devc" (and with it the server)
// with w as its first window.
func NewSession(w Window) ([]string, error) {
	if err := w.check(); err != nil {
		return nil, err
	}
	a := append(base(), "new-session", "-d", "-s", SessionName)
	a = append(a, w.common()...)
	return append(a, w.command()...), nil
}

// NewWindow adds w to the existing session "devc".
func NewWindow(w Window) ([]string, error) {
	if err := w.check(); err != nil {
		return nil, err
	}
	a := append(base(), "new-window", "-d", "-t", "="+SessionName+":")
	a = append(a, w.common()...)
	return append(a, w.command()...), nil
}

// ParseCreated parses the output of NewSession or NewWindow.
func ParseCreated(out string) (window, pane string, err error) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 || !ValidWindowID(f[0]) || !ValidPaneID(f[1]) {
		return "", "", fmt.Errorf("unexpected tmux output %q", strings.TrimSpace(out))
	}
	return f[0], f[1], nil
}

// PaneDiedHook is the command tmux runs when a pane's process ends (with
// remain-on-exit on, so the pane and its exit status still exist): devc
// records the end of the session and then removes the pane. run-shell
// expands the formats and hands the result to sh; every value is quoted for
// the shell with the q: modifier, because the box can set the session
// option to anything. The helper validates all values again.
func PaneDiedHook() string {
	return `run-shell -b "` + Helper + ` box session ended --pane=#{q:pane_id} --session=#{q:` + SessionOption +
		`} --status=#{q:pane_dead_status} --signal=#{q:pane_dead_signal}"`
}

// TagWindow marks a new window as the window of session sid: the session
// option, remain-on-exit and the pane-died hook (the configuration sets the
// last two globally too; repeating them per window protects devc's windows
// from a user configuration that changes the global values).
func TagWindow(window, sid string) ([]string, error) {
	if !ValidWindowID(window) || !sessionRE.MatchString(sid) {
		return nil, fmt.Errorf("invalid window %q or session %q", window, sid)
	}
	a := append(base(), "set-option", "-w", "-t", window, SessionOption, sid, sep,
		"set-option", "-w", "-t", window, "remain-on-exit", "on", sep,
		"set-hook", "-w", "-t", window, "pane-died", PaneDiedHook())
	return a, nil
}

// Attach attaches a new client through its own grouped session named
// client, destroyed when the client detaches, and selects window (a window
// ID, or "" for the session's current window).
func Attach(client, window string) ([]string, error) {
	if !ValidClientName(client) || (window != "" && !ValidWindowID(window)) {
		return nil, fmt.Errorf("invalid client %q or window %q", client, window)
	}
	a := append(base(), "new-session", "-t", "="+SessionName, "-s", client, sep,
		// The trailing colon makes the target a session: set-option
		// resolves a bare name as a pane or window first.
		"set-option", "-t", "="+client+":", "destroy-unattached", "on")
	if window != "" {
		a = append(a, sep, "select-window", "-t", "="+client+":"+window)
	}
	return a, nil
}

// Observe attaches a client for the web app's terminal (work order C12
// section 3) through its own grouped session named client, showing window.
// The client never resizes the window (ignore-size). Its session has no
// status line, so the client's size is exactly the window's; in control
// mode it has no prefix key either, so every key reaches the pane and none
// switches windows. -u: the client's output is UTF-8 whatever the locale
// of the exec.
//
// An observing client is not tmux's read-only client: tmux refuses
// send-keys from any command line while a read-only client is the best
// client of the most recently active session ("client is read-only"), so
// a prompt typed for the session while someone observed it was pasted but
// never submitted, and stayed in front of the next one (the flaky
// TestWebApp step, found in C19r4). Observing is enforced by the daemon
// instead, which is the only writer of the client's terminal and refuses
// input to an observing stream.
func Observe(client, window string, control bool) ([]string, error) {
	if !ValidClientName(client) || !ValidWindowID(window) {
		return nil, fmt.Errorf("invalid client %q or window %q", client, window)
	}
	flags := "ignore-size"
	target := "=" + client + ":"
	a := append([]string{"-u"}, base()...)
	a = append(a, "new-session", "-t", "="+SessionName, "-s", client, "-f", flags, sep,
		"set-option", "-t", target, "destroy-unattached", "on", sep,
		"set-option", "-t", target, "status", "off")
	if control {
		a = append(a, sep, "set-option", "-t", target, "prefix", "None", sep,
			"set-option", "-t", target, "prefix2", "None")
	}
	return append(a, sep, "select-window", "-t", target+window), nil
}

// KillSession removes an observing client's grouped session (and with it
// the client).
func KillSession(client string) ([]string, error) {
	if !ValidClientName(client) {
		return nil, fmt.Errorf("invalid client %q", client)
	}
	return append(base(), "kill-session", "-t", "="+client), nil
}

// WindowSize prints a window's width and height.
func WindowSize(window string) ([]string, error) {
	if !ValidWindowID(window) {
		return nil, fmt.Errorf("invalid window %q", window)
	}
	return append(base(), "display-message", "-p", "-t", window, "#{window_width} #{window_height}"), nil
}

// ParseWindowSize parses the output of WindowSize.
func ParseWindowSize(out string) (cols, rows int, err error) {
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected window size %q", out)
	}
	cols, err1 := strconv.Atoi(f[0])
	rows, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || cols <= 0 || rows <= 0 {
		return 0, 0, fmt.Errorf("unexpected window size %q", out)
	}
	return cols, rows, nil
}

// KillPane removes a pane.
func KillPane(pane string) ([]string, error) {
	if !ValidPaneID(pane) {
		return nil, fmt.Errorf("invalid pane %q", pane)
	}
	return append(base(), "kill-pane", "-t", pane), nil
}

// KillWindow removes a window and the processes in it.
func KillWindow(window string) ([]string, error) {
	if !ValidWindowID(window) {
		return nil, fmt.Errorf("invalid window %q", window)
	}
	return append(base(), "kill-window", "-t", window), nil
}

// panesFormat lists one pane per line, fields separated by spaces (tmux
// replaces tabs in format output); the session option comes last because
// the box can set it to anything.
const panesFormat = "#{window_id} #{pane_id} #{pane_pid} #{pane_dead} #{" + SessionOption + "}"

// ListPanes lists every pane of the server.
func ListPanes() []string {
	return append(base(), "list-panes", "-a", "-F", panesFormat)
}

// Pane is one line of ListPanes.
type Pane struct {
	Window  string
	Pane    string
	PID     int
	Dead    bool
	Session string // "" when the window carries no valid session ID
}

// ParsePanes parses the output of ListPanes. Panes appear once per session
// that links their window (grouped sessions); duplicates are dropped.
// Malformed lines are skipped.
func ParsePanes(out string) []Pane {
	var ps []Pane
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.SplitN(l, " ", 5)
		if len(f) != 5 || !ValidWindowID(f[0]) || !ValidPaneID(f[1]) || seen[f[1]] {
			continue
		}
		pid, err := strconv.Atoi(f[2])
		if err != nil || pid < 0 {
			continue
		}
		p := Pane{Window: f[0], Pane: f[1], PID: pid, Dead: f[3] == "1"}
		if sessionRE.MatchString(f[4]) {
			p.Session = f[4]
		}
		seen[f[1]] = true
		ps = append(ps, p)
	}
	return ps
}

// clientsFormat lists one client per line: its last activity (Unix
// seconds) and the window its session shows.
const clientsFormat = "#{client_activity} #{window_id}"

// ListClients lists the clients attached to the server.
func ListClients() []string {
	return append(base(), "list-clients", "-F", clientsFormat)
}

// Client is one line of ListClients.
type Client struct {
	Activity int64
	Window   string
}

// ParseClients parses the output of ListClients; malformed lines are
// skipped.
func ParseClients(out string) []Client {
	var cs []Client
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || !ValidWindowID(f[1]) {
			continue
		}
		a, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil || a < 0 {
			continue
		}
		cs = append(cs, Client{Activity: a, Window: f[1]})
	}
	return cs
}

var bufferRE = regexp.MustCompile(`^devc-in-[0-9a-f]{8,32}$`)

// LoadBuffer loads standard input into the paste buffer name (a name
// devc generated).
func LoadBuffer(name string) []string {
	return append(base(), "load-buffer", "-b", name, "-")
}

// PasteBuffer pastes buffer name into pane as a bracketed paste (when the
// program asked for bracketed paste mode), keeping newlines as they are
// (-r) and deleting the buffer afterwards.
func PasteBuffer(name, pane string) ([]string, error) {
	if !bufferRE.MatchString(name) || !ValidPaneID(pane) {
		return nil, fmt.Errorf("invalid buffer %q or pane %q", name, pane)
	}
	return append(base(), "paste-buffer", "-p", "-r", "-d", "-b", name, "-t", pane), nil
}

// DeleteBuffer removes buffer name (after a failed paste).
func DeleteBuffer(name string) []string {
	return append(base(), "delete-buffer", "-b", name)
}

var keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,31}$`)

// SendKeys sends named keys to pane. Key names are letters, digits and
// dashes, so none can be read as an option or a command separator.
func SendKeys(pane string, keys []string) ([]string, error) {
	if !ValidPaneID(pane) || len(keys) == 0 {
		return nil, fmt.Errorf("invalid pane %q or no keys", pane)
	}
	a := append(base(), "send-keys", "-t", pane)
	for _, k := range keys {
		if !keyRE.MatchString(k) {
			return nil, fmt.Errorf("invalid key %q", k)
		}
		a = append(a, k)
	}
	return a, nil
}

// NoServer reports whether tmux's error output says that the server or the
// session "devc" does not exist.
func NoServer(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, n := range []string{"no server running", "can't find session", "error connecting to", "session not found"} {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// DuplicateSession reports whether tmux's error output says that the
// session "devc" exists already.
func DuplicateSession(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "duplicate session")
}

// sessionVars are dropped from every tmux client's environment: a client
// that starts the server would otherwise put them into the global
// environment of every window.
var sessionVars = map[string]bool{
	"DEVC_SESSION": true, "DEVC_HARNESS": true, "DEVC_LAUNCH_DEPTH": true, "DEVC_WORKTREE": true, "DEVC_STATUS": true,
	"TMUX": true, "TMUX_PANE": true,
}

// Env returns environ without devc's per-session variables and with the
// toolbox's terminfo directory.
func Env(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || sessionVars[k] || k == "TERMINFO_DIRS" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TERMINFO_DIRS="+TerminfoDirs)
}

// AttachEnv lists the variables a host passes to the attaching client:
// names alone take the host's values.
func AttachEnv() []string {
	return []string{"TERM", "COLORTERM", "TERMINFO_DIRS=" + TerminfoDirs}
}

// Config is /etc/devc/tmux.conf. The settings the harness TUIs need come
// first, then remain-on-exit and the pane-died hook through which devc
// learns that a session's process ended, then the user's own file, which
// may override everything.
func Config() string {
	var b strings.Builder
	b.WriteString("# Generated by devc for the box's tmux server (socket \"devc\"). Do not edit:\n")
	b.WriteString("# it is rewritten at every 'devc up'. Put your own settings in ~/.tmux.conf.\n")
	for _, l := range []string{
		`set-option -g default-terminal "tmux-256color"`,
		`set-option -gw allow-passthrough on`,
		`set-option -s focus-events on`,
		`set-option -s extended-keys on`,
		`set-option -as terminal-features ",*:extkeys"`,
		`set-option -g mouse on`,
		`set-option -g history-limit 100000`,
		`set-option -s set-clipboard on`,
		`set-environment -g TERMINFO_DIRS "` + TerminfoDirs + `"`,
		`set-option -gw remain-on-exit on`,
		`set-hook -g pane-died '` + PaneDiedHook() + `'`,
		`source-file -q ~/.tmux.conf`,
	} {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}
