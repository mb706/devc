package daemon

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// Bounds of API results (a line holds at most proto.MaxMessage).
const (
	defaultPage   = 200
	maxPage       = 1000
	maxPageBytes  = 512 << 10
	defaultRecent = 50
	maxRecent     = 1000
	syncTimeout   = 2 * time.Minute
)

func (d *Daemon) handlers() map[string]proto.Handler {
	return map[string]proto.Handler{
		proto.MethodDaemonInfo:      d.apiInfo,
		proto.MethodDaemonStop:      d.apiStop,
		proto.MethodBoxesList:       d.apiBoxes,
		proto.MethodSessionsList:    d.apiSessions,
		proto.MethodSessionsGet:     d.apiSession,
		proto.MethodEventsRead:      d.apiEvents,
		proto.MethodEventsSubscribe: d.apiSubscribe,
	}
}

func noParams(params json.RawMessage) error {
	var none struct{}
	return proto.DecodeParams(params, &none)
}

// fresh reloads the configuration if it changed, so that an answer never
// misses a project added since the last box-watch pass.
func (d *Daemon) fresh() {
	if err := d.reload(false); err != nil {
		d.log.Warn("reloading the configuration", "err", err)
	}
}

func (d *Daemon) apiInfo(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	if err := noParams(params); err != nil {
		return nil, err
	}
	d.fresh()
	e := d.current()
	return proto.DaemonInfo{Version: d.version, Started: d.started, Instance: d.instance, PID: os.Getpid(), Machine: d.machine,
		ConfigDir: e.cfg.Paths.ConfigDir(), StateDir: e.cfg.Paths.StateDir(), Boxes: d.boxViews()}, nil
}

func (d *Daemon) apiStop(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	if err := noParams(params); err != nil {
		return nil, err
	}
	d.log.Info("stop requested by a client")
	// Answer first, then stop.
	go func() {
		time.Sleep(50 * time.Millisecond)
		d.Stop()
	}()
	return struct{}{}, nil
}

// boxViews describes the box of every configured project.
func (d *Daemon) boxViews() []proto.BoxView {
	e := d.current()
	counts := map[string][2]int{}
	for _, v := range d.hub.views(listFilter{live: true}) {
		c := counts[v.Ref]
		c[0]++
		if v.NeedsYou() {
			c[1]++
		}
		counts[v.Ref] = c
	}
	out := []proto.BoxView{}
	for _, ref := range e.refs {
		v := proto.BoxView{Ref: ref.String(), Realm: ref.Realm, Project: ref.Project, Box: names.BoxName(ref), State: "absent", Channel: proto.ChannelNone}
		d.mu.Lock()
		bi := d.boxes[ref]
		var w *boxWorker
		if bi != nil {
			v.State, v.ContainerID, v.Started, w = bi.state, bi.containerID, bi.started, bi.worker
		}
		d.mu.Unlock()
		if v.State == "" {
			v.State = "absent"
		}
		if w != nil {
			w.view(&v)
		}
		c := counts[v.Ref]
		v.Live, v.NeedsYou = c[0], c[1]
		out = append(out, v)
	}
	return out
}

func (d *Daemon) apiBoxes(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	if err := noParams(params); err != nil {
		return nil, err
	}
	d.fresh()
	return d.boxViews(), nil
}

// matchRefs returns the configured projects a realm/project filter selects.
func (d *Daemon) matchRefs(realm, project string) ([]names.ProjectRef, error) {
	if realm != "" && !names.ValidRealm(realm) || project != "" && !names.Valid(project) {
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid realm or project")
	}
	f := listFilter{realm: realm, project: project}
	var out []names.ProjectRef
	for _, ref := range d.current().refs {
		if f.match(ref) {
			out = append(out, ref)
		}
	}
	return out, nil
}

func (d *Daemon) apiSessions(ctx context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.SessionsListParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.After != "" && !names.ValidSessionID(p.After) {
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid after")
	}
	d.fresh()
	refs, err := d.matchRefs(p.Realm, p.Project)
	if err != nil {
		return nil, err
	}
	var res proto.SessionsListResult
	if p.Sync {
		sctx, cancel := context.WithTimeout(ctx, syncTimeout)
		e := d.current()
		for _, ref := range refs {
			if _, err := d.refresh(sctx, e, ref); err != nil {
				d.noteInvalid(ref, 0, err.Error())
			}
		}
		cancel()
	}
	res.Warnings = d.takeWarnings(refs)
	limit := p.Limit
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	views := d.hub.views(listFilter{realm: p.Realm, project: p.Project, live: p.Live, since: p.Since})
	i := sort.Search(len(views), func(i int) bool { return views[i].Session > p.After })
	size := 0
	res.Sessions = []proto.SessionView{}
	for ; i < len(views); i++ {
		v := views[i]
		d.decorate(&v)
		b, _ := json.Marshal(v)
		if len(res.Sessions) >= limit || (size+len(b) > maxPageBytes && len(res.Sessions) > 0) {
			res.More = true
			break
		}
		size += len(b)
		res.Sessions = append(res.Sessions, v)
	}
	return res, nil
}

// decorate adds what the daemon knows beyond the journal.
func (d *Daemon) decorate(v *proto.SessionView) {
	v.Machine = d.machine
	ref, err := names.Parse(v.Ref)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if bi := d.boxes[ref]; bi != nil {
		v.BoxState = bi.state
	}
	if v.Live {
		v.Window = d.windows[ref][v.Session]
	}
}

func (d *Daemon) apiSession(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.SessionsGetParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if !names.ValidSessionID(p.Session) {
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid session")
	}
	e, ok := d.hub.lookup(p.Session)
	if !ok {
		return nil, proto.Errorf(proto.CodeNotFound, "no session %s", p.Session)
	}
	n := p.Events
	if n <= 0 {
		n = defaultRecent
	}
	evs, err := lastEvents(e.file, min(n, maxRecent), maxPageBytes)
	if err != nil {
		return nil, err
	}
	if evs == nil {
		evs = []proto.Event{}
	}
	v := e.view
	d.decorate(&v)
	return proto.SessionsGetResult{Session: v, Events: evs}, nil
}

func (d *Daemon) apiEvents(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.EventsReadParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if !names.ValidSessionID(p.Session) || p.From < 0 {
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid session or from")
	}
	e, ok := d.hub.lookup(p.Session)
	if !ok {
		return nil, proto.Errorf(proto.CodeNotFound, "no session %s", p.Session)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = maxPage
	}
	evs, more, err := readEvents(e.file, p.From, min(limit, maxPage), maxPageBytes)
	if err != nil {
		return nil, err
	}
	res := proto.EventsReadResult{Ref: e.ref.String(), Events: evs, LastSeq: e.view.LastSeq, More: more}
	if res.Events == nil {
		res.Events = []proto.Event{}
	}
	for _, ev := range evs {
		res.LastSeq = max(res.LastSeq, ev.Seq)
	}
	return res, nil
}

func (d *Daemon) apiSubscribe(_ context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.SubscribeParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if c.Get("subscribed") != nil {
		return nil, proto.Errorf(proto.CodeRefused, "this connection already has a subscription")
	}
	c.Set("subscribed", true)
	if err := d.hub.subscribe(c, p); err != nil {
		return nil, err
	}
	return proto.SubscribeResult{Instance: d.instance}, nil
}
