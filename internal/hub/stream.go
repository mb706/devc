package hub

// GET /api/stream: server-sent events from the change log. Every message
// carries the change's ID, so a browser that reconnects (Last-Event-ID)
// gets exactly the changes after the last one it saw. Inbox changes
// (sessions, machines, requests) go to every stream; the events of a
// session only to streams that name it (?session=<machine>/<id>, up to
// 32). A stream that starts without an ID begins at the present; one whose
// ID is older than the change log gets a reset message and starts at the
// present, and the client reads its snapshot again.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/names"
)

const (
	maxStreamSessions = 32
	streamHeartbeat   = 15 * time.Second
	streamPage        = 500
)

// streamFilter says which session events a stream wants.
type streamFilter map[[2]string]bool

func (f streamFilter) wants(c Change) bool {
	switch c.Kind {
	case ChangeEvent, ChangeSessionReset:
		return f[[2]string{c.Machine, c.Session}]
	}
	return true
}

func (s *Server) apiStream(w http.ResponseWriter, r *http.Request) {
	f := streamFilter{}
	for _, v := range r.URL.Query()["session"] {
		m, sid, ok := strings.Cut(v, "/")
		if !ok || !validMachine(m) || !names.ValidSessionID(sid) {
			writeError(w, http.StatusBadRequest, "session must be <machine>/<session>")
			return
		}
		f[[2]string{m, sid}] = true
	}
	if len(f) > maxStreamSessions {
		writeError(w, http.StatusBadRequest, "at most %d sessions per stream", maxStreamSessions)
		return
	}
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = r.URL.Query().Get("last_id")
	}
	var from int64 = -1
	if last != "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid last event ID")
			return
		}
		from = n
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	rc := http.NewResponseController(w)
	// The server has no write timeout; the deadlines set below must not
	// stay on a connection the client keeps alive.
	defer rc.SetWriteDeadline(time.Time{})
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	id := identityOf(r)
	live := s.addLive(id.Fingerprint, cancel)
	defer s.dropLive(live)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	write := func(id int64, event, data string) error {
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, event, data); err != nil {
			return err
		}
		return nil
	}
	ch := s.st.Changed()
	lo, hi, err := s.st.ChangeRange()
	if err != nil {
		return
	}
	switch {
	case from < 0:
		from = hi
		if write(hi, "ready", fmt.Sprintf(`{"id":%d}`, hi)) != nil {
			return
		}
	case from > hi || (lo > 0 && from < lo-1) || (lo == 0 && from < hi):
		// The client is ahead of this hub (another hub's ID, a new
		// database) or the change log no longer reaches back to it.
		from = hi
		if write(hi, "reset", fmt.Sprintf(`{"id":%d}`, hi)) != nil {
			return
		}
	}
	fl.Flush()
	beat := time.NewTicker(streamHeartbeat)
	defer beat.Stop()
	for {
		for {
			cs, err := s.st.ChangesAfter(from, streamPage)
			if err != nil {
				return
			}
			for _, c := range cs {
				from = c.ID
				if !f.wants(c) {
					continue
				}
				if write(c.ID, c.Kind, string(c.Data)) != nil {
					return
				}
			}
			if len(cs) < streamPage {
				break
			}
		}
		fl.Flush()
		select {
		case <-ctx.Done():
			return
		case <-ch:
			ch = s.st.Changed()
		case <-beat.C:
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
