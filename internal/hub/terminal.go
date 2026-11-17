// This file implements GET /api/terminal/<machine>/<session> (work order
// C12 section 3): a WebSocket between the web app and the machine's
// terminal.open stream of a session's tmux window. The hub picks the
// stream ID, so the machine's terminal.data notifications always find
// their sink, and relays output as binary frames. Each machine link
// carries the terminals of every browser, within the daemon's limit per
// connection. The browser sends JSON text frames: {"type":"input",
// "data":…} (control mode only) and {"type":"visible","visible":bool}. The
// hub sends {"type":"open",cols,rows,mode}, {"type":"size",cols,rows},
// {"type":"ping"} (keeps proxies from closing a quiet stream) and
// {"type":"end",reason} before it closes. Once the machine's answer to
// terminal.open is accepted, the hub confirms the stream
// (terminal.confirm); whatever ends the relay, a failed or unanswered open
// included, it sends terminal.close (finding G5-06).
//
// Control mode is refused here unless hub.toml says [terminal] control =
// true; the machine checks its own switch. A stream ends when the session
// ends, when either side disconnects or the machine goes offline, when the
// browser falls termSinkQueue messages behind, after 30 minutes without
// the page being visible, when the login session ends, and when the device
// certificate is revoked or expires.

package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// Terminal relay limits (variables for tests). termOpenWait bounds the
// machine's answer to terminal.open.
var (
	termHidden    = 30 * time.Minute
	termPing      = 25 * time.Second
	termSinkQueue = 512
	termOpenWait  = 30 * time.Second
)

// termMsg is one message from the machine for a stream.
type termMsg struct {
	data []byte
	cols int
	rows int
}

// termSink receives a stream's notifications on the link's reader, which
// must never wait: a browser that cannot keep up loses its stream.
type termSink struct {
	ch   chan termMsg
	done chan struct{}
	once sync.Once
	why  string
}

func (k *termSink) fail(why string) {
	k.once.Do(func() {
		k.why = why
		close(k.done)
	})
}

func (k *termSink) put(m termMsg) {
	select {
	case <-k.done:
	case k.ch <- m:
	default:
		k.fail("the browser could not keep up with the terminal")
	}
}

func (l *machineLink) addTerm(stream string) *termSink {
	k := &termSink{ch: make(chan termMsg, termSinkQueue), done: make(chan struct{})}
	l.termMu.Lock()
	l.terms[stream] = k
	l.termMu.Unlock()
	return k
}

func (l *machineLink) dropTerm(stream string) {
	l.termMu.Lock()
	delete(l.terms, stream)
	l.termMu.Unlock()
}

func (l *machineLink) term(stream string) *termSink {
	l.termMu.Lock()
	defer l.termMu.Unlock()
	return l.terms[stream]
}

func (l *machineLink) onTermData(_ context.Context, _ *proto.Conn, params json.RawMessage) {
	var d proto.TerminalData
	if proto.DecodeParams(params, &d) != nil || proto.CheckTerminalData(d) != nil {
		return
	}
	if k := l.term(d.Stream); k != nil {
		k.put(termMsg{data: d.Data})
	}
}

func (l *machineLink) onTermSize(_ context.Context, _ *proto.Conn, params json.RawMessage) {
	var d proto.TerminalSize
	if proto.DecodeParams(params, &d) != nil || !proto.ValidTermSize(d.Cols, d.Rows) {
		return
	}
	if k := l.term(d.Stream); k != nil {
		k.put(termMsg{cols: d.Cols, rows: d.Rows})
	}
}

func (l *machineLink) onTermEnd(_ context.Context, _ *proto.Conn, params json.RawMessage) {
	var d proto.TerminalEnd
	if proto.DecodeParams(params, &d) != nil {
		return
	}
	if k := l.term(d.Stream); k != nil {
		k.fail(proto.Printable(proto.Truncate(d.Reason, 200)))
	}
}

// termClient is a browser's message.
type termClient struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Visible *bool  `json:"visible,omitempty"`
}

func newStreamID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) apiTerminal(w http.ResponseWriter, r *http.Request) {
	machine, sid := r.PathValue("machine"), r.PathValue("session")
	if !validMachine(machine) || !names.ValidSessionID(sid) {
		writeError(w, http.StatusBadRequest, "invalid machine or session")
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = proto.TerminalObserve
	}
	if mode != proto.TerminalObserve && mode != proto.TerminalControl {
		writeError(w, http.StatusBadRequest, "mode must be %s or %s", proto.TerminalObserve, proto.TerminalControl)
		return
	}
	if mode == proto.TerminalControl && !s.cfg.Terminal.Control {
		writeError(w, http.StatusForbidden, "this hub does not allow terminal control ([terminal] control in hub.toml)")
		return
	}
	l := s.link(machine)
	if l == nil {
		s.offline(w, machine)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		s.log.Debug("terminal: websocket upgrade failed", "err", err)
		return
	}
	ws.SetReadLimit(16 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	id, login := identityOf(r), loginOf(r)
	live := s.addLive(id, cancel)
	defer s.dropLive(live)

	var wmu sync.Mutex
	send := func(typ websocket.MessageType, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
		defer wcancel()
		return ws.Write(wctx, typ, b)
	}
	sendJSON := func(v any) error {
		b, _ := json.Marshal(v)
		return send(websocket.MessageText, b)
	}
	finish := func(reason string) {
		_ = sendJSON(map[string]string{"type": "end", "reason": reason})
		_ = ws.Close(websocket.StatusNormalClosure, "")
		s.log.Info("terminal closed", "machine", machine, "session", sid, "reason", reason)
	}

	stream := newStreamID()
	sink := l.addTerm(stream)
	defer l.dropTerm(stream)
	// The machine's stream is closed whatever ends this one, a failed or
	// unanswered open included (a no-op when the machine never opened it or
	// ended it already). An open whose answer is lost the machine ends
	// itself, as it is never confirmed (finding G5-06).
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = l.forward(cctx, proto.MethodTerminalClose, proto.TerminalStreamParams{Stream: stream})
	}()
	octx, ocancel := context.WithTimeout(ctx, termOpenWait)
	raw, err := l.forward(octx, proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: stream, Session: sid, Ref: s.sessionRef(machine, sid), Mode: mode})
	ocancel()
	var res proto.TerminalOpenResult
	if err == nil {
		if err = proto.DecodeResult(raw, &res); err == nil && (res.Stream != stream || !proto.ValidTermSize(res.Cols, res.Rows)) {
			err = proto.Errorf(proto.CodeInternal, "invalid answer")
		}
	}
	if err == nil {
		cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = l.forward(cctx, proto.MethodTerminalConfirm, proto.TerminalStreamParams{Stream: stream})
		ccancel()
		// A machine older than terminal.confirm does not know the method
		// and does not wait for it (finding H3-01): its terminals go on as
		// before.
		if proto.IsCode(err, proto.CodeMethodNotFound) {
			err = nil
		}
	}
	if err != nil {
		_, msg, _ := rpcStatus(err)
		finish(msg)
		return
	}
	s.log.Info("terminal opened", "machine", machine, "session", sid, "mode", mode, "cert", id.Name)
	if sendJSON(map[string]any{"type": "open", "cols": res.Cols, "rows": res.Rows, "mode": mode}) != nil {
		return
	}

	// Reader: input and visibility from the browser. Until the page says
	// it is visible, the stream counts as hidden: a page hidden while the
	// socket was connecting still meets the deadline (finding C2-17).
	var hmu sync.Mutex
	hiddenSince := time.Now()
	readErr := make(chan string, 1)
	go func() {
		for {
			typ, b, err := ws.Read(ctx)
			if err != nil {
				readErr <- "the page closed the terminal"
				return
			}
			var m termClient
			if typ != websocket.MessageText || json.Unmarshal(b, &m) != nil {
				readErr <- "the page sent something unexpected"
				return
			}
			switch m.Type {
			case "visible":
				hmu.Lock()
				if m.Visible != nil && !*m.Visible {
					if hiddenSince.IsZero() {
						hiddenSince = time.Now()
					}
				} else {
					hiddenSince = time.Time{}
				}
				hmu.Unlock()
			case "input":
				if mode != proto.TerminalControl {
					readErr <- "this terminal only observes"
					return
				}
				if len(m.Data) == 0 || len(m.Data) > proto.MaxTerminalInput {
					continue
				}
				ictx, icancel := context.WithTimeout(ctx, 10*time.Second)
				_, err := l.forward(ictx, proto.MethodTerminalInput, proto.TerminalInputParams{Stream: stream, Data: m.Data})
				icancel()
				if err != nil {
					_, msg, _ := rpcStatus(err)
					readErr <- "input failed: " + msg
					return
				}
			}
		}
	}()

	ping := time.NewTicker(termPing)
	defer ping.Stop()
	beat := time.NewTicker(streamHeartbeat)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			finish("the hub closed the terminal")
			return
		case <-l.ctx.Done():
			finish("the machine went offline")
			return
		case <-sink.done:
			// Output queued before the end still goes out.
		drain:
			for {
				select {
				case m := <-sink.ch:
					if m.data != nil && send(websocket.MessageBinary, m.data) != nil {
						return
					}
				default:
					break drain
				}
			}
			finish(sink.why)
			return
		case why := <-readErr:
			finish(why)
			return
		case m := <-sink.ch:
			var err error
			if m.data != nil {
				err = send(websocket.MessageBinary, m.data)
			} else {
				err = sendJSON(map[string]any{"type": "size", "cols": m.cols, "rows": m.rows})
			}
			if err != nil {
				return
			}
		case <-ping.C:
			hmu.Lock()
			hidden := !hiddenSince.IsZero() && time.Since(hiddenSince) >= termHidden
			hmu.Unlock()
			if hidden {
				finish("the page was not visible for 30 minutes")
				return
			}
			if sendJSON(map[string]string{"type": "ping"}) != nil {
				return
			}
		case <-beat.C:
			if lg, ok, err := s.st.LoginByID(login.ID); err == nil && (!ok || !s.now().Before(lg.Expires())) {
				finish("the login session ended")
				return
			}
		}
	}
}
