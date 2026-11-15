package hub

// Notifications through ntfy (work order C11b section 3). The notifier
// follows the change log on its own goroutine, so a slow or unreachable
// ntfy server never holds up the machine links. When a session enters
// needs-you, failed or finished, and its machine named this hub as
// notifier, and no logged-in client follows the session's events, one
// message is published: title "<project> · <harness>", the question or the
// last message as body, a priority by section, a click URL into the web
// app, and up to three buttons (one per option of a question with one or
// two options, and Open). An option button posts to /api/act with a token
// in the X-Devc-Action header (never in the URL, which proxies log:
// finding C2-06): random, single-use, valid for an hour, bound to that one
// answer. When the session leaves the state, the message is cleared by its
// sequence ID and its buttons stop working.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mb706/devc/internal/proto"
)

// actPath is the path of notification buttons; ActionHeader carries the
// button's token.
const (
	actPath      = "/api/act"
	ActionHeader = "X-Devc-Action"
)

// Limits of notifications.
const (
	// messageCut is how much of the last message a notification shows.
	messageCut = 300
	// maxNoteBytes keeps a message below ntfy's 4,096 bytes.
	maxNoteBytes = 3500
	maxLabel     = 40
	ntfyTimeout  = 10 * time.Second
	ntfyAttempts = 3
	// actWait is how long a button waits for the machine's final receipt.
	actWait = 20
)

// ntfy priorities.
const (
	priorityDefault = 3
	priorityHigh    = 4
)

// Notification is a message as published to ntfy (JSON publishing).
type Notification struct {
	Topic      string       `json:"topic"`
	Title      string       `json:"title"`
	Message    string       `json:"message"`
	Priority   int          `json:"priority"`
	Tags       []string     `json:"tags"`
	Click      string       `json:"click"`
	Actions    []NtfyAction `json:"actions"`
	SequenceID string       `json:"sequence_id"`
}

// NtfyAction is an action button.
type NtfyAction struct {
	Action  string            `json:"action"`
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Clear   bool              `json:"clear,omitempty"`
}

// noteSections are the sections that notify.
var noteSections = map[string]bool{SectionNeedsYou: true, SectionFailed: true, SectionFinished: true}

// notifyKey names the state of a session that a notification is about ("":
// none). A new key means a new notification; leaving a key clears it.
func notifyKey(sum *SessionSummary) string {
	if sum == nil || !noteSections[sum.Section] {
		return ""
	}
	switch sum.Section {
	case SectionNeedsYou:
		if q := sum.Question; q != nil {
			return SectionNeedsYou + "/q/" + q.ID
		}
		return SectionNeedsYou
	case SectionFinished:
		return SectionFinished + "/" + strconv.FormatInt(sum.DoneTurn, 10) + "/" + strconv.FormatInt(sum.Turn, 10)
	}
	return sum.Section
}

// cutRunes shortens s to n characters, marking the cut.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i, k := 0, 0
	for i = range s {
		if k == n-1 {
			break
		}
		k++
	}
	return s[:i] + "…"
}

// cutBytes shortens s to at most n bytes on a character boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n - len("…")
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}

// actionOptions returns the options a notification offers as buttons: a
// question's options when it has one or two.
func actionOptions(sum *SessionSummary) []string {
	if sum.Section != SectionNeedsYou || sum.Question == nil || len(sum.Question.Options) == 0 || len(sum.Question.Options) > 2 {
		return nil
	}
	return sum.Question.Options
}

// SessionURL is a session's address in the web app.
func SessionURL(publicURL, machine, session string) string {
	return publicURL + "/#/s/" + machine + "/" + session
}

// buildNotification makes the message for a session in a notifying
// section; tokens are the action tokens of actionOptions(sum), in order.
func buildNotification(publicURL, topic, seq string, sum *SessionSummary, tokens []string) Notification {
	harness := sum.Harness
	if harness == "" {
		harness = "session"
	}
	n := Notification{
		Topic:      topic,
		Title:      proto.Printable(sum.Project + " · " + harness),
		Priority:   priorityHigh,
		Click:      SessionURL(publicURL, sum.Machine, sum.Session),
		SequenceID: seq,
	}
	last := cutRunes(proto.PrintableText(strings.TrimSpace(sum.Message)), messageCut)
	switch sum.Section {
	case SectionNeedsYou:
		n.Tags = []string{"question", SectionNeedsYou}
		switch {
		case sum.Question != nil:
			n.Message = proto.PrintableText(sum.Question.Text)
		case sum.Requests > 0:
			n.Message = "A request waits for a decision."
		default:
			n.Message = "The session waits for you."
		}
	case SectionFailed:
		n.Tags = []string{"x", SectionFailed}
		n.Message = last
		if n.Message == "" {
			n.Message = "The session failed."
		}
	case SectionFinished:
		n.Tags = []string{"white_check_mark", SectionFinished}
		n.Priority = priorityDefault
		n.Message = last
		if n.Message == "" {
			n.Message = "The session finished its turn."
		}
	}
	n.Message = cutBytes(n.Message, maxNoteBytes)
	for i, opt := range actionOptions(sum) {
		if i >= len(tokens) {
			break
		}
		n.Actions = append(n.Actions, NtfyAction{
			Action:  "http",
			Label:   cutRunes(proto.Printable(opt), maxLabel),
			URL:     publicURL + actPath,
			Method:  http.MethodPost,
			Headers: map[string]string{RequestHeader: "1", ActionHeader: tokens[i]},
			Clear:   true,
		})
	}
	n.Actions = append(n.Actions, NtfyAction{Action: "view", Label: "Open", URL: n.Click})
	return n
}

// newSeq is a sequence ID for a notification.
func newSeq() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "devc" + hex.EncodeToString(b)
}

// newUUID is a random (version 4) UUID, the form command IDs take.
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// --- ntfy client ---

// ntfyBackoff is the wait before the second attempt (doubled before the
// third); a variable for tests.
var ntfyBackoff = time.Second

// ntfyClient publishes and clears messages, with a timeout per call and a
// few attempts.
type ntfyClient struct {
	cfg  NtfyConfig
	hc   *http.Client
	wait func(attempt int) time.Duration
}

func newNtfyClient(cfg NtfyConfig) *ntfyClient {
	backoff := ntfyBackoff
	return &ntfyClient{cfg: cfg, hc: &http.Client{Timeout: ntfyTimeout}, wait: func(a int) time.Duration { return time.Duration(a) * backoff }}
}

// do sends one request, retried on network errors, 408, 429 and 5xx.
func (c *ntfyClient) do(ctx context.Context, method, url string, body []byte) error {
	var last error
	for attempt := 1; attempt <= ntfyAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.wait(attempt - 1)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			// The error may name the URL, never the token.
			last = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		switch {
		case resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			last = fmt.Errorf("ntfy answered %s", resp.Status)
		default:
			return fmt.Errorf("ntfy answered %s", resp.Status)
		}
	}
	return last
}

func (c *ntfyClient) publish(ctx context.Context, n Notification) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, c.cfg.URL+"/", b)
}

func (c *ntfyClient) clear(ctx context.Context, seq string) error {
	return c.do(ctx, http.MethodPut, c.cfg.URL+"/"+c.cfg.Topic+"/"+seq+"/clear", nil)
}

// NtfyHealth asks an ntfy server whether it is healthy (devc hub doctor).
func NtfyHealth(ctx context.Context, cfg NtfyConfig) error {
	ctx, cancel := context.WithTimeout(ctx, ntfyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL+"/v1/health", nil)
	if err != nil {
		return err
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := (&http.Client{Timeout: ntfyTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var h struct {
		Healthy bool `json:"healthy"`
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&h); err != nil || !h.Healthy {
		return errors.New("does not say it is healthy")
	}
	return nil
}

// --- The notifier ---

// Retries of notifications (variables for tests): due records are looked
// at every notifyRetryTick; a failed publish or clear waits notifyRetryBase,
// doubling up to notifyRetryMax; a clear is given up after clearMaxAge.
var (
	notifyRetryTick = 15 * time.Second
	notifyRetryBase = 30 * time.Second
	notifyRetryMax  = 15 * time.Minute
	clearMaxAge     = 7 * 24 * time.Hour
)

// metaNotifyPos is the change ID the notifier has looked at.
const metaNotifyPos = "notify_pos"

// notifier keeps the notification state of every session explicit and
// durable (findings C2-13, C2-14): each attention state has a record,
// pending until published (or handled without a message), sent, then
// cleared once the state ends. A hub that is not the machine's notifier
// records pending and sends what is still pending and still current when
// it becomes the notifier; a state that ends before it was sent is
// dropped. Failed publishes and clears are retried with backoff, also
// after a restart, and the change log position is kept, so a restart
// misses nothing.
type notifier struct {
	s *Server
	c *ntfyClient
	// rows are the records by session and key; last is each session's
	// current key (for "not news").
	rows map[SessionKey]map[string]*NoteState
	last map[SessionKey]string
}

func newNotifier(s *Server, cfg *NtfyConfig) *notifier {
	n := &notifier{s: s, rows: map[SessionKey]map[string]*NoteState{}, last: map[SessionKey]string{}}
	if cfg != nil {
		n.c = newNtfyClient(*cfg)
	}
	return n
}

func noteKey(r *NoteState) SessionKey { return SessionKey{Machine: r.Machine, Session: r.Session} }

// put stores a record (and keeps it in memory).
func (n *notifier) put(r *NoteState) {
	k := noteKey(r)
	if n.rows[k] == nil {
		n.rows[k] = map[string]*NoteState{}
	}
	n.rows[k][r.Key] = r
	if err := n.s.st.PutNoteState(*r); err != nil {
		n.s.log.Warn("notifications: recording", "err", err)
	}
}

// drop removes a record.
func (n *notifier) drop(r *NoteState) {
	k := noteKey(r)
	delete(n.rows[k], r.Key)
	if len(n.rows[k]) == 0 {
		delete(n.rows, k)
	}
	if err := n.s.st.DeleteNoteState(r.Machine, r.Session, r.Key); err != nil {
		n.s.log.Warn("notifications: recording", "err", err)
	}
}

// backoff is the wait before attempt tries+1.
func backoff(tries int) time.Duration {
	d := notifyRetryBase
	for i := 1; i < tries && d < notifyRetryMax; i++ {
		d *= 2
	}
	return min(d, notifyRetryMax)
}

// current returns the summaries of every session.
func (n *notifier) current() (map[SessionKey]*SessionSummary, error) {
	list, err := n.s.summaries(SessionFilter{})
	if err != nil {
		return nil, err
	}
	cur := map[SessionKey]*SessionSummary{}
	for i := range list {
		cur[SessionKey{Machine: list[i].Machine, Session: list[i].Session}] = &list[i]
	}
	return cur, nil
}

// start loads the records and decides where to follow the change log
// from: where it stopped, if the log still reaches back there; else from
// the present after comparing the projection with the records. On the very
// first start the states the hub holds are not notified.
func (n *notifier) start(ctx context.Context) (int64, error) {
	lo, hi, err := n.s.st.ChangeRange()
	if err != nil {
		return 0, err
	}
	rows, err := n.s.st.NoteStates()
	if err != nil {
		return 0, err
	}
	for i := range rows {
		r := rows[i]
		k := noteKey(&r)
		if n.rows[k] == nil {
			n.rows[k] = map[string]*NoteState{}
		}
		n.rows[k][r.Key] = &r
	}
	cur, err := n.current()
	if err != nil {
		return 0, err
	}
	for k, sum := range cur {
		n.last[k] = notifyKey(sum)
	}
	v, err := n.s.st.Meta(metaNotifyPos)
	if err != nil {
		return 0, err
	}
	pos, perr := strconv.ParseInt(v, 10, 64)
	switch {
	case v == "" || perr != nil:
		now := n.s.now()
		for k, sum := range cur {
			if key := notifyKey(sum); key != "" && n.rows[k][key] == nil {
				n.put(&NoteState{Machine: k.Machine, Session: k.Session, Key: key, State: NoteSent, Created: now})
			}
		}
		n.resync(ctx, cur)
		return hi, nil
	case pos <= hi && (pos >= lo-1 || lo == 0 && pos == hi):
		return pos, nil
	}
	n.s.log.Warn("notifications: the change log moved on while the hub was down; resynchronising")
	n.resync(ctx, cur)
	return hi, nil
}

// run follows the change log until ctx ends.
func (n *notifier) run(ctx context.Context) {
	if n.c == nil {
		return
	}
	from, err := n.start(ctx)
	if err != nil {
		n.s.log.Error("notifications: reading the projection", "err", err)
		return
	}
	n.savePos(from)
	n.s.log.Info("notifications go to ntfy", "server", n.c.cfg.URL, "topic", n.c.cfg.Topic)
	n.retryDue(ctx)
	tick := time.NewTicker(notifyRetryTick)
	defer tick.Stop()
	for {
		ch := n.s.st.Changed()
		cs, gap, err := n.s.st.changesAfter(from, streamPage)
		if err != nil {
			n.s.log.Error("notifications: reading the change log", "err", err)
			cs = nil
		}
		if gap {
			// The change log moved past what was looked at: start again
			// from the projection.
			n.s.log.Warn("notifications fell behind the change log; resynchronising")
			_, hi, err := n.s.st.ChangeRange()
			if err != nil {
				return
			}
			from = hi
			if cur, err := n.current(); err == nil {
				n.resync(ctx, cur)
			}
			n.savePos(from)
			continue
		}
		if len(cs) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-ch:
			case <-tick.C:
				n.retryDue(ctx)
			}
			continue
		}
		// The latest summary of each session in the batch (nil: removed),
		// and the machines whose connection or role changed.
		latest := map[SessionKey]*SessionSummary{}
		var order []SessionKey
		machines := false
		for _, c := range cs {
			from = c.ID
			var k SessionKey
			var sum *SessionSummary
			switch c.Kind {
			case ChangeSession:
				var v SessionSummary
				if json.Unmarshal(c.Data, &v) != nil {
					continue
				}
				k, sum = SessionKey{Machine: c.Machine, Session: c.Session}, &v
			case ChangeSessionRemoved:
				k = SessionKey{Machine: c.Machine, Session: c.Session}
			case ChangeMachine:
				machines = true
				continue
			default:
				continue
			}
			if _, seen := latest[k]; !seen {
				order = append(order, k)
			}
			latest[k] = sum
		}
		for _, k := range order {
			if ctx.Err() != nil {
				return
			}
			n.update(ctx, k, latest[k])
		}
		if machines {
			// A hub that just became a notifier sends what is still
			// pending.
			n.retryDue(ctx)
		}
		n.savePos(from)
	}
}

func (n *notifier) savePos(pos int64) {
	if err := n.s.st.SetMeta(metaNotifyPos, strconv.FormatInt(pos, 10)); err != nil {
		n.s.log.Warn("notifications: recording the position", "err", err)
	}
}

// resync compares the records with the current projection.
func (n *notifier) resync(ctx context.Context, cur map[SessionKey]*SessionSummary) {
	for k := range n.rows {
		if _, ok := cur[k]; !ok {
			n.update(ctx, k, nil)
		}
	}
	for k, sum := range cur {
		n.update(ctx, k, sum)
	}
}

// machineNotifies reports whether a machine has named this hub its
// notifier (and is connected).
func (n *notifier) machineNotifies(name string) bool {
	m, ok, err := n.s.st.Machine(name)
	return err == nil && ok && m.Online && m.Notifier
}

// update applies a session's current state (nil: the session is gone):
// records of states that ended are dropped (pending) or cleared (sent),
// and a new state gets a pending record that is published at once when
// it can be.
func (n *notifier) update(ctx context.Context, k SessionKey, sum *SessionSummary) {
	want := notifyKey(sum)
	prev := n.last[k]
	if sum == nil {
		delete(n.last, k)
	} else {
		n.last[k] = want
	}
	now := n.s.now()
	for key, r := range n.rows[k] {
		if key == want {
			continue
		}
		switch r.State {
		case NotePending:
			// Ended before it was sent: dropped.
			n.drop(r)
		case NoteSent:
			if r.Seq == "" {
				n.drop(r)
				continue
			}
			if err := n.s.st.DeleteActions(r.Seq); err != nil {
				n.s.log.Warn("notifications: removing buttons", "err", err)
			}
			r.State, r.Tries, r.NextAt = NoteCleared, 0, now
			n.put(r)
			n.clear(ctx, r)
		}
	}
	if want == "" {
		return
	}
	if r := n.rows[k][want]; r != nil {
		if r.State == NoteCleared {
			// The state came back before its old message was cleared: the
			// message (same sequence ID) is published again instead.
			r.State, r.Tries, r.NextAt = NotePending, 0, now
			n.put(r)
			n.publish(ctx, r, sum)
		}
		return
	}
	r := &NoteState{Machine: k.Machine, Session: k.Session, Key: want, State: NotePending, Seq: newSeq(), NextAt: now, Created: now}
	if want == SectionNeedsYou && strings.HasPrefix(prev, SectionNeedsYou+"/q/") {
		// The question was answered and the session has not reported its
		// next state yet: not news.
		r.State, r.Seq = NoteSent, ""
	}
	n.put(r)
	if r.State == NotePending {
		n.publish(ctx, r, sum)
	}
}

// retryDue retries the publishes and clears that are due: a pending
// record whose state is still current (when this hub is the machine's
// notifier), a cleared record whose clear has not been confirmed.
func (n *notifier) retryDue(ctx context.Context) {
	now := n.s.now()
	var due []*NoteState
	for _, m := range n.rows {
		for _, r := range m {
			if (r.State == NotePending || r.State == NoteCleared) && !r.NextAt.After(now) {
				due = append(due, r)
			}
		}
	}
	notifies := map[string]bool{}
	for _, r := range due {
		if ctx.Err() != nil {
			return
		}
		if r.State == NoteCleared {
			n.clear(ctx, r)
			continue
		}
		on, ok := notifies[r.Machine]
		if !ok {
			on = n.machineNotifies(r.Machine)
			notifies[r.Machine] = on
		}
		if !on {
			continue
		}
		list, err := n.s.summaries(SessionFilter{Machine: r.Machine, Session: r.Session})
		if err != nil {
			continue
		}
		if len(list) != 1 || notifyKey(&list[0]) != r.Key {
			n.drop(r)
			continue
		}
		n.publish(ctx, r, &list[0])
	}
}

// publish sends a pending record's notification with its buttons, when the
// machine named this hub its notifier; a session a client follows is
// handled without a message. A failure is retried later.
func (n *notifier) publish(ctx context.Context, r *NoteState, sum *SessionSummary) {
	k := noteKey(r)
	if !n.machineNotifies(r.Machine) {
		n.s.log.Debug("notification pending: the machine has not named this hub its notifier", "machine", k.Machine, "session", k.Session, "section", sum.Section)
		return
	}
	if n.s.watched(k) {
		n.s.log.Debug("no notification: a client follows the session", "machine", k.Machine, "session", k.Session, "section", sum.Section)
		r.State, r.Seq = NoteSent, ""
		n.put(r)
		return
	}
	now := n.s.now()
	fail := func(what string, err error) {
		r.Tries++
		r.NextAt = now.Add(backoff(r.Tries))
		n.put(r)
		n.s.log.Warn("notifications: "+what, "machine", k.Machine, "session", k.Session, "tries", r.Tries, "err", err)
	}
	opts := actionOptions(sum)
	var tokens []string
	var actions []Action
	for i, opt := range opts {
		tok, id, err := NewToken()
		if err != nil {
			fail("making buttons failed", err)
			return
		}
		tokens = append(tokens, tok)
		actions = append(actions, Action{ID: id, Machine: k.Machine, Session: k.Session, Question: sum.Question.ID, Choice: i + 1,
			Label: proto.Truncate(opt, 200), Command: newUUID(), Seq: r.Seq, Expires: now.Add(ActionTTL)})
	}
	// Buttons of an earlier attempt of this message go.
	_ = n.s.st.DeleteActions(r.Seq)
	if len(actions) > 0 {
		if err := n.s.st.AddActions(actions); err != nil {
			fail("storing buttons failed", err)
			return
		}
	}
	note := buildNotification(n.s.publicURL, n.c.cfg.Topic, r.Seq, sum, tokens)
	if err := n.c.publish(ctx, note); err != nil {
		_ = n.s.st.DeleteActions(r.Seq)
		fail("publishing failed; it is retried", err)
		return
	}
	r.State, r.Tries = NoteSent, 0
	n.put(r)
	n.s.log.Info("notification published", "machine", k.Machine, "session", k.Session, "section", sum.Section, "buttons", len(tokens))
}

// clear clears a cleared record's message at ntfy; the record goes when
// ntfy confirms (or after clearMaxAge), else the clear is retried later.
func (n *notifier) clear(ctx context.Context, r *NoteState) {
	k := noteKey(r)
	if err := n.c.clear(ctx, r.Seq); err != nil {
		now := n.s.now()
		if now.Sub(r.Created) > clearMaxAge {
			n.s.log.Warn("notifications: giving up clearing a message", "machine", k.Machine, "session", k.Session, "err", err)
			n.drop(r)
			return
		}
		r.Tries++
		r.NextAt = now.Add(backoff(r.Tries))
		n.put(r)
		n.s.log.Warn("notifications: clearing a message failed; it is retried", "machine", k.Machine, "session", k.Session, "tries", r.Tries, "err", err)
		return
	}
	n.s.log.Info("notification cleared", "machine", k.Machine, "session", k.Session)
	n.drop(r)
}

// --- Buttons ---

// apiAct performs the one command a notification button is bound to. It
// needs a device certificate but no login session: the single-use token is
// the credential. The answer is a short text for the ntfy app.
func (s *Server) apiAct(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	now := s.now()
	text := func(code int, format string, args ...any) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		fmt.Fprintf(w, format+"\n", args...)
	}
	if s.limited(w, "c:"+id.Fingerprint) {
		return
	}
	invalid := func() {
		s.lim.fail("c:"+id.Fingerprint, now)
		s.log.Info("notification button refused", "cert", id.Name)
		text(http.StatusNotFound, "This button is no longer valid.")
	}
	vals := r.Header.Values(ActionHeader)
	if len(vals) != 1 || !ValidToken(vals[0]) {
		invalid()
		return
	}
	tok := vals[0]
	tid := TokenID(tok)
	a, ok, err := s.st.PeekAction(tid, now)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !ok {
		invalid()
		return
	}
	// A machine that is not connected leaves the button usable for later.
	l := s.link(a.Machine)
	if l == nil {
		text(http.StatusServiceUnavailable, "Machine %s is offline; try again later.", a.Machine)
		return
	}
	a, err = s.st.UseAction(tid, now)
	if errors.Is(err, errTokenUnusable) {
		invalid()
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), forwardTimeout)
	defer cancel()
	raw, err := l.forward(ctx, proto.MethodSessionAnswer, proto.AnswerParams{ID: a.Command, Session: a.Session, Ref: s.sessionRef(a.Machine, a.Session), Question: a.Question, Choice: a.Choice})
	var rec proto.Receipt
	if err == nil && proto.DecodeResult(raw, &rec) != nil {
		err = errors.New("invalid receipt")
	}
	if err == nil && !rec.Final {
		raw, err = l.forward(ctx, proto.MethodCommandGet, proto.CommandGetParams{ID: a.Command, Wait: actWait})
		if err == nil && proto.DecodeResult(raw, &rec) != nil {
			err = errors.New("invalid receipt")
		}
	}
	label := proto.Printable(cutRunes(a.Label, maxLabel))
	if err != nil {
		code, msg, uncertain := rpcStatus(err)
		s.log.Info("notification button", "cert", id.Name, "machine", a.Machine, "session", a.Session, "err", msg)
		if uncertain {
			text(code, "Maybe not answered: %s", msg)
			return
		}
		text(code, "Not answered: %s", msg)
		return
	}
	s.log.Info("notification button", "cert", id.Name, "machine", a.Machine, "session", a.Session, "choice", a.Choice, "outcome", rec.Outcome)
	switch rec.Outcome {
	case proto.OutcomeDelivered:
		text(http.StatusOK, "Answered: %s", label)
	case proto.OutcomeRefused:
		text(http.StatusConflict, "Not answered: %s", proto.Printable(proto.Truncate(rec.Detail, 200)))
	default:
		text(http.StatusAccepted, "Sent: %s (%s)", label, rec.Outcome)
	}
}
