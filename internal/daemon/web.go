package daemon

// projects.list and changes.get (work order C12 section 3), on the hub's
// allowlist: both only read. Worktrees are listed in the box (`devc box
// worktree list`, decoded strictly by gitx.WorktreeList) and remembered,
// so a stopped box still shows the last list with its time; changes are
// computed on the relay with host git (gitx.Changes) and never touch the
// project directory.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

const (
	// worktreesFresh is how long a worktree list is used before the box is
	// asked again.
	worktreesFresh  = 20 * time.Second
	worktreeTimeout = 30 * time.Second
	changesTimeout  = time.Minute
	// projectWorkers bounds the boxes asked at once.
	projectWorkers = 4
)

// worktreeCache remembers each project's last worktree list.
type worktreeCache struct {
	mu sync.Mutex
	m  map[names.ProjectRef]*worktreeEntry
}

type worktreeEntry struct {
	at      time.Time
	list    []boxside.WorktreeInfo
	err     string
	loading chan struct{}
}

// worktrees returns the worktree list of ref: asked in the box when it
// runs and the remembered list is older than worktreesFresh (one request
// at a time per project), else the remembered one. at is zero when the
// list was never read.
func (d *Daemon) worktrees(ctx context.Context, e *env, ref names.ProjectRef) (list []boxside.WorktreeInfo, at time.Time, problem string) {
	c := &d.wtCache
	running := d.boxState(ref) == "running"
	for {
		c.mu.Lock()
		if c.m == nil {
			c.m = map[names.ProjectRef]*worktreeEntry{}
		}
		ent := c.m[ref]
		if ent == nil {
			ent = &worktreeEntry{}
			c.m[ref] = ent
		}
		if !running || time.Since(ent.at) < worktreesFresh {
			list, at, problem = ent.list, ent.at, ent.err
			c.mu.Unlock()
			return list, at, problem
		}
		if ent.loading != nil {
			wait := ent.loading
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, time.Time{}, "cancelled"
			}
		}
		ent.loading = make(chan struct{})
		c.mu.Unlock()

		wts, err := d.readWorktrees(ctx, e, ref)
		c.mu.Lock()
		if err != nil {
			ent.err = proto.Printable(proto.Truncate(err.Error(), 300))
			// Keep the last good list; ask again at the next request.
		} else {
			ent.list, ent.at, ent.err = wts, time.Now().UTC(), ""
		}
		close(ent.loading)
		ent.loading = nil
		list, at, problem = ent.list, ent.at, ent.err
		c.mu.Unlock()
		return list, at, problem
	}
}

func (d *Daemon) readWorktrees(ctx context.Context, e *env, ref names.ProjectRef) ([]boxside.WorktreeInfo, error) {
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		return nil, err
	}
	gs := d.gitService(e)
	ctx, cancel := context.WithTimeout(ctx, worktreeTimeout)
	defer cancel()
	return gs.WorktreeList(ctx, gs.Project(r, true))
}

func (d *Daemon) apiProjects(ctx context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.ProjectsListParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	refs, err := d.matchRefs(p.Realm, p.Project)
	if err != nil {
		return nil, err
	}
	d.fresh()
	e := d.current()
	type counts struct{ live, needs int }
	byRef := map[names.ProjectRef]*counts{}
	byWorktree := map[[2]string]int{}
	for _, v := range d.hub.views(listFilter{realm: p.Realm, project: p.Project, live: true}) {
		ref, err := names.Parse(v.Ref)
		if err != nil {
			continue
		}
		c := byRef[ref]
		if c == nil {
			c = &counts{}
			byRef[ref] = c
		}
		c.live++
		if v.NeedsYou() {
			c.needs++
		}
		wt := v.Worktree
		if wt == "" {
			wt = "main"
		}
		byWorktree[[2]string{v.Ref, wt}]++
	}
	out := make([]proto.ProjectView, len(refs))
	sem := make(chan struct{}, projectWorkers)
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := d.projectView(ctx, e, ref)
			if c := byRef[ref]; c != nil {
				v.Live, v.NeedsYou = c.live, c.needs
			}
			for j := range v.Worktrees {
				v.Worktrees[j].Sessions = byWorktree[[2]string{v.Ref, v.Worktrees[j].Name}]
			}
			out[i] = v
		}()
	}
	wg.Wait()
	return proto.ProjectsListResult{Projects: out}, nil
}

// projectView describes one configured project.
func (d *Daemon) projectView(ctx context.Context, e *env, ref names.ProjectRef) proto.ProjectView {
	v := proto.ProjectView{Ref: ref.String(), Realm: ref.Realm, Project: ref.Project, Box: d.boxState(ref),
		Harnesses: []proto.HarnessView{}, Worktrees: []proto.WorktreeView{}}
	if v.Box == "" {
		v.Box = "absent"
	}
	if probs := e.cfg.ProblemsFor(ref); len(probs) > 0 {
		v.Problem = "the configuration of " + ref.String() + " has problems: run 'devc config check' on the machine"
	}
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		v.Problem = proto.Printable(proto.Truncate(err.Error(), 300))
		return v
	}
	for _, h := range r.Harnesses {
		v.Harnesses = append(v.Harnesses, proto.HarnessView{ID: h.ID,
			Ready: fileExists(e.cfg.Paths.BoxEtcDir(ref) + "/harness/" + h.ID + ".json")})
	}
	gs := d.gitService(e)
	v.DefaultBranch = gs.DefaultBranch(ctx, gs.Project(r, false))
	list, at, problem := d.worktrees(ctx, e, ref)
	if !at.IsZero() {
		v.WorktreesAt = &at
	}
	v.Error = problem
	for _, w := range list {
		wv := proto.WorktreeView{Name: w.Name, Branch: w.Branch, Head: w.Head, Dirty: w.Dirty, Error: w.Error}
		if w.Origin != nil {
			wv.Origin = &proto.Divergence{Ref: w.Origin.Ref, Ahead: w.Origin.Ahead, Behind: w.Origin.Behind}
		}
		if w.Upstream != nil {
			wv.Upstream = &proto.Divergence{Ref: w.Upstream.Ref, Ahead: w.Upstream.Ahead, Behind: w.Upstream.Behind}
		}
		v.Worktrees = append(v.Worktrees, wv)
	}
	return v
}

func (d *Daemon) apiChanges(ctx context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.ChangesParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if err := checkSession(p.Session); err != nil {
		return nil, err
	}
	if p.Scope == "" {
		p.Scope = proto.ScopeSnapshot
	}
	if p.Scope != proto.ScopeSnapshot && p.Scope != proto.ScopeBranch {
		return nil, proto.Errorf(proto.CodeInvalidParams, "scope must be %s or %s", proto.ScopeSnapshot, proto.ScopeBranch)
	}
	v, ref, ok := d.cmd.lookup(ctx, p.Session)
	if !ok {
		return nil, proto.Errorf(proto.CodeNotFound, "no session %s", p.Session)
	}
	e := d.current()
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		return nil, proto.Errorf(proto.CodeNotFound, "the project %s is not configured", ref)
	}
	worktree := v.Worktree
	if worktree == "" || names.Check(names.KindWorktree, worktree) != nil {
		worktree = "main"
	}
	gs := d.gitService(e)
	proj := gs.Project(r, d.boxState(ref) == "running")
	ctx, cancel := context.WithTimeout(ctx, changesTimeout)
	defer cancel()
	branch := d.worktreeBranch(ctx, e, gs, proj, worktree)
	res, err := gs.Changes(ctx, proj, p.Scope, worktree, branch)
	if err != nil {
		d.log.Warn("changes", "session", p.Session, "scope", p.Scope, "err", err)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, proto.Errorf(proto.CodeBusy, "computing the changes took too long")
		}
		return nil, proto.Errorf(proto.CodeInternal, "the changes could not be computed: %s", proto.Printable(proto.Truncate(err.Error(), 200)))
	}
	return res, nil
}

// worktreeBranch is the branch checked out in a worktree: from the box's
// worktree list (current or remembered), else by devc's convention (task
// worktrees get <branch_prefix><name>; main the default branch).
func (d *Daemon) worktreeBranch(ctx context.Context, e *env, gs *gitx.Service, p *gitx.Project, worktree string) string {
	list, _, _ := d.worktrees(ctx, e, p.Ref)
	for _, w := range list {
		if w.Name == worktree {
			return w.Branch
		}
	}
	if worktree == "main" {
		return gs.DefaultBranch(ctx, p)
	}
	b := p.Resolved.Git.BranchPrefix + worktree
	if gitx.CheckName(b) != nil {
		return ""
	}
	return b
}
