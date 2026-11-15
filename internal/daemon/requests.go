package daemon

// Requests (work order C10a section 2). A box sends host.request; the
// daemon validates it strictly (internal/requests), resolves what needs the
// project's configuration (a seal path against the data root), stores it,
// records a request event in the session's journal and, for the kinds the
// project approves automatically, approves it at once. Nothing runs before
// an approval. Approved requests are carried out through the existing
// commands as child processes of the same binary (`devc seal`, `devc save`,
// `devc root`), so their locks, fail-closed checks and root log apply.
// Who may decide what is requests.MayDecide's alone to say.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mb706/devc/internal/data"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/requests"
	"github.com/mb706/devc/internal/session"
)

// Request limits per project.
const (
	maxPending     = 100
	maxPerMinute   = 20
	maxStatusWait  = 600
	requestTimeout = 30 * time.Minute
	maxResultText  = 2000
	maxReasonText  = 500
)

// requestService keeps the requests of every project.
type requestService struct {
	d     *Daemon
	store *requests.Store
	now   func() time.Time

	// mu serialises every change of the request logs.
	mu        sync.Mutex
	changedCh chan struct{}
}

func newRequestService(d *Daemon) *requestService {
	return &requestService{d: d, store: &requests.Store{Paths: d.o.Paths}, now: time.Now, changedCh: make(chan struct{})}
}

func (rs *requestService) changed() <-chan struct{} {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.changedCh
}

// putLocked stores a request, records its event and wakes waiters. The
// caller holds rs.mu.
func (rs *requestService) putLocked(ctx context.Context, ref names.ProjectRef, r *proto.Request) error {
	r.Updated = rs.now().UTC()
	if err := rs.store.Put(ref, *r); err != nil {
		return err
	}
	close(rs.changedCh)
	rs.changedCh = make(chan struct{})
	rs.event(ctx, ref, r)
	return nil
}

// event writes a request event into the requesting session's journal, when
// the request names a session of this project that has one.
func (rs *requestService) event(ctx context.Context, ref names.ProjectRef, r *proto.Request) {
	if r.Session == "" {
		return
	}
	e := rs.d.current()
	if _, ok := session.FindJournal(e.cfg.Paths, []names.ProjectRef{ref}, r.Session); !ok {
		return
	}
	ev := proto.RequestEvent{ID: r.ID, Kind: r.Kind, Risk: r.Risk, Status: r.Status, Summary: requests.Summary(r.Params)}
	if err := rs.d.recordHost(context.WithoutCancel(ctx), ref, r.Session, proto.TypeRequest, ev); err != nil {
		rs.d.log.Warn("recording a request event", "ref", ref.String(), "request", r.ID, "err", err)
	}
}

// expireLocked marks a project's pending requests past their expiry.
func (rs *requestService) expireLocked(ctx context.Context, ref names.ProjectRef, list []proto.Request) []proto.Request {
	now := rs.now()
	for i := range list {
		if requests.Expired(&list[i], now) {
			list[i].Status, list[i].Result = proto.RequestExpired, "nobody decided within seven days"
			if err := rs.putLocked(ctx, ref, &list[i]); err != nil {
				rs.d.log.Warn("expiring a request", "request", list[i].ID, "err", err)
			}
		}
	}
	return list
}

// recoverInterrupted runs once at start: a request found approved was being
// carried out when the daemon stopped. It is not run again (it may have
// taken effect); it fails and says so, and the box can ask again.
func (rs *requestService) recoverInterrupted(ctx context.Context) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, ref := range rs.d.current().refs {
		list, err := rs.store.Load(ref)
		if err != nil {
			continue
		}
		for i := range list {
			if list[i].Status == proto.RequestApproved {
				list[i].Status, list[i].Result = proto.RequestFailed, "the daemon stopped while carrying this out; it was not repeated (make the request again if it is still needed)"
				if err := rs.putLocked(ctx, ref, &list[i]); err != nil {
					rs.d.log.Warn("recovering a request", "request", list[i].ID, "err", err)
				}
			}
		}
	}
}

// expireAll expires the pending requests of every project (housekeeping).
func (rs *requestService) expireAll(ctx context.Context) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, ref := range rs.d.current().refs {
		list, err := rs.store.Load(ref)
		if err == nil {
			rs.expireLocked(ctx, ref, list)
		}
	}
}

func invalidParams(err error) *proto.Error {
	return proto.Errorf(proto.CodeInvalidParams, "%s", proto.Printable(proto.Truncate(err.Error(), 300)))
}

// create handles host.request from the box of ref.
func (rs *requestService) create(ctx context.Context, ref names.ProjectRef, params json.RawMessage) (any, error) {
	var in proto.RequestParams
	if err := proto.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	p, err := requests.Validate(in)
	if err != nil {
		return nil, invalidParams(err)
	}
	e := rs.d.current()
	r, perr := resolvedOf(e, ref)
	if perr != nil {
		return nil, perr
	}
	if p.Kind == proto.RequestSeal {
		dp, err := data.ProjectOf(e.cfg.Paths, r)
		if err != nil {
			return nil, proto.Errorf(proto.CodeInvalidParams, "%s defines no data zones; nothing can be sealed", ref)
		}
		rel, err := data.Normalize(dp, e.cfg.Paths.ProjectsRoot(), p.Path)
		if err != nil {
			return nil, invalidParams(err)
		}
		p.Path = rel
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	list, err := rs.store.Load(ref)
	if err != nil {
		rs.d.log.Error("reading the request log", "ref", ref.String(), "err", err)
		return nil, proto.Errorf(proto.CodeInternal, "the host could not read its request log")
	}
	list = rs.expireLocked(ctx, ref, list)
	pending, recent := 0, 0
	now := rs.now()
	for _, q := range list {
		if q.Status == proto.RequestPending {
			pending++
		}
		if now.Sub(q.Time) < time.Minute {
			recent++
		}
	}
	if pending >= maxPending || recent >= maxPerMinute {
		return nil, proto.Errorf(proto.CodeBusy, "too many requests from %s: wait for the user to decide the pending ones", ref)
	}
	req := proto.Request{ID: requests.NewID(), Time: now.UTC(), Ref: ref.String(), Session: p.Session, Kind: p.Kind,
		Params: p, Risk: proto.RequestRisk(p.Kind), Status: proto.RequestPending}
	req.Params.Session = ""
	if err := rs.putLocked(ctx, ref, &req); err != nil {
		rs.d.log.Error("storing a request", "ref", ref.String(), "err", err)
		return nil, proto.Errorf(proto.CodeInternal, "the host could not store the request")
	}
	rs.d.log.Info("request", "ref", ref.String(), "id", req.ID, "kind", req.Kind, "risk", req.Risk)
	if slices.Contains(r.Requests.Auto, req.Kind) && requests.MayDecide(proto.OriginAuto, req.Risk) == nil {
		if err := rs.decideLocked(ctx, ref, &req, true, "", proto.OriginAuto); err != nil {
			rs.d.log.Warn("approving a request automatically", "request", req.ID, "err", err)
		}
	}
	return proto.RequestReply{ID: req.ID, Status: req.Status, Risk: req.Risk}, nil
}

// status handles host.request.status: only requests of the caller's own
// project are visible.
func (rs *requestService) status(ctx context.Context, ref names.ProjectRef, params json.RawMessage) (any, error) {
	var p proto.RequestStatusParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if !proto.ValidRequestID(p.ID) || p.Wait < 0 || p.Wait > maxStatusWait {
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid request ID or wait")
	}
	deadline := time.Now().Add(time.Duration(p.Wait) * time.Second)
	for {
		ch := rs.changed()
		list, err := rs.store.Load(ref)
		if err != nil {
			return nil, proto.Errorf(proto.CodeInternal, "the host could not read its request log")
		}
		var found *proto.Request
		for i := range list {
			if list[i].ID == p.ID {
				found = &list[i]
			}
		}
		if found == nil {
			return nil, proto.Errorf(proto.CodeNotFound, "no request %s for this box", p.ID)
		}
		if requests.Expired(found, rs.now()) {
			rs.mu.Lock()
			rs.expireLocked(ctx, ref, []proto.Request{*found})
			rs.mu.Unlock()
			continue
		}
		left := time.Until(deadline)
		if proto.FinalRequest(found.Status) || left <= 0 {
			return proto.RequestStatus{ID: found.ID, Kind: found.Kind, Risk: found.Risk, Status: found.Status, Result: found.Result, Reason: found.Reason}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		case <-time.After(left):
		}
	}
}

// list handles requests.list.
func (rs *requestService) list(ctx context.Context, p proto.RequestsListParams) (proto.RequestsListResult, error) {
	refs, err := rs.d.matchRefs(p.Realm, p.Project)
	if err != nil {
		return proto.RequestsListResult{}, err
	}
	out := proto.RequestsListResult{Requests: []proto.Request{}}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, ref := range refs {
		list, err := rs.store.Load(ref)
		if err != nil {
			return out, err
		}
		for _, r := range rs.expireLocked(ctx, ref, list) {
			if p.All || !proto.FinalRequest(r.Status) {
				out.Requests = append(out.Requests, r)
			}
		}
	}
	slices.SortFunc(out.Requests, func(a, b proto.Request) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// decide handles requests.decide with the origin of the connection it came
// on (OriginLocal for the daemon's socket).
func (rs *requestService) decide(ctx context.Context, connOrigin string, p proto.DecideParams) (proto.DecideResult, error) {
	origin, err := requests.EffectiveOrigin(connOrigin, p.Origin)
	if err != nil {
		return proto.DecideResult{}, proto.Errorf(proto.CodeInvalidParams, "%s", err.Error())
	}
	if p.ID == "" || len(p.ID) > 64 {
		return proto.DecideResult{}, proto.Errorf(proto.CodeInvalidParams, "invalid request ID")
	}
	if err := requests.CheckReason(p.Reason, maxReasonText); err != nil {
		return proto.DecideResult{}, proto.Errorf(proto.CodeInvalidParams, "%s", err.Error())
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	ref, req, err := rs.store.Find(rs.d.current().refs, p.ID)
	if err != nil {
		if errors.Is(err, requests.ErrNotFound) {
			return proto.DecideResult{}, proto.Errorf(proto.CodeNotFound, "no request %s", proto.Printable(p.ID))
		}
		return proto.DecideResult{}, proto.Errorf(proto.CodeInvalidParams, "%s", err.Error())
	}
	rs.expireLocked(ctx, ref, []proto.Request{req})
	if requests.Expired(&req, rs.now()) {
		req.Status = proto.RequestExpired
	}
	if err := rs.decideLocked(ctx, ref, &req, p.Approve, p.Reason, origin); err != nil {
		return proto.DecideResult{Request: req}, err
	}
	return proto.DecideResult{Request: req}, nil
}

// decideLocked applies a decision to a pending request and starts an
// approved one. The caller holds rs.mu.
func (rs *requestService) decideLocked(ctx context.Context, ref names.ProjectRef, req *proto.Request, approve bool, reason, origin string) error {
	if req.Status != proto.RequestPending {
		return proto.Errorf(proto.CodeRefused, "request %s is %s already", req.ID, req.Status)
	}
	if err := requests.MayDecide(origin, req.Risk); err != nil {
		return proto.Errorf(proto.CodeRefused, "%s", err.Error())
	}
	e := rs.d.current()
	if approve {
		if _, perr := resolvedOf(e, ref); perr != nil {
			return perr
		}
	}
	req.DecidedBy = origin
	if !approve {
		req.Status, req.Reason = proto.RequestDenied, reason
		rs.d.log.Info("request denied", "request", req.ID, "origin", origin)
		return rs.putLocked(ctx, ref, req)
	}
	req.Status = proto.RequestApproved
	if req.Kind == proto.RequestNote {
		req.Status, req.Result = proto.RequestDone, "noted"
	}
	if err := rs.putLocked(ctx, ref, req); err != nil {
		return err
	}
	rs.d.log.Info("request approved", "request", req.ID, "origin", origin)
	if req.Status == proto.RequestApproved {
		r := *req
		rs.d.goSafe("request "+r.ID, func() { rs.execute(ref, r) })
	}
	return nil
}

// execute carries out an approved request and stores the outcome.
func (rs *requestService) execute(ref names.ProjectRef, req proto.Request) {
	ctx := rs.d.ctx
	steps, unavailable := requestCommands(ref, req)
	status, result := proto.RequestDone, ""
	if unavailable != "" {
		status, result = proto.RequestFailed, unavailable
	}
	var out []string
	for _, args := range steps {
		res, err := rs.d.runChild(ctx, args, requestTimeout)
		text := strings.TrimSpace(string(res.Stdout) + "\n" + string(res.Stderr))
		if text != "" {
			out = append(out, text)
		}
		if err != nil {
			status = proto.RequestFailed
			var ee *execx.ExitError
			if !errors.As(err, &ee) {
				out = append(out, err.Error())
			}
			break
		}
	}
	if result == "" {
		result = strings.Join(out, "\n")
		if result == "" && status == proto.RequestDone {
			result = "done"
		}
	}
	// Keep the end of long output: that is where the outcome is.
	if r := []rune(result); len(r) > maxResultText {
		result = "…" + string(r[len(r)-maxResultText:])
	}
	req.Status, req.Result = status, proto.PrintableText(result)
	rs.mu.Lock()
	err := rs.putLocked(ctx, ref, &req)
	rs.mu.Unlock()
	if err != nil {
		rs.d.log.Error("storing a request's outcome", "request", req.ID, "err", err)
	}
	rs.d.log.Info("request finished", "request", req.ID, "status", status)
}

// requestCommands are the devc commands (without the global flags) that
// carry out an approved request, or the answer for kinds that cannot be
// carried out yet.
func requestCommands(ref names.ProjectRef, req proto.Request) ([][]string, string) {
	p := req.Params
	switch req.Kind {
	// "--" ends flag parsing: a box-chosen path such as "--help" stays a
	// path.
	case proto.RequestSeal:
		return [][]string{{"seal", "--", p.Path, ref.String()}}, ""
	case proto.RequestSavepoint:
		if p.Name != "" {
			return [][]string{{"save", "--", p.Name, ref.String()}}, ""
		}
		return [][]string{{"save", "--", ref.String()}}, ""
	case proto.RequestPackage:
		root := []string{"root", ref.String(), "--"}
		var steps [][]string
		switch p.Manager {
		case "apt":
			env := []string{"env", "DEBIAN_FRONTEND=noninteractive"}
			steps = append(steps, concat(root, env, []string{"apt-get", "update"}))
			steps = append(steps, concat(root, env, []string{"apt-get", "install", "-y", "--no-install-recommends"}, p.Packages))
		case "dnf":
			steps = append(steps, concat(root, []string{"dnf", "install", "-y"}, p.Packages))
		case "apk":
			steps = append(steps, concat(root, []string{"apk", "add", "--no-progress"}, p.Packages))
		}
		return steps, ""
	case proto.RequestNet:
		return nil, "not available yet: network exceptions arrive with the gateway (C13); ask the user to run 'devc forward' or change the network level"
	case proto.RequestGrant:
		return nil, "not available yet: grants are changed in project.toml by the user for now"
	case proto.RequestPR:
		return nil, "not available yet: pull requests arrive with the GitHub facade (C15); ask the user to push and open it"
	}
	return nil, "unknown request kind"
}

func concat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// runChild runs devc itself with this daemon's directories, from /.
func (d *Daemon) runChild(ctx context.Context, args []string, timeout time.Duration) (execx.Result, error) {
	full := append(append([]string{}, d.o.Global...), args...)
	return d.o.Runner.Run(ctx, execx.Cmd{Name: d.o.Executable, Args: full, Env: d.o.HousekeepingEnv, Dir: "/", Timeout: timeout, MaxOutput: 64 << 10})
}

// apiRequests and apiDecide serve requests.list and requests.decide on the
// local socket: its origin is local.
func (d *Daemon) apiRequests(ctx context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.RequestsListParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	d.fresh()
	return d.requests.list(ctx, p)
}

func (d *Daemon) decideHandler(origin string) proto.Handler {
	return func(ctx context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
		var p proto.DecideParams
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		d.fresh()
		return d.requests.decide(ctx, origin, p)
	}
}
