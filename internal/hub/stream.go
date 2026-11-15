package hub

// GET /api/stream: server-sent events from the change log. Every message
// carries the change's ID, so a browser that reconnects (Last-Event-ID)
// gets exactly the changes after the last one it saw. Inbox changes
// (sessions, machines, requests) go to every stream; the events of a
// session only to streams that name it (?session=<machine>/<id>, up to
// 32). A stream that starts without an ID begins at the present; one whose
// ID is older than the change log gets a reset message and starts at the
// present, and the client reads its snapshot again. While a stream names
// a session, the hub sends no notifications for it.

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
	streamPage        = 500
)

// streamHeartbeat is how often a stream sends a comment and checks its
// login session (a variable for tests).
var streamHeartbeat = 15 * time.Second

// streamPageHook, in tests, runs before a stream reads a page of changes.
var streamPageHook func()

// changesAfter reads the next page of the change log after from. The IDs
// of the log are consecutive (AUTOINCREMENT, rows deleted only by pruning
// the oldest), and a page is one statement, so one consistent snapshot: a
// first ID other than from+1 means pruning overtook the reader, and gap is
// true (finding C2-09).
func (s *Store) changesAfter(from int64, limit int) (cs []Change, gap bool, err error) {
	cs, err = s.ChangesAfter(from, limit)
	if err != nil {
		return nil, false, err
	}
	return cs, len(cs) > 0 && cs[0].ID != from+1, nil
}

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
	login := loginOf(r)
	live := s.addLive(id, cancel)
	defer s.dropLive(live)
	// A session a logged-in client follows gets no notifications.
	keys := make([]SessionKey, 0, len(f))
	for k := range f {
		keys = append(keys, SessionKey{Machine: k[0], Session: k[1]})
	}
	s.addWatch(keys)
	defer s.dropWatch(keys)

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
			if streamPageHook != nil {
				streamPageHook()
			}
			cs, gap, err := s.st.changesAfter(from, streamPage)
			if err != nil {
				return
			}
			if gap {
				// The log no longer reaches back to this stream's position:
				// changes were lost to it, so the client reads its snapshot
				// again.
				_, hi, err := s.st.ChangeRange()
				if err != nil {
					return
				}
				from = hi
				if write(hi, "reset", fmt.Sprintf(`{"id":%d}`, hi)) != nil {
					return
				}
				continue
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
			// A stream ends with its login session (logout, revocation,
			// expiry), at the next heartbeat.
			if l, ok, err := s.st.LoginByID(login.ID); err == nil && (!ok || !s.now().Before(l.Expires())) {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
