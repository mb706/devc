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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
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
	return r.runWith(ctx, realm, nil, args...)
}

// runWith runs the worker with extra mounts.
func (r *Repo) runWith(ctx context.Context, realm string, extra []engine.RunMount, args ...string) (execx.Result, error) {
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
	unlock, err := flock.Acquire(ctx, r.Paths.ContextLockFile(realm), flock.Options{Timeout: 5 * time.Minute})
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
		Mounts: append([]engine.RunMount{
			{Source: dir, Target: boxside.ContextMount},
			{Source: r.Toolbox.Dir, Target: paths.InBoxTools, ReadOnly: true},
		}, extra...),
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

// New returns the context repository runner of a configuration: the
// toolbox of its state directory, the auxiliary containers' security
// options (label=disable on SELinux hosts only with selinux.mode
// "disable"), the installer image's build context in the cache.
func New(c *config.Config, eng engine.Engine, exe, version string, out io.Writer) *Repo {
	tb := toolbox.New(c.Paths.ToolboxDir())
	disable := false
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err == nil {
		disable = c.Global.SELinux.Mode == config.SELinuxDisable
	}
	return &Repo{
		Paths: c.Paths, Engine: eng, Toolbox: tb, Executable: exe, Version: version,
		SecurityOpts: tb.AuxSecurityOpts(disable),
		ImageContext: filepath.Join(c.Paths.CacheDir(), "aux", "installer"), Out: out,
	}
}

// maxBundleResult caps the worker's JSON answers.
const maxBundleResult = 4 << 20

// BundleResult is a bundle of the context repository: its refs now and,
// when there were new objects, the bundle file in the exchange directory.
type BundleResult struct {
	Refs map[string]string
	// File is the bundle (in the directory given to Bundle), "" when
	// nothing was new.
	File string
}

// Bundle bundles the objects of the realm's context repository that the
// commits in excludes do not reach (all of them without excludes). dir is
// a new, empty host directory of the caller's; the worker writes the
// bundle there, and it is checked to be a regular file (not a link) before
// it is handed back.
func (r *Repo) Bundle(ctx context.Context, realm string, excludes []string, dir string) (*BundleResult, error) {
	var b strings.Builder
	for _, e := range excludes {
		if boxside.ValidOID(e) {
			b.WriteString(e + "\n")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, boxside.ContextExcludesFile), []byte(b.String()), 0o600); err != nil {
		return nil, err
	}
	res, err := r.runWith(ctx, realm, []engine.RunMount{{Source: dir, Target: boxside.ContextIOMount}}, "bundle")
	if err != nil {
		return nil, err
	}
	var out boxside.ContextBundleResult
	if err := decodeStrict(res.Stdout, &out); err != nil {
		return nil, fmt.Errorf("the context worker's bundle report is invalid: %w", err)
	}
	refs := map[string]string{}
	for k, v := range out.Refs {
		if !boxside.ValidContextRef(k) || !boxside.ValidOID(v) {
			return nil, fmt.Errorf("the context worker reported an invalid ref %q", proto.Printable(proto.Truncate(k, 100)))
		}
		refs[k] = v
	}
	br := &BundleResult{Refs: refs}
	if out.Bundle {
		p := filepath.Join(dir, boxside.ContextBundleFile)
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, errors.New("the context worker wrote no regular bundle file")
		}
		br.File = p
	}
	return br, nil
}

// RestoreResult reports a restore of the context repository.
type RestoreResult = boxside.ContextRestoreResult

// Restore fetches the bundles in dir (named <seq>.bundle, applied in
// order) into the realm's context repository and creates the refs of
// refs that are missing; see boxside.ContextWorker.Restore. dir is a host
// directory of the caller's, mounted read-only.
func (r *Repo) Restore(ctx context.Context, realm string, refs map[string]string, dir string) (*RestoreResult, error) {
	b, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, boxside.ContextRefsFile), b, 0o600); err != nil {
		return nil, err
	}
	res, err := r.runWith(ctx, realm, []engine.RunMount{{Source: dir, Target: boxside.ContextIOMount, ReadOnly: true}}, "restore")
	if err != nil {
		return nil, err
	}
	var out RestoreResult
	if err := decodeStrict(res.Stdout, &out); err != nil {
		return nil, fmt.Errorf("the context worker's restore report is invalid: %w", err)
	}
	for i := range out.Conflicts {
		out.Conflicts[i] = proto.Printable(proto.Truncate(out.Conflicts[i], 300))
	}
	for i := range out.Skipped {
		out.Skipped[i] = proto.Printable(proto.Truncate(out.Skipped[i], 300))
	}
	return &out, nil
}

func decodeStrict(b []byte, v any) error {
	if len(b) > maxBundleResult {
		return errors.New("too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("data after the report")
	}
	return nil
}
