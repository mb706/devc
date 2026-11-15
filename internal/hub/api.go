package hub

// The HTTP API (work order C11a section 5): JSON over HTTPS for device
// certificates. Reads come from the projection; commands and decisions are
// forwarded to the machine with the client's ID, and the machine's answer
// (its stored receipt) is returned. A machine that is not connected gets
// nothing queued: the request fails at once.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// Limits of API answers.
const (
	defaultEvents   = 100
	maxEvents       = 500
	defaultSessions = 500
	maxSessions     = 5000
	maxWait         = 60
	forwardTimeout  = 90 * time.Second
)

// validMachine reports whether s may name a machine (registry names follow
// the name rules).
func validMachine(s string) bool { return names.Valid(s) }

func (s *Server) apiMachines(w http.ResponseWriter, _ *http.Request) {
	ms, err := s.st.Machines()
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": ms})
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.log.Error("api", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// summaries returns the matching sessions as the API shows them.
func (s *Server) summaries(f SessionFilter) ([]SessionSummary, error) {
	rows, err := s.st.Sessions(f)
	if err != nil {
		return nil, err
	}
	pending, err := s.st.PendingBySession()
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, summarize(r, pending[[2]string{r.Machine, r.View.Session}]))
	}
	return out, nil
}

// streamPosition is the change ID a client should resume a stream from
// after reading a snapshot.
func (s *Server) streamPosition() int64 {
	_, hi, _ := s.st.ChangeRange()
	return hi
}

func (s *Server) apiInbox(w http.ResponseWriter, r *http.Request) {
	pos := s.streamPosition()
	f, ok := s.filter(w, r)
	if !ok {
		return
	}
	list, err := s.summaries(f)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sections": BuildInbox(list), "stream_id": pos})
}

// filter reads the machine, realm, project and live parameters.
func (s *Server) filter(w http.ResponseWriter, r *http.Request) (SessionFilter, bool) {
	q := r.URL.Query()
	f := SessionFilter{Machine: q.Get("machine"), Realm: q.Get("realm"), Project: q.Get("project")}
	if (f.Machine != "" && !validMachine(f.Machine)) || (f.Realm != "" && !names.ValidRealm(f.Realm)) || (f.Project != "" && !names.Valid(f.Project)) {
		writeError(w, http.StatusBadRequest, "invalid machine, realm or project")
		return f, false
	}
	switch q.Get("live") {
	case "":
	case "true", "1":
		t := true
		f.Live = &t
	case "false", "0":
		t := false
		f.Live = &t
	default:
		writeError(w, http.StatusBadRequest, "live must be true or false")
		return f, false
	}
	return f, true
}

func intParam(r *http.Request, name string, def, lo, hi int64) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be a number from %d to %d", name, lo, hi)
	}
	return n, nil
}

func (s *Server) apiSessions(w http.ResponseWriter, r *http.Request) {
	pos := s.streamPosition()
	f, ok := s.filter(w, r)
	if !ok {
		return
	}
	limit, err := intParam(r, "limit", defaultSessions, 1, maxSessions)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	list, err := s.summaries(f)
	if err != nil {
		s.internal(w, err)
		return
	}
	sortRecent(list)
	more := false
	if int64(len(list)) > limit {
		list, more = list[:limit], true
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list, "more": more, "stream_id": pos})
}

// SessionDetail is the answer of GET /api/sessions/<machine>/<session>.
type SessionDetail struct {
	Session SessionSummary `json:"session"`
	Events  []proto.Event  `json:"events"`
	// More says older events exist; Partial that the machine could not be
	// asked for events the hub does not hold.
	More     bool  `json:"more"`
	Partial  bool  `json:"partial,omitempty"`
	StreamID int64 `json:"stream_id"`
}

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	pos := s.streamPosition()
	machine, sid := r.PathValue("machine"), r.PathValue("session")
	if !validMachine(machine) || !names.ValidSessionID(sid) {
		writeError(w, http.StatusBadRequest, "invalid machine or session")
		return
	}
	before, err := intParam(r, "before", 0, 0, 1<<62)
	if err == nil {
		var limit int64
		limit, err = intParam(r, "limit", defaultEvents, 1, maxEvents)
		if err == nil {
			s.sessionDetail(w, r, machine, sid, before, int(limit), pos)
			return
		}
	}
	writeError(w, http.StatusBadRequest, "%s", err.Error())
}

func (s *Server) sessionDetail(w http.ResponseWriter, r *http.Request, machine, sid string, before int64, limit int, pos int64) {
	list, err := s.summaries(SessionFilter{Machine: machine, Session: sid})
	if err != nil {
		s.internal(w, err)
		return
	}
	if len(list) == 0 {
		writeError(w, http.StatusNotFound, "no session %s on %s", sid, machine)
		return
	}
	sum := list[0]
	evs, err := s.st.Events(machine, sid, before, limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	d := SessionDetail{Session: sum, Events: evs, StreamID: pos}
	upper := before
	if upper <= 0 {
		upper = sum.LastSeq + 1
	}
	// When paging backwards, the hub's tail may not reach back far enough:
	// ask the machine for the events below the oldest one held. (The most
	// recent events come from the tail alone; More tells the client to page.)
	lowest := upper
	if len(evs) > 0 {
		lowest = evs[0].Seq
	}
	if before > 0 && len(evs) < limit && lowest > 1 {
		want := limit - len(evs)
		older, ok := s.readOlder(r.Context(), machine, sid, sum.Ref, lowest, want)
		d.Events = append(older, d.Events...)
		d.Partial = !ok
	}
	if d.Events == nil {
		d.Events = []proto.Event{}
	}
	if len(d.Events) > 0 {
		d.More = d.Events[0].Seq > 1
	} else {
		d.More = upper > 1 && d.Partial
	}
	writeJSON(w, http.StatusOK, d)
}

// Bounds of reading older events from a machine (variables for tests).
var (
	// olderPageBytes bounds the events one backward page returns.
	olderPageBytes = 1 << 20
	// olderReadBytes and olderReads bound what the hub reads for one page.
	olderReadBytes = 16 << 20
	olderReads     = 64
)

// readOlder returns up to want events of a session immediately below seq
// lowest, from the machine. The machine's events.read goes forward and
// stops at its own byte cap (More), so the hub follows it up to lowest-1,
// keeping the most recent events within olderPageBytes: a page always ends
// right below lowest and the next one starts below its first event, so
// paging never skips (finding C2-08). ok is false when the machine could
// not be asked or broke a bound; the page is then empty.
func (s *Server) readOlder(ctx context.Context, machine, sid, ref string, lowest int64, want int) ([]proto.Event, bool) {
	l := s.link(machine)
	if l == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	from := max(0, lowest-1-int64(want))
	var out []proto.Event
	sizes := []int{}
	pageBytes, readBytes := 0, 0
	for reads := 0; ; reads++ {
		if reads >= olderReads {
			return nil, false
		}
		raw, err := l.forward(ctx, proto.MethodEventsRead, proto.EventsReadParams{Session: sid, Ref: ref, From: from, Limit: int(lowest - 1 - from)})
		var res proto.EventsReadResult
		if err != nil || proto.DecodeResult(raw, &res) != nil {
			return nil, false
		}
		if readBytes += len(raw); readBytes > olderReadBytes {
			return nil, false
		}
		progress := false
		for _, e := range res.Events {
			ev, err := cleanEventOnly(e)
			if err != nil || ev.Session != sid || ev.Seq <= from || ev.Seq >= lowest {
				continue
			}
			b, _ := json.Marshal(ev)
			out, sizes = append(out, ev), append(sizes, len(b))
			pageBytes += len(b)
			from, progress = ev.Seq, true
		}
		for len(out) > 1 && pageBytes > olderPageBytes {
			pageBytes -= sizes[0]
			out, sizes = out[1:], sizes[1:]
		}
		if from >= lowest-1 || !res.More {
			break
		}
		if !progress {
			// More is said, but nothing below lowest came: a machine that
			// does not advance.
			return nil, false
		}
	}
	if len(out) > want {
		out = out[len(out)-want:]
	}
	return out, true
}

// commandMethods maps the command types of POST /api/commands to the
// daemon's methods.
var commandMethods = map[string]string{
	proto.CommandSend:      proto.MethodSessionSend,
	proto.CommandAnswer:    proto.MethodSessionAnswer,
	proto.CommandInterrupt: proto.MethodSessionInterrupt,
	proto.CommandKeys:      proto.MethodSessionKeys,
	proto.CommandSeen:      proto.MethodSessionSeen,
	proto.CommandStart:     proto.MethodSessionStart,
	proto.CommandStop:      proto.MethodSessionStop,
}

// CommandRequest is the body of POST /api/commands: the machine, the
// command type, the client's command ID, the parameters of the type (as
// the daemon's method takes them, without the ID) and how long to wait
// for a final receipt (seconds, at most 60).
type CommandRequest struct {
	Machine string          `json:"machine"`
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Params  json.RawMessage `json:"params"`
	Wait    int             `json:"wait,omitempty"`
}

// decodeBody decodes a JSON body strictly.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "the request is too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: %s", proto.Printable(proto.Truncate(err.Error(), 200)))
		return false
	}
	// Only white space may follow. Reading on to the end of the body
	// (bounded by the size limit and the body deadline) also lifts that
	// deadline (front.go).
	rest, _ := io.ReadAll(io.MultiReader(dec.Buffered(), io.LimitReader(r.Body, 64)))
	if len(bytes.TrimSpace(rest)) > 0 {
		writeError(w, http.StatusBadRequest, "invalid JSON body: data after the object")
		return false
	}
	return true
}

// commandParams decodes the parameters of a command type strictly and sets
// the client's ID.
func commandParams(typ, id string, raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	var target any
	switch typ {
	case proto.CommandSend:
		p := &proto.SendParams{}
		target = p
		defer func() { p.ID = id }()
	case proto.CommandAnswer:
		p := &proto.AnswerParams{}
		target = p
		defer func() { p.ID = id }()
	case proto.CommandInterrupt, proto.CommandStop:
		p := &proto.SessionCommandParams{}
		target = p
		defer func() { p.ID = id }()
	case proto.CommandKeys:
		p := &proto.KeysParams{}
		target = p
		defer func() { p.ID = id }()
	case proto.CommandSeen:
		p := &proto.SeenParams{}
		target = p
		defer func() { p.ID = id }()
	case proto.CommandStart:
		p := &proto.StartParams{}
		target = p
		defer func() { p.ID = id }()
	default:
		return nil, errors.New("unknown command type")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, errors.New("params must be an object")
	}
	if _, ok := probe["id"]; ok {
		return nil, errors.New("give the command ID as id beside params, not in them")
	}
	if err := proto.DecodeParams(raw, target); err != nil {
		return nil, err
	}
	return target, nil
}

// commandRef names the session's project in a command's parameters.
func (s *Server) commandRef(machine string, params any) error {
	switch p := params.(type) {
	case *proto.SendParams:
		return s.fillRef(machine, &p.Ref, p.Session)
	case *proto.AnswerParams:
		return s.fillRef(machine, &p.Ref, p.Session)
	case *proto.SessionCommandParams:
		return s.fillRef(machine, &p.Ref, p.Session)
	case *proto.KeysParams:
		return s.fillRef(machine, &p.Ref, p.Session)
	case *proto.SeenParams:
		return s.fillRef(machine, &p.Ref, p.Session)
	}
	return nil
}

// rpcStatus maps a machine's error to an HTTP status. uncertain says the
// machine may have received the call although no answer came: the client
// keeps the command's ID and asks for its receipt (finding C2-11).
func rpcStatus(err error) (status int, msg string, uncertain bool) {
	var e *proto.Error
	if errors.As(err, &e) {
		msg := proto.Printable(proto.Truncate(e.Message, 500))
		switch e.Code {
		case proto.CodeInvalidParams, proto.CodeInvalidRequest, proto.CodeParseError:
			return http.StatusBadRequest, msg, false
		case proto.CodeNotFound:
			return http.StatusNotFound, msg, false
		case proto.CodeRefused:
			return http.StatusForbidden, msg, false
		case proto.CodeBusy:
			return http.StatusTooManyRequests, msg, false
		case proto.CodeMethodNotFound:
			return http.StatusNotImplemented, "the machine does not offer this to the hub", false
		}
		return http.StatusBadGateway, "the machine failed: " + msg, false
	}
	if errors.Is(err, errOffline) {
		return http.StatusServiceUnavailable, "the machine went offline; the command may or may not have arrived (ask for its receipt, or retry with the same ID)", true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "the machine did not answer in time (ask for the receipt, or retry with the same ID)", true
	}
	return http.StatusBadGateway, "the machine could not be asked", true
}

// writeRPCError answers with a machine's error; an uncertain outcome says
// so in the body ("uncertain": true).
func writeRPCError(w http.ResponseWriter, err error) {
	st, msg, uncertain := rpcStatus(err)
	if uncertain {
		writeJSON(w, st, map[string]any{"error": msg, "uncertain": true})
		return
	}
	writeError(w, st, "%s", msg)
}

// sessionRef is the project of a session the hub holds ("" when it holds
// none): the hub names it on every call about the session (finding C1-01).
func (s *Server) sessionRef(machine, sid string) string {
	rows, err := s.st.Sessions(SessionFilter{Machine: machine, Session: sid})
	if err != nil || len(rows) != 1 {
		return ""
	}
	return rows[0].View.Ref
}

// fillRef checks a client's ref, or fills in the one the hub holds for the
// session.
func (s *Server) fillRef(machine string, ref *string, sid string) error {
	if *ref != "" {
		if _, err := names.Parse(*ref); err != nil {
			return errors.New("invalid ref: use <realm>/<project>")
		}
		return nil
	}
	if sid != "" {
		*ref = s.sessionRef(machine, sid)
	}
	return nil
}

// offline answers for a machine that is not connected.
func (s *Server) offline(w http.ResponseWriter, machine string) {
	m, ok, _ := s.st.Machine(machine)
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, "no machine %s", machine)
	case m.LastSeen != nil:
		writeError(w, http.StatusServiceUnavailable, "machine %s is offline (last seen %s); the hub does not queue commands", machine, m.LastSeen.UTC().Format(time.RFC3339))
	default:
		writeError(w, http.StatusServiceUnavailable, "machine %s is offline; the hub does not queue commands", machine)
	}
}

func (s *Server) apiCommand(w http.ResponseWriter, r *http.Request) {
	var c CommandRequest
	if !decodeBody(w, r, &c) {
		return
	}
	method, ok := commandMethods[c.Type]
	switch {
	case !validMachine(c.Machine):
		writeError(w, http.StatusBadRequest, "invalid machine")
		return
	case !ok:
		writeError(w, http.StatusBadRequest, "unknown command type %q (send, answer, interrupt, keys, seen, start, stop)", proto.Printable(proto.Truncate(c.Type, 32)))
		return
	case !proto.ValidCommandID(c.ID):
		writeError(w, http.StatusBadRequest, "invalid command id: use a UUID")
		return
	case c.Wait < 0 || c.Wait > maxWait:
		writeError(w, http.StatusBadRequest, "wait must be between 0 and %d seconds", maxWait)
		return
	}
	params, err := commandParams(c.Type, c.ID, c.Params)
	if err == nil {
		err = s.commandRef(c.Machine, params)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", proto.Printable(proto.Truncate(err.Error(), 300)))
		return
	}
	l := s.link(c.Machine)
	if l == nil {
		s.offline(w, c.Machine)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), forwardTimeout)
	defer cancel()
	raw, err := l.forward(ctx, method, params)
	if err != nil {
		writeRPCError(w, err)
		return
	}
	var rec proto.Receipt
	if err := proto.DecodeResult(raw, &rec); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the machine sent an invalid receipt", "uncertain": true})
		return
	}
	if c.Wait > 0 && !rec.Final {
		raw, err = l.forward(ctx, proto.MethodCommandGet, proto.CommandGetParams{ID: c.ID, Wait: c.Wait})
		var fin proto.Receipt
		if err == nil && proto.DecodeResult(raw, &fin) != nil {
			err = errors.New("invalid receipt")
		}
		if err != nil {
			// The command was taken; only its final outcome is not known
			// yet: the receipt so far says so.
			s.log.Info("command forwarded; waiting for its receipt failed", "machine", c.Machine, "type", c.Type, "id", c.ID, "err", err)
			writeJSON(w, http.StatusOK, rec)
			return
		}
		rec = fin
	}
	s.log.Info("command forwarded", "machine", c.Machine, "type", c.Type, "id", c.ID, "outcome", rec.Outcome)
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) apiCommandGet(w http.ResponseWriter, r *http.Request) {
	machine, id := r.PathValue("machine"), r.PathValue("id")
	if !validMachine(machine) || !proto.ValidCommandID(id) {
		writeError(w, http.StatusBadRequest, "invalid machine or command id")
		return
	}
	wait, err := intParam(r, "wait", 0, 0, maxWait)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	l := s.link(machine)
	if l == nil {
		s.offline(w, machine)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), forwardTimeout)
	defer cancel()
	raw, err := l.forward(ctx, proto.MethodCommandGet, proto.CommandGetParams{ID: id, Wait: int(wait)})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	var rec proto.Receipt
	if err := proto.DecodeResult(raw, &rec); err != nil {
		writeError(w, http.StatusBadGateway, "the machine sent an invalid receipt")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) apiRequests(w http.ResponseWriter, r *http.Request) {
	pos := s.streamPosition()
	f, ok := s.filter(w, r)
	if !ok {
		return
	}
	rows, err := s.st.Requests(f.Machine)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := []RequestSummary{}
	for _, row := range rows {
		ref, err := names.Parse(row.Request.Ref)
		if err != nil || (f.Realm != "" && ref.Realm != f.Realm) || (f.Project != "" && ref.Project != f.Project) {
			continue
		}
		out = append(out, RequestSummary{Machine: row.Machine, Stale: !row.Online, Request: row.Request})
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out, "stream_id": pos})
}

// DecideRequest is the body of POST /api/requests/<machine>/<id>/decide.
type DecideRequest struct {
	Approve bool   `json:"approve"`
	Reason  string `json:"reason,omitempty"`
}

func (s *Server) apiDecide(w http.ResponseWriter, r *http.Request) {
	machine, id := r.PathValue("machine"), r.PathValue("id")
	if !validMachine(machine) || !proto.ValidRequestID(id) {
		writeError(w, http.StatusBadRequest, "invalid machine or request id")
		return
	}
	var d DecideRequest
	if !decodeBody(w, r, &d) {
		return
	}
	l := s.link(machine)
	if l == nil {
		s.offline(w, machine)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), forwardTimeout)
	defer cancel()
	// No origin is sent: the machine knows the decision comes from a hub
	// by the connection, and applies that origin's rules.
	raw, err := l.forward(ctx, proto.MethodRequestsDecide, proto.DecideParams{ID: id, Approve: d.Approve, Reason: d.Reason})
	l.kick()
	if err != nil {
		// Busy (the machine's executor is full) answers 429: the request
		// stays pending and may be decided again.
		writeRPCError(w, err)
		return
	}
	var res proto.DecideResult
	if err := proto.DecodeResult(raw, &res); err != nil || cleanRequest(&res.Request) != nil {
		writeError(w, http.StatusBadGateway, "the machine sent an invalid answer")
		return
	}
	s.log.Info("request decided through the hub", "machine", machine, "request", id, "approve", d.Approve, "status", res.Request.Status)
	writeJSON(w, http.StatusOK, RequestSummary{Machine: machine, Request: res.Request})
}
