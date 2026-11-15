package hub

// Validation of what machines send. A machine is authenticated, but it may
// be compromised; nothing it sends is stored before it passed these checks,
// and it can only ever write under its own registry name.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/requests"
)

const (
	maxViewBytes = 64 << 10
	maxBoxes     = 1000
)

func okText(s string, max int) bool { return len(s) <= max && utf8.ValidString(s) }

func validTS(s string, required bool) bool {
	if s == "" {
		return !required
	}
	if len(s) > 64 {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

// cleanEvent validates an event notification and returns the event with
// canonically re-encoded data.
func cleanEvent(n proto.EventNote) (proto.Event, names.ProjectRef, error) {
	ref, err := names.Parse(n.Ref)
	if err != nil {
		return proto.Event{}, ref, errors.New("invalid project")
	}
	ev, err := cleanEventOnly(n.Event)
	return ev, ref, err
}

func cleanEventOnly(e proto.Event) (proto.Event, error) {
	switch {
	case e.V != proto.Version:
		return e, errors.New("unknown event version")
	case e.Seq <= 0:
		return e, errors.New("invalid seq")
	case !names.ValidSessionID(e.Session):
		return e, errors.New("invalid session")
	case !proto.Known(e.Type):
		return e, errors.New("unknown event type")
	case !validTS(e.TS, true) || !validTS(e.At, false):
		return e, errors.New("invalid timestamp")
	}
	data, err := proto.ValidateData(e.Type, e.Data)
	if err != nil {
		return e, fmt.Errorf("%s: %w", e.Type, err)
	}
	return proto.Event{V: e.V, Seq: e.Seq, TS: e.TS, At: e.At, Session: e.Session, Type: e.Type, Data: data}, nil
}

var viewStates = map[string]bool{proto.StateIdle: true, proto.StateWorking: true, proto.StateBlocked: true, proto.StateDone: true,
	proto.StateError: true, "ended": true, "unknown": true}

var boxStates = map[string]bool{"": true, "running": true, "stopped": true, "absent": true}

// cleanView validates a session view from a machine and names the machine.
func cleanView(v *proto.SessionView, machine string) error {
	switch {
	case !names.ValidSessionID(v.Session):
		return errors.New("invalid session")
	case !names.ValidRealm(v.Realm) || !names.Valid(v.Project) || v.Ref != v.Realm+"/"+v.Project:
		return errors.New("invalid project")
	case !viewStates[v.State] || !boxStates[v.BoxState]:
		return errors.New("invalid state")
	case !okText(v.Harness, 64) || !okText(v.Worktree, 64) || !okText(v.Window, 64) || !okText(v.Kind, proto.MaxKind) || !okText(v.EndReason, proto.MaxReason):
		return errors.New("invalid field")
	case v.Events < 0 || v.LastSeq < 0 || v.Turn < 0 || v.DoneTurn < 0 || v.SeenTurn < 0:
		return errors.New("invalid counter")
	}
	if v.Question != nil {
		b, _ := json.Marshal(v.Question)
		if _, err := proto.ValidateData(proto.TypeQuestion, b); err != nil {
			return errors.New("invalid question")
		}
	}
	v.Machine = machine
	if b, _ := json.Marshal(v); len(b) > maxViewBytes {
		return errors.New("view too large")
	}
	return nil
}

var requestStatuses = map[string]bool{proto.RequestPending: true, proto.RequestApproved: true, proto.RequestDenied: true,
	proto.RequestDone: true, proto.RequestFailed: true, proto.RequestExpired: true}

var origins = map[string]bool{"": true, proto.OriginLocal: true, proto.OriginAuto: true, proto.OriginHub: true}

// cleanRequest validates a request from a machine.
func cleanRequest(r *proto.Request) error {
	if !proto.ValidRequestID(r.ID) {
		return errors.New("invalid request ID")
	}
	if _, err := names.Parse(r.Ref); err != nil {
		return errors.New("invalid project")
	}
	switch {
	case proto.RequestRisk(r.Kind) == "" || r.Risk != proto.RequestRisk(r.Kind) || r.Params.Kind != r.Kind:
		return errors.New("invalid kind or risk")
	case !requestStatuses[r.Status] || !origins[r.DecidedBy]:
		return errors.New("invalid status")
	case r.Session != "" && !names.ValidSessionID(r.Session):
		return errors.New("invalid session")
	case r.Params.Session != "":
		return errors.New("invalid parameters")
	case !okText(r.Result, 8000) || !okText(r.Reason, 2000):
		return errors.New("invalid text")
	}
	if _, err := requests.Validate(r.Params); err != nil {
		return err
	}
	return nil
}

// cleanBoxes validates a machine's box list (invalid entries are dropped).
func cleanBoxes(in []proto.BoxView) []proto.BoxView {
	out := []proto.BoxView{}
	for _, b := range in {
		if len(out) >= maxBoxes {
			break
		}
		if !names.ValidRealm(b.Realm) || !names.Valid(b.Project) || b.Ref != b.Realm+"/"+b.Project || !boxStates[b.State] ||
			!okText(b.Box, 256) || !okText(b.ContainerID, 64) || !okText(b.Channel, 32) || !okText(b.ChannelDetail, 1000) || !okText(b.HelperVersion, 128) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// cleanInfo validates daemon.info.
func cleanInfo(in *proto.DaemonInfo) error {
	if !okText(in.Version, 128) || !okText(in.Machine, 255) || !okText(in.Instance, 64) {
		return errors.New("invalid daemon info")
	}
	in.Boxes = cleanBoxes(in.Boxes)
	return nil
}
