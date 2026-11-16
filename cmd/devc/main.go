// Command devc is the single devc binary. It dispatches on the name it was
// invoked under: "devc" (and "devc-*") runs the CLI; "git-remote-devc" is
// the git remote helper for devc:: remotes in boxes; pre-receive, update
// and post-receive with DEVC_RELAY_PROJECT set are the relay service's
// hooks; any other name is a harness shim in a box (/opt/devc/shims/<bin>
// links to devc) and runs the launcher.
//
// Security rules: a shim acts only inside a box (DEVC_BOX=1), and the names
// that select the CLI or the remote helper can never be a harness's shim
// name (internal/config refuses such manifests).
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/cli"
	"github.com/mb706/devc/internal/names"
)

func main() {
	os.Exit(dispatch(filepath.Base(os.Args[0]), os.Args[1:], shim{root: "/", stderr: os.Stderr}))
}

// role is what the binary does, decided by route from its name.
type role int

const (
	roleCLI role = iota
	roleRemoteHelper
	roleShim
)

// route maps the base name of argv[0] to a role; the relay hooks are told
// apart from shims later, by dispatch, because they also need the
// environment.
func route(name string) role {
	switch {
	case name == "devc" || strings.HasPrefix(name, "devc-"):
		// devc-linux-amd64 and similar release names run the CLI too.
		return roleCLI
	case name == "git-remote-devc":
		return roleRemoteHelper
	}
	return roleShim
}

// launcher runs a harness shim, the remote helper or a relay hook; tests
// replace it.
type launcher interface {
	launch(name string, args []string) int
	remoteHelper(args []string) int
	relayHook(name string, args []string) int
}

// relayHook reports whether devc runs as one of the relay service's hooks:
// the hooks directory holds links named after the hooks, and the daemon
// sets the project for receive-pack (work order C10a).
func relayHook(name string, getenv func(string) string) bool {
	return (name == "pre-receive" || name == "update" || name == "post-receive") && getenv("DEVC_RELAY_PROJECT") != ""
}

// dispatch runs the role the name selects and returns the exit status.
func dispatch(name string, args []string, l launcher) int {
	switch route(name) {
	case roleCLI:
		return cli.Run()
	case roleRemoteHelper:
		return l.remoteHelper(args)
	}
	if relayHook(name, os.Getenv) {
		return l.relayHook(name, args)
	}
	return l.launch(name, args)
}

// shim is the production launcher: a name that matches a launch
// description in /etc/devc/harness runs that harness.
type shim struct {
	root   string
	stderr io.Writer
}

// remoteHelper serves git's remote-helper protocol on stdin and stdout
// (git runs it in the box for devc:: URLs): through the daemon when it is
// connected, else the C05 stub.
func (s shim) remoteHelper(args []string) int {
	return boxside.RemoteHelperMain(args, os.Stdin, os.Stdout, s.stderr, boxside.ExecGit, boxside.DefaultRelayDialer())
}

// relayHook runs `devc relay-hook <name> args…`.
func (s shim) relayHook(name string, args []string) int {
	return cli.Main(append([]string{"relay-hook", name}, args...), os.Stdout, os.Stderr, os.Getenv)
}

// launch finds the launch description whose bin is name and execs the
// harness through the launcher. It returns only on failure: 127 when there
// is no such harness here, 126 when the launcher could not exec it.
func (s shim) launch(name string, args []string) int {
	if os.Getenv("DEVC_BOX") != "1" {
		fmt.Fprintf(s.stderr, "devc: invoked as %q: harness shims work only inside a devc box\n", name)
		return 127
	}
	l, err := boxside.FindByBin(s.root, name)
	if err != nil {
		fmt.Fprintf(s.stderr, "devc: %s: %v\n", name, err)
		return 127
	}
	if l == nil {
		fmt.Fprintf(s.stderr, "devc: %s is not available in this box: list its harness in harnesses in project.toml, install it with 'devc harness install', and run 'devc up' on the host\n", name)
		return 127
	}
	self, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	err = boxside.RunLaunch(l, args, boxside.LaunchEnv{Root: s.root, Environ: os.Environ(), Self: self, Stderr: s.stderr, NewSession: names.NewSessionID, IsTerminal: boxside.StdioIsTerminal})
	fmt.Fprintf(s.stderr, "devc: %v\n", err)
	return 126
}
