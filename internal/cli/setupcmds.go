// This file defines `devc init`, which sets up the configuration directory
// of a new machine, and `devc cursor` and `devc code`, which attach a
// desktop editor to a box.

package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/examples"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// nextSteps is printed after `devc init`.
const nextSteps = `Next steps:
  devc doctor                   check the host (devc doctor --fix prints the root commands it needs)
  devc toolbox build tmux       build the static tmux that sessions run in
  devc harness install claude   install a harness (claude, codex, or one of your own)
  devc env build <env>          build an image, for example: devc env build base
`

func newInitCmd(a *app) *cobra.Command {
	var example bool
	cmd := &cobra.Command{
		Use:   "init <git-url> | --example",
		Short: "Set up the configuration directory of a new machine",
		Long: "Clone your configuration repository into the configuration directory (default ~/.config/devc), or, with " +
			"--example, write devc's example configuration there. The directory must not exist or be empty. The clone " +
			"uses your own git configuration and credentials.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if example == (len(args) == 1) {
				return usageErrorf("give either a git URL or --example")
			}
			if !example {
				if err := config.CheckUpstream(args[0]); err != nil {
					return usageErrorf("%v", strings.Replace(err.Error(), "upstream", "git URL", 1))
				}
			}
			p, err := a.paths()
			if err != nil {
				return err
			}
			dir := p.ConfigDir()
			if err := requireEmptyDir(dir); err != nil {
				return err
			}
			if example {
				if err := writeExample(dir); err != nil {
					return fmt.Errorf("writing the example configuration: %w", err)
				}
				fmt.Fprintf(a.stdout, "wrote the example configuration to %s\n", dir)
			} else {
				url := args[0]
				if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
				defer cancel()
				// The user's repository, with the user's own git settings
				// and credentials; a terminal stays attached for prompts.
				code, err := a.runner.Interactive(ctx, execx.Cmd{Name: "git", Dir: "/", Stdout: a.stderr, Stderr: a.stderr,
					Args: []string{"-c", "protocol.ext.allow=never", "clone", "--", url, dir}})
				if err != nil {
					return err
				}
				if code != 0 {
					return fmt.Errorf("git clone failed with exit status %d", code)
				}
				fmt.Fprintf(a.stdout, "cloned %s into %s\n", url, dir)
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			if len(probs) > 0 {
				fmt.Fprintf(a.stdout, "the configuration has %d problem(s); see 'devc config check'\n", len(probs))
			} else {
				fmt.Fprintf(a.stdout, "configuration OK: %d realm(s), %d project(s), %d env(s)\n", len(c.Realms()), len(c.Projects("")), len(c.Envs()))
			}
			fmt.Fprint(a.stdout, "\n"+nextSteps)
			return nil
		},
	}
	cmd.Flags().BoolVar(&example, "example", false, "write the example configuration instead of cloning")
	return cmd
}

// requireEmptyDir refuses a configuration directory that has content.
func requireEmptyDir(dir string) error {
	des, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case len(des) > 0:
		return fmt.Errorf("the configuration directory %s is not empty: move it away, or pass --config-dir", dir)
	}
	return nil
}

// writeExample copies the embedded example configuration into dir.
func writeExample(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return fs.WalkDir(examples.Config, "config", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "config"), "/")
		if rel == "" {
			return nil
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.Mkdir(target, 0o755)
		}
		b, err := examples.Config.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// editorLong is the help text shared by `devc cursor` and `devc code`.
const editorLong = "Start the box if needed and open %s attached to it, at the main checkout (the project directory when " +
	"there is none). The editor's CLI must be on PATH.\n\n" +
	"An attached editor is a deliberate hole in the sandbox: the editor installs and runs its server inside the box, " +
	"and that server talks to the editor on your desktop. Code in the box can act on the host through it (editor " +
	"extensions, terminals and tasks run with your rights there). Attach only to boxes whose code you trust as much " +
	"as code on your desktop."

// newEditorCmd returns `devc <bin>` for an editor with VS Code's attach
// convention; it warns at every use, because the editor's server in the
// box can act on the host through the editor.
func newEditorCmd(a *app, bin, display string) *cobra.Command {
	return &cobra.Command{
		Use:   bin + " [<realm>/<project>]",
		Short: "Open " + display + " attached to a box (weakens the sandbox)",
		Long:  fmt.Sprintf(editorLong, display),
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.boxEnv()
			if err != nil {
				return err
			}
			ref, err := a.resolveRef(e.cfg, args)
			if err != nil {
				return err
			}
			tool, err := execx.LookPath(bin)
			if err != nil {
				return fmt.Errorf("the %s command-line tool %q is not on PATH: install it from %s (its command palette offers \"Install '%s' command\")", display, bin, display, bin)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
			defer cancel()
			name, err := e.mgr.EnsureRunning(ctx, ref)
			if err != nil {
				return a.boxError(e.cfg, err)
			}
			uri := EditorURI(name, editorFolder(e.cfg.Paths, ref))
			fmt.Fprintf(a.stderr, "devc: warning: %s attached to %s can act on this machine through its server in the box\n", display, name)
			if _, err := a.runner.Run(ctx, execx.Cmd{Name: tool, Args: []string{"--folder-uri", uri}, Dir: "/", Timeout: 2 * time.Minute}); err != nil {
				return fmt.Errorf("starting %s: %w", display, err)
			}
			return nil
		},
	}
}

// EditorURI is the folder URI of an editor attached to a container (VS
// Code's Dev Containers convention, also used by Cursor).
func EditorURI(container, folder string) string {
	return "vscode-remote://attached-container+" + hex.EncodeToString([]byte(container)) + folder
}

// editorFolder is the in-box folder to open: main/ when it exists, else the
// project directory. Only an lstat of the host path is done.
func editorFolder(p *paths.Paths, ref names.ProjectRef) string {
	if fi, err := os.Lstat(filepath.Join(p.HostProjectDir(ref), "main")); err == nil && fi.IsDir() {
		return path.Join(paths.InBoxProjectDir(ref), "main")
	}
	return paths.InBoxProjectDir(ref)
}
