// The inbox (work order C11a section 4) is derived from the projection on
// every request, never stored as truth.

package hub

import (
	"sort"

	"github.com/mb706/devc/internal/proto"
)

// Inbox sections in order of precedence.
const (
	SectionNeedsYou = "needs-you"
	SectionFailed   = "failed"
	SectionFinished = "finished"
	SectionWorking  = "working"
	SectionIdle     = "idle"
)

// Sections lists the sections in display order.
var Sections = []string{SectionNeedsYou, SectionFailed, SectionFinished, SectionWorking, SectionIdle}

// SectionOf places a session in the inbox: the section, or "" when the
// session is not in the inbox. pending says the session has a pending
// request.
//
// A live session is always in exactly one section: needs-you when blocked
// or with a pending request, failed in state error, finished when a turn
// reached done that nobody marked seen, working, else idle. An ended
// session stays while it has a pending request (needs-you) or a turn that
// was not marked seen: failed when it ended as lost, else finished. Once
// seen, it leaves.
func SectionOf(v *proto.SessionView, pending bool) string {
	if !v.Live {
		switch {
		case pending:
			return SectionNeedsYou
		case v.Turn <= v.SeenTurn:
			return ""
		case v.EndReason == proto.ReasonLost:
			return SectionFailed
		}
		return SectionFinished
	}
	switch {
	case v.State == proto.StateBlocked || pending:
		return SectionNeedsYou
	case v.State == proto.StateError:
		return SectionFailed
	case v.DoneTurn > v.SeenTurn:
		return SectionFinished
	case v.State == proto.StateWorking:
		return SectionWorking
	}
	return SectionIdle
}

// SessionSummary is a session as the API shows it: its view (Machine is the
// hub's name for the machine), whether the machine is offline (Stale), its
// pending requests and its inbox section ("" when not in the inbox).
type SessionSummary struct {
	proto.SessionView
	Stale    bool   `json:"stale"`
	Requests int    `json:"requests"`
	Section  string `json:"section"`
}

func summarize(r SessionRow, pending int) SessionSummary {
	v := r.View
	v.Machine = r.Machine
	return SessionSummary{SessionView: v, Stale: !r.Online, Requests: pending, Section: SectionOf(&v, pending > 0)}
}

// InboxSection is one section with its sessions.
type InboxSection struct {
	Name     string           `json:"name"`
	Sessions []SessionSummary `json:"sessions"`
}

// BuildInbox sorts sessions into the sections, most recent change first
// within each.
func BuildInbox(sessions []SessionSummary) []InboxSection {
	by := map[string][]SessionSummary{}
	for _, s := range sessions {
		if s.Section != "" {
			by[s.Section] = append(by[s.Section], s)
		}
	}
	out := make([]InboxSection, 0, len(Sections))
	for _, name := range Sections {
		list := by[name]
		sortRecent(list)
		if list == nil {
			list = []SessionSummary{}
		}
		out = append(out, InboxSection{Name: name, Sessions: list})
	}
	return out
}

// sortRecent orders by last change, newest first.
func sortRecent(list []SessionSummary) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !a.LastEvent.Equal(b.LastEvent) {
			return a.LastEvent.After(b.LastEvent)
		}
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		return a.Session > b.Session
	})
}
