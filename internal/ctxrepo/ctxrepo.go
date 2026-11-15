// Package ctxrepo keeps each realm's context repository (work order C04
// section 6): durable/realms/<realm>/context/ holds notes/<project>/ and
// memory/<group>/, which boxes mount, and a bare git repository in .git,
// which no box ever sees.
//
// Security rules: the working tree is box-written (README rule 2), so the
// host never runs git on it. Every git operation runs in a throwaway
// container from the installer image with no network, no capabilities, no
// new privileges and the box seccomp profile, with only the context
// directory and the read-only toolbox mounted; inside, `devc box
// context-worker` walks the tree without following anything and feeds the
// accepted files to git fast-import. Output the worker prints (file names,
// diffs) is box-influenced and is made printable before it reaches a
// terminal.
package ctxrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/toolbox"
)

// Repo runs operations on context repositories.
type Repo struct {
	Paths   *paths.Paths
	Engine  engine.Engine
	Toolbox *toolbox.Toolbox
	// Executable and Version are synced into the toolbox, whose devc
	// binary the worker container runs.
	Executable string
	Version    string
	// SecurityOpts are the auxiliary containers' security options.
	SecurityOpts []string
	// ImageContext is the build context directory of the installer image.
	ImageContext string
	// Out receives progress messages (an image build).
	Out io.Writer
}

// workerTimeout bounds one worker container.
const workerTimeout = 10 * time.Minute

func (r *Repo) run(ctx context.Context, realm string, args ...string) (execx.Result, error) {
	if !names.ValidRealm(realm) {
		return execx.Result{}, fmt.Errorf("invalid realm name %q", realm)
	}
	dir := r.Paths.ContextDir(realm)
	if err := r.Paths.Ensure(dir); err != nil {
		return execx.Result{}, err
	}
	if err := r.Paths.Ensure(r.Toolbox.Dir); err != nil {
		return execx.Result{}, err
	}
	if _, err := r.Toolbox.Sync(r.Executable, r.Version); err != nil {
		return execx.Result{}, fmt.Errorf("syncing the toolbox: %w", err)
	}
	if err := toolbox.EnsureInstallerImage(ctx, r.Engine, r.ImageContext, r.Out); err != nil {
		return execx.Result{}, err
	}
	if err := r.Paths.Ensure(r.Paths.LocalDir()); err != nil {
		return execx.Result{}, err
	}
	unlock, err := flock.Acquire(ctx, filepath.Join(r.Paths.LocalDir(), "context-"+realm+".lock"), flock.Options{Timeout: 5 * time.Minute})
	if err != nil {
		return execx.Result{}, err
	}
	defer unlock()
	res, err := r.Engine.Run(ctx, engine.RunOptions{
		Image:        toolbox.InstallerImage,
		Labels:       map[string]string{box.LabelRealm: realm, box.LabelRole: "aux", "devc.aux": "context"},
		Network:      "none",
		SecurityOpts: r.SecurityOpts,
		CapDropAll:   true,
		Mounts: []engine.RunMount{
			{Source: dir, Target: boxside.ContextMount},
			{Source: r.Toolbox.Dir, Target: paths.InBoxTools, ReadOnly: true},
		},
		// As for the installers: the directories above devc's mounts are
		// tmpfs, not image-layer directories.
		Tmpfs:   toolbox.InstallerTmpfs,
		Env:     []string{"DEVC_CONTEXT_WORKER=1", "HOME=/tmp"},
		Workdir: "/",
		Cmd:     append([]string{paths.InBoxHelper, "box", "context-worker"}, args...),
		Timeout: workerTimeout,
	})
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) {
			return res, fmt.Errorf("the context worker failed: %s", proto.PrintableText(lastLines(string(res.Stderr), 20)))
		}
		return res, err
	}
	return res, nil
}

// Result of a commit.
type Result struct {
	// Commit is the new commit's abbreviated ID, "" when nothing changed.
	Commit string
	// Skipped lists the entries that were left out, printable.
	Skipped []string
}

// Commit records the realm's context directory as a commit when it
// changed.
func (r *Repo) Commit(ctx context.Context, realm, message string) (Result, error) {
	res, err := r.run(ctx, realm, "commit", "--message", message)
	out := Result{Skipped: skipped(string(res.Stderr))}
	if err != nil {
		return out, err
	}
	for _, l := range strings.Split(string(res.Stdout), "\n") {
		if c, ok := strings.CutPrefix(l, "committed "); ok && len(c) <= 64 {
			out.Commit = proto.Printable(c)
		}
	}
	return out, nil
}

// Log returns the last n commits with their file lists, printable.
func (r *Repo) Log(ctx context.Context, realm string, n int) (string, error) {
	res, err := r.run(ctx, realm, "log", "-n", strconv.Itoa(n))
	if err != nil {
		return "", err
	}
	return proto.PrintableText(string(res.Stdout)), nil
}

// Diff returns what a commit would record now, printable, and the skipped
// entries.
func (r *Repo) Diff(ctx context.Context, realm string) (string, []string, error) {
	res, err := r.run(ctx, realm, "diff")
	if err != nil {
		return "", nil, err
	}
	return proto.PrintableText(string(res.Stdout)), skipped(string(res.Stderr)), nil
}

// skipped extracts the worker's "skipped …" lines.
func skipped(stderr string) []string {
	var out []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, "skipped ") {
			out = append(out, proto.Printable(l))
		}
	}
	return out
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}
