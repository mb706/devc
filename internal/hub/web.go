package hub

// The API the web app needs beyond C11a (work order C12 section 3):
// projects and changes, forwarded to the machine's projects.list and
// changes.get. Like everything a machine sends, the answers are decoded
// strictly and checked before they reach a browser; invalid entries are
// dropped and texts made printable.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

const (
	maxProjects  = 1000
	maxHarnesses = 64
	maxWorktrees = 1000
	// webTimeout bounds a projects or changes call to a machine.
	webTimeout = 75 * time.Second
)

var hexRE = regexp.MustCompile(`^[0-9a-f]{0,64}$`)

func okRef(s string) bool { return s == "" || names.CheckRef(s) == nil }

// MachineProjects is one machine's part of GET /api/projects.
type MachineProjects struct {
	Machine  string              `json:"machine"`
	Online   bool                `json:"online"`
	Projects []proto.ProjectView `json:"projects"`
	Error    string              `json:"error,omitempty"`
}

func (s *Server) apiProjects(w http.ResponseWriter, r *http.Request) {
	machine := r.URL.Query().Get("machine")
	if machine != "" && !validMachine(machine) {
		writeError(w, http.StatusBadRequest, "invalid machine")
		return
	}
	ms, err := s.st.Machines()
	if err != nil {
		s.internal(w, err)
		return
	}
	var out []MachineProjects
	for _, m := range ms {
		if machine == "" || m.Name == machine {
			out = append(out, MachineProjects{Machine: m.Name, Projects: []proto.ProjectView{}})
		}
	}
	if machine != "" && len(out) == 0 {
		writeError(w, http.StatusNotFound, "no machine %s", machine)
		return
	}
	var wg sync.WaitGroup
	for i := range out {
		l := s.link(out[i].Machine)
		if l == nil {
			out[i].Error = "the machine is offline"
			continue
		}
		out[i].Online = true
		wg.Add(1)
		go func(mp *MachineProjects) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), webTimeout)
			defer cancel()
			raw, err := l.forward(ctx, proto.MethodProjectsList, proto.ProjectsListParams{})
			var res proto.ProjectsListResult
			if err == nil {
				err = proto.DecodeResult(raw, &res)
			}
			if err != nil {
				_, msg, _ := rpcStatus(err)
				mp.Error = msg
				return
			}
			mp.Projects = cleanProjects(res.Projects)
		}(&out[i])
	}
	wg.Wait()
	if out == nil {
		out = []MachineProjects{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": out})
}

// cleanProjects validates a machine's project list.
func cleanProjects(in []proto.ProjectView) []proto.ProjectView {
	out := []proto.ProjectView{}
	for _, p := range in {
		if len(out) >= maxProjects {
			break
		}
		if !names.ValidRealm(p.Realm) || !names.Valid(p.Project) || p.Ref != p.Realm+"/"+p.Project || !boxStates[p.Box] ||
			!okRef(p.DefaultBranch) || p.Live < 0 || p.NeedsYou < 0 {
			continue
		}
		p.Problem = proto.Printable(proto.Truncate(p.Problem, 500))
		p.Error = proto.Printable(proto.Truncate(p.Error, 500))
		hs := []proto.HarnessView{}
		for _, h := range p.Harnesses {
			if names.Valid(h.ID) && len(hs) < maxHarnesses {
				hs = append(hs, h)
			}
		}
		p.Harnesses = hs
		wts := []proto.WorktreeView{}
		for _, wt := range p.Worktrees {
			if len(wts) >= maxWorktrees || !names.ValidKind(names.KindWorktree, wt.Name) || !okRef(wt.Branch) || !hexRE.MatchString(wt.Head) || wt.Sessions < 0 {
				continue
			}
			ok := true
			for _, d := range []*proto.Divergence{wt.Origin, wt.Upstream} {
				if d != nil && (d.Ahead < 0 || d.Behind < 0 || !okText(d.Ref, 300)) {
					ok = false
				}
				if d != nil {
					d.Ref = proto.Printable(d.Ref)
				}
			}
			if !ok {
				continue
			}
			wt.Error = proto.Printable(proto.Truncate(wt.Error, 500))
			wts = append(wts, wt)
		}
		p.Worktrees = wts
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// ChangesReply is the answer of GET /api/changes/<machine>/<session>.
type ChangesReply struct {
	Machine string `json:"machine"`
	Session string `json:"session"`
	proto.ChangesResult
}

func (s *Server) apiChanges(w http.ResponseWriter, r *http.Request) {
	machine, sid := r.PathValue("machine"), r.PathValue("session")
	if !validMachine(machine) || !names.ValidSessionID(sid) {
		writeError(w, http.StatusBadRequest, "invalid machine or session")
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = proto.ScopeSnapshot
	}
	if scope != proto.ScopeSnapshot && scope != proto.ScopeBranch {
		writeError(w, http.StatusBadRequest, "scope must be %s or %s", proto.ScopeSnapshot, proto.ScopeBranch)
		return
	}
	l := s.link(machine)
	if l == nil {
		s.offline(w, machine)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webTimeout)
	defer cancel()
	raw, err := l.forward(ctx, proto.MethodChangesGet, proto.ChangesParams{Session: sid, Ref: s.sessionRef(machine, sid), Scope: scope})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	var res proto.ChangesResult
	if err := proto.DecodeResult(raw, &res); err != nil || cleanChanges(&res, scope) != nil {
		writeError(w, http.StatusBadGateway, "the machine sent invalid changes")
		return
	}
	writeJSON(w, http.StatusOK, ChangesReply{Machine: machine, Session: sid, ChangesResult: res})
}

// cleanChanges validates a changes.get answer.
func cleanChanges(c *proto.ChangesResult, scope string) error {
	switch {
	case c.Scope != scope:
		return errors.New("wrong scope")
	case !names.ValidKind(names.KindWorktree, c.Worktree) || !okRef(c.Branch):
		return errors.New("invalid worktree or branch")
	case !hexRE.MatchString(c.Base) || !hexRE.MatchString(c.Head):
		return errors.New("invalid commit")
	case len(c.Files) > proto.MaxChangedFiles || c.FilesMore < 0 || c.Added < 0 || c.Deleted < 0:
		return errors.New("invalid file list")
	case len(c.Diff) > proto.MaxDiffBytes*2:
		// Printable may grow the text (replacement characters); twice the
		// cap is plenty.
		return errors.New("diff too large")
	}
	for i := range c.Files {
		f := &c.Files[i]
		if f.Added < 0 || f.Deleted < 0 || f.Path == "" {
			return errors.New("invalid file")
		}
		f.Path = proto.Printable(proto.Truncate(f.Path, proto.MaxPath))
		f.OldPath = proto.Printable(proto.Truncate(f.OldPath, proto.MaxPath))
	}
	if c.Files == nil {
		c.Files = []proto.ChangedFile{}
	}
	c.BaseLabel = proto.Printable(proto.Truncate(c.BaseLabel, 200))
	c.HeadLabel = proto.Printable(proto.Truncate(c.HeadLabel, 200))
	c.Note = proto.Printable(proto.Truncate(c.Note, 500))
	c.Diff = proto.PrintableText(c.Diff)
	return nil
}
