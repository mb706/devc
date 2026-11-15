package gitx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

// Service runs the git operations of devc.
type Service struct {
	Host   *Host
	Engine engine.Engine
	Paths  *paths.Paths
	// Out receives summaries, notes and confirmations.
	Out io.Writer
	// Now is the clock for timestamped refs.
	Now func() time.Time
}

// Project is one project as the git operations see it.
type Project struct {
	Ref      names.ProjectRef
	Resolved *config.Resolved
	Relay    *Relay
	// Container is the box's name; Running says it runs and is ready.
	Container string
	Running   bool
}

// Project returns the project of a resolved configuration.
func (s *Service) Project(r *config.Resolved, running bool) *Project {
	ref := names.ProjectRef{Realm: r.Realm, Project: r.Project}
	return &Project{
		Ref: ref, Resolved: r, Container: names.BoxName(ref), Running: running,
		Relay: &Relay{Host: s.Host, Dir: s.Paths.RelayRepo(ref)},
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) printf(format string, args ...any) {
	if s.Out != nil {
		fmt.Fprintf(s.Out, format, args...)
	}
}

// ErrNotRunning is returned by operations that need the box.
var ErrNotRunning = errors.New("the box is not running")

// boxEnv is the environment of every git command devc runs in a box.
var boxEnv = []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}

// boxExec runs a command in the box as dev from dir.
func (s *Service) boxExec(ctx context.Context, p *Project, dir string, stdin io.Reader, timeout time.Duration, cmd ...string) (execx.Result, error) {
	if !p.Running {
		return execx.Result{}, fmt.Errorf("%s: %w: start it with 'devc up %s'", p.Container, ErrNotRunning, p.Ref)
	}
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	return s.Engine.Exec(ctx, engine.ExecOptions{
		Container: p.Container, User: fmt.Sprintf("%d:%d", paths.BoxUID, paths.BoxGID), Workdir: dir,
		Env: boxEnv, Cmd: cmd, Stdin: stdin, Timeout: timeout,
	})
}

// boxGit runs git in the box and returns its trimmed output; errors carry
// the box's message, made printable.
func (s *Service) boxGit(ctx context.Context, p *Project, dir string, args ...string) (string, error) {
	res, err := s.boxExec(ctx, p, dir, nil, 0, append([]string{"git"}, args...)...)
	if err != nil {
		return "", fmt.Errorf("in %s: %s", p.Container, proto.PrintableText(err.Error()))
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// boxGitCode runs git in the box and returns its exit code.
func (s *Service) boxGitCode(ctx context.Context, p *Project, dir string, args ...string) (int, string, error) {
	res, err := s.boxExec(ctx, p, dir, nil, 0, append([]string{"git"}, args...)...)
	var ee *execx.ExitError
	if errors.As(err, &ee) {
		return res.ExitCode, "", nil
	}
	if err != nil {
		return -1, "", err
	}
	return 0, strings.TrimSpace(string(res.Stdout)), nil
}

// helperTimeout bounds `devc box …` commands that walk worktrees.
const helperTimeout = 30 * time.Minute

// boxHelper runs `devc box <args>` in the box and returns its standard
// output, which the caller must decode strictly.
func (s *Service) boxHelper(ctx context.Context, p *Project, stdin io.Reader, args ...string) ([]byte, error) {
	res, err := s.boxExec(ctx, p, "/", stdin, helperTimeout, append([]string{paths.InBoxHelper, "box"}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("in %s: %s%s", p.Container, proto.PrintableText(err.Error()), printable(tail(res.Stderr)))
	}
	return res.Stdout, nil
}

func printable(s string) string { return proto.PrintableText(s) }

// maxReport caps JSON reports read from a box.
const maxReport = 1 << 20

// decodeStrict decodes one JSON value from a box: no unknown fields,
// nothing after it.
func decodeStrict(b []byte, v any) error {
	if len(b) > maxReport {
		return fmt.Errorf("the report is larger than %d bytes", maxReport)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the report")
	}
	return nil
}

// boxURL is the ext:: remote of the project's box repository.
func (s *Service) boxURL(p *Project) (string, error) {
	return BoxURL(s.Engine.Binary(), p.Container, paths.InBoxRepoDir(p.Ref))
}

// fetchFromBox fetches refspecs from the box repository into the relay.
// git-upload-pack runs in the box; the relay checks every object.
func (s *Service) fetchFromBox(ctx context.Context, p *Project, refspecs ...string) error {
	if !p.Running {
		return fmt.Errorf("%s: %w", p.Container, ErrNotRunning)
	}
	url, err := s.boxURL(p)
	if err != nil {
		return err
	}
	args := append([]string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--no-auto-gc", url}, refspecs...)
	res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Args: args, Box: true})
	if err != nil {
		return fmt.Errorf("fetching from %s: %s%s", p.Container, printable(err.Error()), printable(tail(res.Stderr)))
	}
	return nil
}

// pushResult is one line of git push --porcelain.
type pushResult struct {
	Flag    byte
	From    string
	To      string
	Summary string
}

func parsePorcelain(out string) []pushResult {
	var rs []pushResult
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 3 || line[1] != '\t' {
			continue
		}
		parts := strings.SplitN(line[2:], "\t", 2)
		from, to, _ := strings.Cut(parts[0], ":")
		r := pushResult{Flag: line[0], From: from, To: to}
		if len(parts) == 2 {
			r.Summary = parts[1]
		}
		rs = append(rs, r)
	}
	return rs
}

// pushToBox pushes relay refs to the box repository (git-receive-pack runs
// in the box). It returns the refs the box rejected; a failure of the
// transfer itself is an error.
func (s *Service) pushToBox(ctx context.Context, p *Project, refspecs ...string) ([]pushResult, error) {
	if !p.Running {
		return nil, fmt.Errorf("%s: %w", p.Container, ErrNotRunning)
	}
	url, err := s.boxURL(p)
	if err != nil {
		return nil, err
	}
	args := append([]string{"push", "--porcelain", "--no-verify", "--no-recurse-submodules", url}, refspecs...)
	res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Args: args, Box: true})
	results := parsePorcelain(string(res.Stdout))
	var rejected []pushResult
	for _, r := range results {
		if r.Flag == '!' {
			rejected = append(rejected, r)
		}
	}
	// git exits non-zero when any ref is rejected; only a failure without
	// rejected refs is a failure of the transfer.
	if err != nil && len(rejected) == 0 {
		return rejected, fmt.Errorf("pushing to %s: %s%s", p.Container, printable(err.Error()), printable(tail(res.Stderr)))
	}
	return rejected, nil
}

// reflect pushes what the box should see of the relay: its branches as
// origin/*, and with all set also the upstream's branches as github/* and
// the tags. Tags the box already has with another value are reported, not
// forced.
func (s *Service) reflect(ctx context.Context, p *Project, all bool) ([]string, error) {
	// git push fails when no refspec matches anything, so only refspecs
	// whose source namespace has refs are passed.
	refs, err := p.Relay.Refs(ctx, HeadsPrefix, GithubPrefix, TagsPrefix)
	if err != nil {
		return nil, err
	}
	has := map[string]bool{}
	for _, r := range refs {
		for _, prefix := range []string{HeadsPrefix, GithubPrefix, TagsPrefix} {
			if strings.HasPrefix(r.Name, prefix) {
				has[prefix] = true
			}
		}
	}
	var specs []string
	if has[HeadsPrefix] {
		specs = append(specs, "+refs/heads/*:refs/remotes/origin/*")
	}
	if all && has[GithubPrefix] {
		specs = append(specs, "+"+GithubPrefix+"*:"+GithubPrefix+"*")
	}
	if all && has[TagsPrefix] {
		specs = append(specs, "refs/tags/*:refs/tags/*")
	}
	if len(specs) == 0 {
		return nil, nil
	}
	rejected, err := s.pushToBox(ctx, p, specs...)
	var notes []string
	for _, r := range rejected {
		notes = append(notes, fmt.Sprintf("the box kept its own %s (%s)", printable(r.To), printable(r.Summary)))
	}
	return notes, err
}

// snapshot runs `devc box snapshot --all` and validates its report.
func (s *Service) snapshot(ctx context.Context, p *Project) (boxside.SnapshotReport, error) {
	var rep boxside.SnapshotReport
	out, err := s.boxHelper(ctx, p, nil, "snapshot", "--all")
	if err != nil {
		return rep, err
	}
	if err := decodeStrict(out, &rep); err != nil {
		return rep, fmt.Errorf("the snapshot report of %s is invalid: %w", p.Container, err)
	}
	if rep.V != boxside.SnapshotReportVersion || len(rep.Worktrees) > 1000 {
		return rep, fmt.Errorf("the snapshot report of %s is invalid (version %d, %d worktrees)", p.Container, rep.V, len(rep.Worktrees))
	}
	for i := range rep.Worktrees {
		w := &rep.Worktrees[i]
		if !names.ValidKind(names.KindWorktree, w.Name) || (w.Commit != "" && !ValidOID(w.Commit)) || len(w.Skipped) > 1000 {
			return rep, fmt.Errorf("the snapshot report of %s is invalid (worktree %q)", p.Container, proto.Printable(w.Name))
		}
		for j, f := range w.Skipped {
			if len(f) > 4096 {
				f = f[:4096]
			}
			w.Skipped[j] = proto.Printable(f)
		}
		if len(w.Error) > 2000 {
			w.Error = w.Error[:2000]
		}
		w.Error = proto.Printable(w.Error)
	}
	return rep, nil
}

// MainBranch returns the branch checked out in the box's main worktree.
func (s *Service) MainBranch(ctx context.Context, p *Project) (string, error) {
	code, out, err := s.boxGitCode(ctx, p, "/", "--git-dir="+paths.InBoxRepoDir(p.Ref), "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	b, ok := strings.CutPrefix(out, HeadsPrefix)
	if code != 0 || !ok || CheckName(b) != nil {
		return "", fmt.Errorf("the main worktree of %s has no branch checked out", p.Container)
	}
	return b, nil
}
