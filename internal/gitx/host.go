// Package gitx moves code between a project's box repository, its relay
// and its upstream (work order C05, spec section 9): the relay, banking,
// the relay service's policy and records for a box's own pushes, pull,
// push, log and diff, project initialisation and adoption, the host side
// of worktrees, the changes view and the relay's part of the archive.
//
// Two rules shape it (README rule 2):
//
//   - Host git on project code runs only against the relay, a bare
//     repository in the state directory whose configuration and hooks the
//     host controls. Every host git command on a relay goes through
//     Host.Cmd, which sets --git-dir to the relay, "/" as working directory
//     and core.hooksPath to an empty directory owned by devc (only the
//     relay service's receive-pack gets devc's relay hooks instead). Git
//     never runs on the host with a box-writable directory as repository,
//     working tree or current directory.
//   - Git inside the box runs through `podman exec` as dev. When the host
//     fetches from or pushes to the box repository, the transport is git's
//     ext:: helper running `podman exec … git-upload-pack|git-receive-pack`
//     in the box, so whatever hooks and configuration the box repository
//     holds execute there.
//
// Everything that comes back from a box (ref names, snapshot reports,
// worktree listings, commit messages shown to the user) is hostile input:
// names are validated before they reach a ref or a message, reports are
// decoded strictly, and text shown on a terminal is made printable.
//
// Files: host.go (Host, the only runner of host git on relays, and the
// box transport's URL); relay.go (the relay repository, its configuration
// and ref transactions); policy.go (the ref namespaces and the pure bank
// policy); box.go (Service, and everything that reaches into the box);
// bank.go (banking and forwarding); relayservice.go (the push policy, the
// hooks' records and the storage checks of the relay service); upstream.go
// (pull, push, log, diff); project.go (project init and adoption);
// worktree.go; changes.go (the web app's changes view); github.go (the
// box's GitHub remote); archive.go (bundles and restores of a relay).
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mb706/devc/internal/execx"
)

// Mode selects the environment a host git command runs in.
type Mode int

const (
	// Internal commands work on the relay or talk to a box. They see no
	// global or system git configuration and no GIT_* variable from the
	// caller, never prompt, and run in the C locale because their output is
	// parsed.
	Internal Mode = iota
	// Upstream commands talk to the project's upstream. The user's global
	// and system configuration stays in effect (it carries credentials);
	// they are attached to the terminal when there is one, otherwise they
	// run with GIT_TERMINAL_PROMPT=0.
	Upstream
	// Display commands produce output for the user (log, diff). Their
	// configuration is isolated as for Internal; the output is captured and
	// made safe for a terminal before it is shown.
	Display
)

// Call is one host git invocation.
type Call struct {
	// Relay is the relay repository (--git-dir); it must be absolute.
	Relay string
	Args  []string
	Mode  Mode
	// Box marks a command that reaches the box through the ext:: transport:
	// only that invocation gets protocol.ext.allow=always, and every other
	// protocol is switched off for it.
	Box     bool
	Stdin   io.Reader
	Timeout time.Duration
	// MaxOutput overrides the capture cap.
	MaxOutput int
	// HooksDir overrides Host.HooksDir for this call: only the relay
	// service's receive-pack runs with devc's own relay hooks.
	HooksDir string
	// Env is added to the command's environment after the mode's.
	Env []string
	// Config holds key=value settings given as -c options for this call.
	Config []string
	// Group runs the command in a process group of its own that is ended
	// and waited for as a whole (execx.Cmd.KillGroup), also on a host that
	// is not Background: a bank's fetch, which its watchdog may end while
	// index-pack writes (C20a, as for pushes in finding E1-16).
	Group bool
}

// Host runs git on the host. Apart from `devc init`'s clone of the
// configuration repository and doctor's version check, it is the only way
// devc runs host git, so every git command on a relay gets the settings
// of Cmd.
type Host struct {
	Runner execx.Runner
	// Bin is the git executable (default "git").
	Bin string
	// HooksDir is the empty directory used as core.hooksPath.
	HooksDir string
	// Terminal says that upstream commands may attach to the terminal.
	Terminal bool
	// Stderr receives the progress and prompts of attached upstream commands.
	Stderr io.Writer
	// Init runs once before the first command (it creates HooksDir).
	Init func() error
	// Background makes upstream commands non-interactive whatever the
	// terminal: no prompts from git, no askpass programs, and a new session
	// without a controlling terminal, so ssh fails instead of asking (the
	// daemon's fetch-through and the relay hook's forwards).
	Background bool

	once    sync.Once
	initErr error
}

// Timeouts of host git commands.
const (
	defaultTimeout  = 2 * time.Minute
	transferTimeout = 30 * time.Minute
	upstreamTimeout = time.Hour
)

// repoEnv are variables that point git at another repository, working tree
// or configuration. They are removed even for upstream commands, whose
// other user settings (ssh command, askpass) stay.
var repoEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
	"GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_*", "GIT_CONFIG_VALUE_*",
	"GIT_EXTERNAL_DIFF", "GIT_DIFF_OPTS", "GIT_QUARANTINE_PATH", "GIT_REPLACE_REF_BASE", "GIT_NO_REPLACE_OBJECTS",
	"GIT_PROTOCOL", "GIT_EXEC_PATH", "GIT_TEMPLATE_DIR", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE",
}

// diffing subcommands always get --no-ext-diff --no-textconv, so no diff
// driver or text conversion configured anywhere runs on box content.
var diffing = map[string]bool{"log": true, "diff": true, "show": true}

// Cmd builds the command for c: the relay as --git-dir, devc's hooks
// directory, no fsmonitor, no replacement objects, the mode's environment
// and "/" as working directory. Run and Serve use it; it is exported for
// tests, which assert on every argument and the environment.
func (h *Host) Cmd(c Call) (execx.Cmd, error) {
	if !filepath.IsAbs(c.Relay) || filepath.Clean(c.Relay) != c.Relay {
		return execx.Cmd{}, fmt.Errorf("git: relay %q is not a clean absolute path", c.Relay)
	}
	hooks := h.HooksDir
	if c.HooksDir != "" {
		hooks = c.HooksDir
	}
	if !filepath.IsAbs(hooks) {
		return execx.Cmd{}, fmt.Errorf("git: hooks directory %q is not absolute", hooks)
	}
	if len(c.Args) == 0 || strings.HasPrefix(c.Args[0], "-") {
		return execx.Cmd{}, errors.New("git: no subcommand")
	}
	bin := h.Bin
	if bin == "" {
		bin = "git"
	}
	// Replacement objects are never interpreted (finding D1-07): a
	// refs/replace/ ref would change how existing history reads. git
	// passes the option on to the git processes it starts.
	args := []string{
		"--git-dir=" + c.Relay,
		"-c", "core.hooksPath=" + hooks,
		"-c", "core.fsmonitor=false",
		"--no-replace-objects",
	}
	if c.Box {
		// What a box sends stays in its pack (finding D1-05), also for
		// relays configured before that setting existed.
		args = append(args, "-c", "protocol.allow=never", "-c", "protocol.ext.allow=always",
			"-c", "fetch.unpackLimit=1", "-c", "transfer.unpackLimit=1")
	}
	for _, kv := range c.Config {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" || strings.HasPrefix(kv, "-") || strings.ContainsAny(kv, "\x00\n") {
			return execx.Cmd{}, fmt.Errorf("git: invalid setting %q", kv)
		}
		args = append(args, "-c", kv)
	}
	args = append(args, c.Args[0])
	if diffing[c.Args[0]] {
		args = append(args, "--no-ext-diff", "--no-textconv")
	}
	args = append(args, c.Args[1:]...)
	cmd := execx.Cmd{Name: bin, Args: args, Dir: "/", Stdin: c.Stdin, Timeout: c.Timeout, MaxOutput: c.MaxOutput}
	switch c.Mode {
	case Upstream:
		cmd.Unset = repoEnv
		switch {
		case h.Background:
			cmd.Unset = append(append([]string{}, repoEnv...), backgroundUnset...)
			cmd.Env = []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"}
			cmd.Setsid = true
		case !h.Terminal:
			cmd.Env = []string{"GIT_TERMINAL_PROMPT=0"}
		}
		if cmd.Timeout == 0 {
			cmd.Timeout = upstreamTimeout
		}
	default:
		cmd.Unset = []string{"GIT_*"}
		cmd.Env = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
		if c.Mode == Internal {
			cmd.Env = append(cmd.Env, "LC_ALL=C")
		}
	}
	if cmd.Timeout == 0 {
		cmd.Timeout = defaultTimeout
		if c.Box {
			cmd.Timeout = transferTimeout
		}
	}
	cmd.Env = append(cmd.Env, c.Env...)
	cmd.Env = append(cmd.Env, "GIT_NO_REPLACE_OBJECTS=1")
	// A background host (the daemon) ends what git started with git: an
	// index-pack or pack-objects that outlived an ended fetch would go on
	// writing into the relay (finding E1-16). Commands of the command line
	// stay in the terminal's foreground group, which an interrupt ends.
	cmd.KillGroup = h.Background || c.Group
	return cmd, nil
}

// backgroundUnset are the variables that would let a background upstream
// command ask someone: graphical askpass programs and the display they use.
var backgroundUnset = []string{"GIT_ASKPASS", "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "DISPLAY", "WAYLAND_DISPLAY"}

// Run runs c and captures its standard output. An upstream command with a
// terminal is attached to it (prompts reach the user) and only its standard
// output is captured.
func (h *Host) Run(ctx context.Context, c Call) (execx.Result, error) {
	if h.Init != nil {
		h.once.Do(func() { h.initErr = h.Init() })
		if h.initErr != nil {
			return execx.Result{}, h.initErr
		}
	}
	cmd, err := h.Cmd(c)
	if err != nil {
		return execx.Result{}, err
	}
	if c.Mode != Upstream || !h.Terminal || h.Background {
		return h.Runner.Run(ctx, cmd)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = h.Stderr
	code, err := h.Runner.Interactive(ctx, cmd)
	res := execx.Result{Stdout: out.Bytes(), ExitCode: code}
	if err != nil {
		return res, err
	}
	if code != 0 {
		return res, &GitError{Cmd: cmd.String(), Code: code}
	}
	return res, nil
}

// Serve runs c with its standard output and error streamed to stdout and
// stderr instead of captured (the relay service's upload-pack and
// receive-pack, whose standard input and output are a box's stream). It
// runs in a new session without a controlling terminal and returns the exit
// status; a non-zero exit is not an error.
func (h *Host) Serve(ctx context.Context, c Call, stdout, stderr io.Writer) (int, error) {
	if h.Init != nil {
		h.once.Do(func() { h.initErr = h.Init() })
		if h.initErr != nil {
			return -1, h.initErr
		}
	}
	cmd, err := h.Cmd(c)
	if err != nil {
		return -1, err
	}
	// The whole session ends with the service and is gone before Serve
	// returns: the caller removes what an ended push left in the relay and
	// releases the relay's lock only then (finding E1-16).
	cmd.Stdout, cmd.Stderr, cmd.Setsid, cmd.KillGroup = stdout, stderr, true, true
	return h.Runner.Interactive(ctx, cmd)
}

// GitError is a failed attached git command; its messages went to the
// terminal.
type GitError struct {
	Cmd  string
	Code int
}

func (e *GitError) Error() string {
	return fmt.Sprintf("%s: exit status %d (see the messages above)", e.Cmd, e.Code)
}

// extEscape quotes a word for git's ext:: transport, where a space
// separates arguments and "%" starts a placeholder.
func extEscape(s string) string {
	return strings.NewReplacer("%", "%%", " ", "% ").Replace(s)
}

// BoxURL is the ext:: remote that reaches a box repository: the engine
// runs git-upload-pack or git-receive-pack (%S) as dev in the container,
// on the repository's in-box git directory.
func BoxURL(engineBin, container, repoDir string) (string, error) {
	if engineBin == "" || strings.ContainsAny(engineBin, "\x00\n\r\t") {
		return "", fmt.Errorf("invalid engine command %q", engineBin)
	}
	if strings.HasPrefix(container, "-") || strings.ContainsAny(container, " %\x00\n") || !strings.HasPrefix(repoDir, "/") || strings.ContainsAny(repoDir, " %\x00\n") {
		return "", fmt.Errorf("invalid box transport target %q %q", container, repoDir)
	}
	return "ext::" + extEscape(engineBin) + " exec -i -u 1000:1000 " + container + " %S " + repoDir, nil
}
