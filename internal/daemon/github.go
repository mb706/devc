// This file serves GitHub to boxes (work order C15). host.github is the
// read-only facade: the box names an operation, github.PlanRead validates
// it against the project's repository (its upstream as configured now) and
// grants.github_read (in force at the box's start and now), and the daemon
// performs the GET requests with the host's own credential, which never
// leaves the daemon: the answer is GitHub's status and body, scrubbed of
// the credential, on the stream that asked. host.github.token hands a box
// the token of its project's mode, "token:<secret>" (stored) or "app"
// (minted for the one repository with the permissions in force), when the
// box was started with that mode, repository and App and the configuration
// still says so (box.Capabilities, findings C1-02 and D2-02). Tokens are
// never logged.

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/github"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
)

// ghReadsPerMinute bounds the facade reads of one project: each spends the
// host credential's rate limit, which the user's own tools share.
var ghReadsPerMinute = 120

// ghReadTimeout bounds one facade read, all its requests together (the box
// waits a little longer for the answer).
const ghReadTimeout = 150 * time.Second

// ghTokensPerMinute bounds the token requests of one project (git asks for
// every operation, each session at its start).
var ghTokensPerMinute = 120

// readBudget allows at most n actions per project in any minute.
type readBudget struct {
	mu   sync.Mutex
	n    int
	now  func() time.Time
	seen map[names.ProjectRef][]time.Time
}

func newReadBudget(n int) *readBudget {
	return &readBudget{n: n, now: time.Now, seen: map[names.ProjectRef][]time.Time{}}
}

// allow records an action of ref and reports whether it is within the
// budget; a refused action is not recorded.
func (b *readBudget) allow(ref names.ProjectRef) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	keep := b.seen[ref][:0]
	for _, t := range b.seen[ref] {
		if now.Sub(t) < time.Minute && !now.Before(t) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= b.n {
		b.seen[ref] = keep
		return false
	}
	b.seen[ref] = append(keep, now)
	return true
}

// secretStore is the configured store of host secrets.
func (d *Daemon) secretStore(e *env) secrets.Store {
	if d.o.Secrets != nil {
		return d.o.Secrets
	}
	return secrets.NewFileStore(e.cfg.Paths)
}

// githubRepo is the project's repository, or why it has none.
func (d *Daemon) githubRepo(r *config.Resolved) (github.Repo, *proto.Error) {
	repo, err := github.ParseRepo(r.Upstream, d.o.GitHub.Web)
	if err != nil {
		return github.Repo{}, proto.Errorf(proto.CodeRefused, "%s/%s is not a GitHub project: %s", r.Realm, r.Project, proto.Printable(err.Error()))
	}
	return repo, nil
}

// serveGitHub answers one host.github request on its stream: a response
// line, then exactly the announced number of body bytes. A read that is
// malformed or names a repository the box may not read is refused before
// it counts against the budget and before the credential is read.
func (w *boxWorker) serveGitHub(ctx context.Context, st *yamux.Stream, m *proto.RPCMessage) {
	raw := rawStream{st: st, stall: streamStall}
	end := raw.guard(ctx, w.d.log)
	defer end()
	d, ref := w.d, w.ref
	fail := func(perr *proto.Error) { _ = respond(st, m.ID, nil, perr) }
	var p proto.GitHubParams
	if err := proto.DecodeParams(m.Params, &p); err != nil {
		var pe *proto.Error
		if !errors.As(err, &pe) {
			pe = proto.Errorf(proto.CodeInvalidParams, "invalid parameters")
		}
		fail(pe)
		return
	}
	// A grant removed a moment ago is gone for this read.
	d.fresh()
	e := d.current()
	r, perr := resolvedOf(e, ref)
	if perr != nil {
		fail(perr)
		return
	}
	own, perr := d.githubRepo(r)
	if perr != nil {
		fail(perr)
		return
	}
	// grants.github_read is a grant like the others: in force at the start
	// and now. The project's own repository needs no record.
	var extra []github.Repo
	if len(r.Grants.GitHubRead) > 0 {
		if caps, err := box.ReadCapabilities(e.cfg.Paths, ref); err == nil && caps.ContainerID == w.containerID {
			for _, s := range caps.GitHubReadRepos(r) {
				if repo, err := github.ParseRepoName(s); err == nil {
					extra = append(extra, repo)
				}
			}
		}
	}
	plan, err := github.PlanRead(p, own, extra)
	switch {
	case errors.Is(err, github.ErrRepoNotAllowed):
		d.log.Warn("refused a GitHub read", "ref", ref.String(), "err", proto.Printable(err.Error()))
		fail(proto.Errorf(proto.CodeRefused, "%s", proto.Printable(err.Error())))
		return
	case err != nil:
		fail(invalidParams(err))
		return
	}
	if !d.ghReads.allow(ref) {
		fail(proto.Errorf(proto.CodeBusy, "too many GitHub reads from %s: at most %d a minute", ref, ghReadsPerMinute))
		return
	}
	tok, err := github.HostToken(ctx, d.secretStore(e), ref.Realm, d.o.Runner, d.o.GitHub.Web)
	if err != nil {
		d.log.Warn("no GitHub credential for a read", "ref", ref.String(), "err", err)
		fail(proto.Errorf(proto.CodeRefused, "the host has no GitHub credential: ask the user to run 'devc secret set %s %s' or 'gh auth login' on the host", ref.Realm, github.HostTokenSecret))
		return
	}
	client := github.NewClient(d.o.GitHub.API, tok)
	rctx, cancel := context.WithTimeout(ctx, ghReadTimeout)
	defer cancel()
	res, err := client.Read(rctx, plan)
	if err != nil {
		d.log.Warn("GitHub read failed", "ref", ref.String(), "op", plan.Op, "repo", plan.Repo.String(), "err", proto.Printable(err.Error()))
		fail(proto.Errorf(proto.CodeInternal, "the host could not read from GitHub: %s", proto.Printable(proto.Truncate(err.Error(), 300))))
		return
	}
	d.log.Info("GitHub read", "ref", ref.String(), "op", plan.Op, "repo", plan.Repo.String(), "path", plan.Path, "status", res.Status, "bytes", len(res.Body))
	reply := proto.GitHubReply{Status: res.Status, Text: res.Text, Length: int64(len(res.Body)), Truncated: res.Truncated, Note: res.Note}
	if err := respond(st, m.ID, reply, nil); err != nil {
		return
	}
	_, _ = raw.Write(res.Body)
}

// githubToken serves host.github.token for the box of ref (container
// containerID). Every request whose parameters decode counts against the
// budget, refused ones too: the budget is checked before anything else.
func (d *Daemon) githubToken(ctx context.Context, ref names.ProjectRef, containerID string, params json.RawMessage) (any, error) {
	var p proto.GitHubTokenParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if !d.ghTokens.allow(ref) {
		return nil, proto.Errorf(proto.CodeBusy, "too many GitHub token requests from %s: at most %d a minute", ref, ghTokensPerMinute)
	}
	// A mode changed a moment ago applies to this request.
	d.fresh()
	e := d.current()
	r, perr := resolvedOf(e, ref)
	if perr != nil {
		return nil, perr
	}
	if !r.Grants.GitHubTokenMode() {
		return nil, proto.Errorf(proto.CodeRefused, "%s holds no GitHub token (grants.github is %q): push through origin, the relay, and read GitHub with 'devc gh'", ref, r.Grants.GitHub)
	}
	caps, perr := d.startedWith(e.cfg.Paths, ref, containerID)
	if perr != nil {
		return nil, perr
	}
	if _, perr := d.githubRepo(r); perr != nil {
		return nil, perr
	}
	// The mode, the repository and the App are those of the box's start
	// (findings C1-02, D2-02).
	g, err := caps.GitHubToken(r, d.o.GitHub.Web)
	if err != nil {
		d.log.Warn("refused a GitHub token", "ref", ref.String(), "err", err)
		return nil, proto.Errorf(proto.CodeRefused, "%s; ask the user to bring the box up again ('devc down %s' and 'devc up %s')", proto.Printable(err.Error()), ref, ref)
	}
	mode, perms, repo := g.Mode, g.Permissions, g.Repo
	store := d.secretStore(e)
	web := d.o.GitHub.Web
	reply := proto.GitHubTokenReply{Username: "x-access-token", Protocol: web.Scheme, Host: web.Host}
	if mode == config.GitHubModeApp {
		app := r.GitHubApp
		if app == nil {
			return nil, proto.Errorf(proto.CodeRefused, "the realm %s has no [github.app]", r.Realm)
		}
		key, err := store.Get(r.Realm, app.KeySecret)
		if err != nil {
			d.log.Warn("reading the GitHub App key", "ref", ref.String(), "secret", app.KeySecret, "err", err)
			return nil, proto.Errorf(proto.CodeNotFound, "the GitHub App's key is not available on the host: ask the user to run 'devc secret set %s %s < key.pem'", r.Realm, app.KeySecret)
		}
		tok, err := d.minter.Token(ctx, d.o.GitHub.API, ref.String(), github.App{ID: app.AppID, InstallationID: app.InstallationID}, key, repo, perms)
		if err != nil {
			d.log.Warn("minting a GitHub token", "ref", ref.String(), "repo", repo.String(), "err", proto.Printable(err.Error()))
			return nil, proto.Errorf(proto.CodeInternal, "the host could not get a token from the GitHub App: %s", proto.Printable(proto.Truncate(err.Error(), 300)))
		}
		reply.Token = tok.Value
		exp := tok.Expires.UTC()
		reply.Expires = &exp
	} else {
		name := r.Grants.GitHubTokenSecret()
		v, err := store.Get(r.Realm, name)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			return nil, proto.Errorf(proto.CodeNotFound, "the GitHub token %s is not set in realm %s: ask the user to run 'devc secret set %s %s'", name, r.Realm, r.Realm, name)
		case err != nil:
			d.log.Error("reading a GitHub token", "ref", ref.String(), "secret", name, "err", err)
			return nil, proto.Errorf(proto.CodeInternal, "the host could not read the GitHub token")
		}
		reply.Token = string(secrets.TrimValue(v))
	}
	d.log.Info("GitHub token delivered", "ref", ref.String(), "mode", mode, "repo", repo.String())
	return reply, nil
}
