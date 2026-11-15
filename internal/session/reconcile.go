package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/tmux"
)

// Live is one session a box reports alive (`devc box session ps --json`).
type Live struct {
	Session string `json:"session"`
	Source  string `json:"source"`
	Window  string `json:"window,omitempty"`
	Pane    string `json:"pane,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

// maxLiveOutput bounds the report a box prints.
const maxLiveOutput = 1 << 20

// ParseLive validates the report of `devc box session ps --json`. The box
// controls this output, so anything unexpected rejects the whole report:
// reconciliation then does nothing rather than act on a guess.
func ParseLive(out []byte) ([]Live, error) {
	if len(out) > maxLiveOutput {
		return nil, errors.New("the live-session report is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	var ls []Live
	if err := dec.Decode(&ls); err != nil {
		return nil, fmt.Errorf("parsing the live-session report: %w", err)
	}
	if len(ls) > MaxSpoolFiles {
		return nil, errors.New("the live-session report lists too many sessions")
	}
	for _, l := range ls {
		switch {
		case !names.ValidSessionID(l.Session):
			return nil, fmt.Errorf("the live-session report names an invalid session %q", proto.Printable(proto.Truncate(l.Session, 40)))
		case l.Source != "tmux" && l.Source != "process":
			return nil, errors.New("the live-session report has an invalid source")
		case l.Window != "" && !tmux.ValidWindowID(l.Window), l.Pane != "" && !tmux.ValidPaneID(l.Pane), l.PID < 0:
			return nil, errors.New("the live-session report has an invalid window, pane or pid")
		case l.Source == "tmux" && l.Window == "":
			return nil, errors.New("the live-session report has a tmux session without a window")
		}
	}
	return ls, nil
}

// QueryLive asks the box for its live sessions.
func QueryLive(ctx context.Context, eng engine.Engine, container string) ([]Live, error) {
	res, err := eng.Exec(ctx, engine.ExecOptions{
		Container: container, Workdir: "/", Timeout: 30 * time.Second,
		Cmd: []string{paths.InBoxHelper, "box", "session", "ps", "--json"},
	})
	if err != nil {
		return nil, fmt.Errorf("asking %s for its sessions: %w", container, err)
	}
	return ParseLive(res.Stdout)
}

// openSessions lists the sessions whose journals show no end.
func (s *Store) openSessions(ref names.ProjectRef) (map[string]bool, error) {
	ids, err := journalSessions(s.Paths, ref)
	if err != nil {
		return nil, err
	}
	open := map[string]bool{}
	for _, id := range ids {
		js, err := loadJournalState(s.Paths.JournalFile(ref, id))
		if err != nil {
			return nil, err
		}
		if !js.ended {
			open[id] = true
		}
	}
	return open, nil
}

// endSessions writes session.end with reason for every session in
// candidates that is still open and not in live. The caller holds the lock.
func (s *Store) endSessions(ref names.ProjectRef, candidates map[string]bool, live []Live, reason string) (int, error) {
	alive := map[string]bool{}
	for _, l := range live {
		alive[l.Session] = true
	}
	open, err := s.openSessions(ref)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for id := range candidates {
		if !open[id] || alive[id] {
			continue
		}
		if err := s.AppendHost(ref, id, hostEvent{proto.TypeSessionEnd, proto.SessionEnd{Reason: reason}}); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// hasState reports whether a project has journals or a runtime directory.
func (s *Store) hasState(ref names.ProjectRef) bool {
	if ids, err := journalSessions(s.Paths, ref); err != nil || len(ids) > 0 {
		return true
	}
	rt, err := s.Paths.BoxRuntimeDir(names.BoxID(ref))
	if err != nil {
		return false
	}
	_, err = os.Lstat(rt)
	return err == nil
}

// BoxState is what a refresh needs to know about the box.
type BoxState struct {
	// Running is true when the container runs; Ready when box init has
	// reported ready, so its sessions can be asked for.
	Running   bool
	Ready     bool
	Container string
}

// RefreshResult is what Refresh did and saw.
type RefreshResult struct {
	Report IngestReport
	// Live is the box's report, nil when it was not asked.
	Live  []Live
	Ended int // sessions reconciliation ended
	// Warning is set when the box could not be asked for its sessions.
	Warning string
}

// Refresh ingests the spool and reconciles the journals of one box: in a
// ready box, sessions that the journal shows open but the box no longer
// runs end with reason "lost"; when the box does not run at all, every
// open session ends so. To avoid a race with sessions that start meanwhile,
// only sessions already open before the box was asked are reconciled, and
// the spool is ingested again after asking, so an end the box wrote in
// between wins over "lost". Refresh takes the ingestion lock.
func (s *Store) Refresh(ctx context.Context, ref names.ProjectRef, st BoxState, eng engine.Engine) (RefreshResult, error) {
	return s.refresh(ctx, ref, st, eng, proto.ReasonLost)
}

// RefreshStopped ingests and ends every open session with reason, for a box
// that has just been stopped.
func (s *Store) RefreshStopped(ctx context.Context, ref names.ProjectRef, reason string) (RefreshResult, error) {
	return s.refresh(ctx, ref, BoxState{}, nil, reason)
}

func (s *Store) refresh(ctx context.Context, ref names.ProjectRef, st BoxState, eng engine.Engine, reason string) (RefreshResult, error) {
	var res RefreshResult
	if !st.Running && !s.hasState(ref) {
		return res, nil // never had a box or a session: nothing to do
	}
	unlock, err := s.Lock(ctx, ref)
	if err != nil {
		return res, err
	}
	defer unlock()
	rep, err := s.Ingest(ref)
	res.Report = rep
	if err != nil {
		return res, err
	}
	switch {
	case st.Running && !st.Ready:
		return res, nil
	case st.Running:
		before, err := s.openSessions(ref)
		if err != nil {
			return res, err
		}
		live, err := QueryLive(ctx, eng, st.Container)
		if err != nil {
			res.Warning = err.Error()
			return res, nil
		}
		res.Live = live
		rep, err := s.Ingest(ref)
		res.Report.add(rep)
		if err != nil {
			return res, err
		}
		res.Ended, err = s.endSessions(ref, before, live, reason)
		return res, err
	default:
		open, err := s.openSessions(ref)
		if err != nil {
			return res, err
		}
		res.Ended, err = s.endSessions(ref, open, nil, reason)
		return res, err
	}
}
