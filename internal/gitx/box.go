// This file holds Service and everything it does in the box: git and
// `devc box …` commands through `podman exec` as dev, fetches from and
// pushes to the box repository over the ext:: transport, and the strict
// decoding of what the box reports.

package gitx

import (
	"context"
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
	// Free measures the free space of a path (paths.FreeSpace when nil);
	// tests replace it.
	Free func(path string) (int64, error)
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
// nothing after it (proto.StrictJSON).
func decodeStrict(b []byte, v any) error {
	if len(b) > maxReport {
		return fmt.Errorf("the report is larger than %d bytes", maxReport)
	}
	return proto.StrictJSON(b, v)
}

// boxURL is the ext:: remote of the project's box repository.
func (s *Service) boxURL(p *Project) (string, error) {
	return BoxURL(s.Engine.Binary(), p.Container, paths.InBoxRepoDir(p.Ref))
}

func (s *Service) free(path string) (int64, error) {
	if s.Free != nil {
		return s.Free(path)
	}
	return paths.FreeSpace(path)
}

// fetchFromBox fetches refspecs, each naming one ref (no wildcard), from
// the box repository into the relay; git-upload-pack runs in the box, and
// the relay checks every object. It is bounded like a push through the
// relay service (C20a; findings D1-05, E1-16, F1-01): it does not start
// when the state file system has less than paths.StateMinFree free or the
// relay is over git.relay_quota, and Watch ends it when the relay's new
// objects would take it over the budget or the file system below half
// the reserve. git runs in a process group of its own, which is ended as
// a whole; only once none of its processes is left are its temporary
// packs and lock files removed (RemoveFetchLeftovers), so an ended fetch
// leaves the relay as it was apart from packs it completed. Refs are
// written only below refs/devc/incoming/, which the bank empties.
func (s *Service) fetchFromBox(ctx context.Context, p *Project, refspecs []string) error {
	if !p.Running {
		return fmt.Errorf("%s: %w", p.Container, ErrNotRunning)
	}
	for _, rs := range refspecs {
		if strings.ContainsAny(rs, "*\n\x00") || !strings.HasPrefix(rs, "+refs/") || !strings.Contains(rs, ":"+IncomingPrefix) {
			return fmt.Errorf("internal error: refspec %q is not one ref into %s", printableName(rs), IncomingPrefix)
		}
	}
	url, err := s.boxURL(p)
	if err != nil {
		return err
	}
	relay := p.Relay.Dir
	free, err := s.free(relay)
	if err != nil {
		return fmt.Errorf("checking the free space of the state file system: %w", err)
	}
	if free < paths.StateMinFree {
		return fmt.Errorf("the state file system has only %s free and devc keeps %s free, so nothing was fetched from %s: free space on the host",
			SizeText(free), SizeText(paths.StateMinFree), p.Container)
	}
	var quota int64
	if p.Resolved != nil {
		quota = p.Resolved.Git.RelayQuota
	}
	used, err := RelayUsage(relay)
	if err != nil {
		return fmt.Errorf("measuring the relay: %w", err)
	}
	if quota > 0 && used > quota {
		return fmt.Errorf("the relay holds %s, its budget is %s (git.relay_quota), so nothing was fetched from %s: raise the budget or clean up the relay",
			SizeText(used), SizeText(quota), p.Container)
	}
	base, err := ObjectsUsage(relay)
	if err != nil {
		return fmt.Errorf("measuring the relay: %w", err)
	}
	gctx, end := context.WithCancel(ctx)
	defer end()
	watched := make(chan string, 1)
	go func() {
		why := Watch(gctx, relay, used, quota, func() (int64, error) {
			n, err := ObjectsUsage(relay)
			return n - base, err
		}, s.free)
		if why != "" {
			end()
		}
		watched <- why
	}()
	args := []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--no-auto-gc", "--stdin", url}
	res, err := s.Host.Run(gctx, Call{Relay: relay, Args: args, Box: true, Group: true, Stdin: strings.NewReader(strings.Join(refspecs, "\n") + "\n")})
	end()
	why := <-watched
	var left *execx.GroupLeftError
	if errors.As(err, &left) {
		// As holdRelay does for a push (finding F1-01): the caller holds
		// the box lock, and keeps it until git's processes are gone.
		s.printf("devc: git processes of the fetch from %s did not end; devc keeps the box locked until they have\n", p.Container)
		select {
		case <-left.Gone:
			RemoveFetchLeftovers(relay)
		case <-ctx.Done():
		}
		return fmt.Errorf("fetching from %s: %w", p.Container, err)
	}
	if why != "" || err != nil {
		RemoveFetchLeftovers(relay)
	}
	if why != "" {
		return fmt.Errorf("devc ended the fetch from %s: %s", p.Container, why)
	}
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
