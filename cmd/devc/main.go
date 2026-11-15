// Command devc is the single devc binary. It dispatches on the name it was
// invoked under: "devc" runs the CLI; "git-remote-devc" is the git remote
// helper (filled in by a later chunk); any other name is a harness shim in a
// box (/opt/devc/shims/<bin> links to devc) and runs the launcher.
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

type role int

const (
	roleCLI role = iota
	roleRemoteHelper
	roleShim
)

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

// launcher runs a harness shim; tests replace it.
type launcher interface {
	launch(name string, args []string) int
}

func dispatch(name string, args []string, l launcher) int {
	switch route(name) {
	case roleCLI:
		return cli.Run()
	case roleRemoteHelper:
		fmt.Fprintln(os.Stderr, "git-remote-devc: not implemented")
		return 2
	}
	return l.launch(name, args)
}

// shim is the production launcher: a name that matches a launch
// description in /etc/devc/harness runs that harness.
type shim struct {
	root   string
	stderr io.Writer
}

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
	err = boxside.RunLaunch(l, args, boxside.LaunchEnv{Root: s.root, Environ: os.Environ(), Self: self, Stderr: s.stderr, NewSession: names.NewSessionID})
	fmt.Fprintf(s.stderr, "devc: %v\n", err)
	return 126
}
