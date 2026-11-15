// Package boxside is everything the devc binary does inside a box, reached
// through the hidden `devc box …` commands (plan 3.3) and the harness shims:
// the container's main process (init, which also applies harness seeds),
// the identity fix run by `devc up`, the login shell used by `devc sh`, the
// harness launcher (which registers each session), the hook receiver that
// turns harness hooks into spool events, the session commands that create,
// list and stop tmux windows, a self-test of the hardening, and the git
// work that must happen in the box (snapshot commits, worktrees, the
// repository report, the worktree hook and the stub remote helper
// git-remote-devc), and the worktree links to the data root. Workers run
// outside boxes: rm-worker and the data workers (seal, trash, savepoints,
// restore) in the helper container, the context worker in the context
// container (internal/ctxrepo).
//
// Security rules: code here runs in the box and trusts nothing the box can
// write. It reads its own description only from /etc/devc, which the host
// renders and mounts read-only, and it never talks to the host except
// through files in /run/devc that the host reads with internal/safefs and
// through the output of commands the host runs, which the host validates.
// Events are validated here as the host would validate them, so a mapping
// bug shows up as a note rather than a refused line. The hook receiver
// never prints and never fails. tmux runs with argument vectors and an
// environment without per-session variables (internal/tmux). The context
// worker walks a box-written tree through safefs and never lets git look
// at it. The data workers resolve every path beneath the mounted project
// directory descriptor by descriptor (safefs.Dir) and never act on anything
// reached through a symlink. The git commands here run box git on the box's own repository;
// their JSON reports are decoded strictly and validated by the host.
package boxside

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Paths inside the box.
const (
	EtcDir       = "/etc/devc"
	InfoFile     = "/etc/devc/box.json"
	DotfilesDir  = "/etc/devc/dotfiles"
	RunDir       = "/run/devc"
	ReadyFile    = "ready"
	SpoolDirName = "spool"
)

// Info is /etc/devc/box.json, written by the host at every `devc up`.
type Info struct {
	Schema      int    `json:"schema"`
	Realm       string `json:"realm"`
	Project     string `json:"project"`
	BoxName     string `json:"box_name"`
	ProjectDir  string `json:"project_dir"`
	Home        string `json:"home"`
	Toolbox     string `json:"toolbox"`
	Runtime     string `json:"runtime"`
	Notes       string `json:"notes"`
	Memory      string `json:"memory"`
	Network     string `json:"network"`
	Shell       string `json:"shell"` // configured shell; empty means pick one
	DevcVersion string `json:"devc_version"`
	// HomeMounts are the top-level entries of the home that exist only as
	// mount points (.cache, harness homes); they do not make a home "used".
	HomeMounts []string `json:"home_mounts,omitempty"`
	// BranchPrefix is put before the name of a task worktree's branch;
	// DataRoot is the data directory beside main/, never a worktree name.
	BranchPrefix string `json:"branch_prefix,omitempty"`
	DataRoot     string `json:"data_root,omitempty"`
	// DataLinks is set when the project defines data zones: worktrees get
	// a `data` link to the data root.
	DataLinks bool `json:"data_links,omitempty"`
}

// InfoSchema is the current version of box.json.
const InfoSchema = 1

// ReadInfo reads box.json below root ("/" in a box).
func ReadInfo(root string) (*Info, error) {
	b, err := os.ReadFile(filepath.Join(root, InfoFile))
	if err != nil {
		return nil, err
	}
	var in Info
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", InfoFile, err)
	}
	if in.Schema != InfoSchema {
		return nil, fmt.Errorf("%s has schema %d, this devc understands %d: run 'devc up' on the host", InfoFile, in.Schema, InfoSchema)
	}
	return &in, nil
}

// shellCandidates are tried in order when no shell is configured.
var shellCandidates = []string{"zsh", "bash", "sh"}

// Conventional locations first: on merged-/usr systems /bin/bash and
// /usr/bin/bash are the same file, and /bin/bash is what users expect.
var shellDirs = []string{"/bin", "/usr/bin", "/usr/local/bin"}

// ChooseShell returns the configured shell if it is an executable file in
// the box, otherwise the first of zsh, bash and sh found, otherwise /bin/sh.
// root prefixes every path ("/" in a box).
func ChooseShell(root, configured string) string {
	exe := func(p string) bool {
		fi, err := os.Stat(filepath.Join(root, p))
		return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
	}
	if configured != "" {
		if filepath.IsAbs(configured) {
			if exe(configured) {
				return configured
			}
		} else {
			for _, d := range shellDirs {
				if p := filepath.Join(d, configured); exe(p) {
					return p
				}
			}
		}
	}
	for _, name := range shellCandidates {
		for _, d := range shellDirs {
			if p := filepath.Join(d, name); exe(p) {
				return p
			}
		}
	}
	return "/bin/sh"
}
