package hub

// Notifications through ntfy (work order C11b section 3). The notifier
// follows the change log on its own goroutine, so a slow or unreachable
// ntfy server never holds up the machine links. When a session enters
// needs-you, failed or finished, and its machine named this hub as
// notifier, and no logged-in client follows the session's events, one
// message is published: title "<project> · <harness>", the question or the
// last message as body, a priority by section, a click URL into the web
// app, and up to three buttons (one per option of a question with one or
// two options, and Open). An option button posts to /api/act/<token>: a
// random token, single-use, valid for an hour, bound to that one answer.
// When the session leaves the state, the message is cleared by its
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

// actPrefix is the path of notification buttons.
const actPrefix = "/api/act/"

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
			URL:     publicURL + actPrefix + tokens[i],
			Method:  http.MethodPost,
			Headers: map[string]string{RequestHeader: "1"},
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

type noteState struct{ key, seq string }

type notifier struct {
	s     *Server
	c     *ntfyClient
	state map[SessionKey]noteState
}

func newNotifier(s *Server, cfg *NtfyConfig) *notifier {
	n := &notifier{s: s, state: map[SessionKey]noteState{}}
	if cfg != nil {
		n.c = newNtfyClient(*cfg)
	}
	return n
}

// load sets the state from the projection (states the hub already holds
// at start are not notified) and the notifications recorded as published.
func (n *notifier) load(ctx context.Context) error {
	list, err := n.s.summaries(SessionFilter{})
	if err != nil {
		return err
	}
	cur := map[SessionKey]*SessionSummary{}
	n.state = map[SessionKey]noteState{}
	for i := range list {
		k := SessionKey{Machine: list[i].Machine, Session: list[i].Session}
		cur[k] = &list[i]
		if key := notifyKey(&list[i]); key != "" {
			n.state[k] = noteState{key: key}
		}
	}
	rows, err := n.s.st.Notifications()
	if err != nil {
		return err
	}
	for _, r := range rows {
		k := SessionKey{Machine: r.Machine, Session: r.Session}
		n.state[k] = noteState{key: r.Key, seq: r.Seq}
		// A notification whose session moved on (or went) while the hub
		// was down is cleared.
		n.update(ctx, k, cur[k])
	}
	return nil
}

// run follows the change log until ctx ends.
func (n *notifier) run(ctx context.Context) {
	if n.c == nil {
		return
	}
	_, from, err := n.s.st.ChangeRange()
	if err == nil {
		err = n.load(ctx)
	}
	if err != nil {
		n.s.log.Error("notifications: reading the projection", "err", err)
		return
	}
	n.s.log.Info("notifications go to ntfy", "server", n.c.cfg.URL, "topic", n.c.cfg.Topic)
	for {
		ch := n.s.st.Changed()
		lo, _, err := n.s.st.ChangeRange()
		if err != nil {
			n.s.log.Error("notifications: reading the change log", "err", err)
			return
		}
		if lo > from+1 {
			// The change log moved past what was looked at: start again
			// from the projection (and clear what moved on).
			n.s.log.Warn("notifications fell behind the change log; resynchronising")
			_, from, _ = n.s.st.ChangeRange()
			n.resync(ctx)
			continue
		}
		cs, err := n.s.st.ChangesAfter(from, streamPage)
		if err != nil {
			n.s.log.Error("notifications: reading the change log", "err", err)
			return
		}
		if len(cs) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-ch:
			}
			continue
		}
		// The latest summary of each session in the batch (nil: removed).
		latest := map[SessionKey]*SessionSummary{}
		var order []SessionKey
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
	}
}

// resync applies the current projection to every session known.
func (n *notifier) resync(ctx context.Context) {
	list, err := n.s.summaries(SessionFilter{})
	if err != nil {
		n.s.log.Error("notifications: reading the projection", "err", err)
		return
	}
	cur := map[SessionKey]*SessionSummary{}
	for i := range list {
		cur[SessionKey{Machine: list[i].Machine, Session: list[i].Session}] = &list[i]
	}
	for k := range n.state {
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

// update compares a session's state with the one last notified: it clears
// a notification whose state was left and publishes one for a new state.
func (n *notifier) update(ctx context.Context, k SessionKey, sum *SessionSummary) {
	want := notifyKey(sum)
	cur := n.state[k]
	if want == cur.key {
		return
	}
	if cur.seq != "" {
		if err := n.s.st.DeleteActions(cur.seq); err != nil {
			n.s.log.Warn("notifications: removing buttons", "err", err)
		}
		if err := n.c.clear(ctx, cur.seq); err != nil {
			n.s.log.Warn("notifications: clearing a message failed", "machine", k.Machine, "session", k.Session, "err", err)
		} else {
			n.s.log.Info("notification cleared", "machine", k.Machine, "session", k.Session)
		}
		if err := n.s.st.DeleteNotification(k.Machine, k.Session); err != nil {
			n.s.log.Warn("notifications: recording", "err", err)
		}
	}
	if want == "" {
		delete(n.state, k)
		return
	}
	st := noteState{key: want}
	switch {
	case want == SectionNeedsYou && strings.HasPrefix(cur.key, SectionNeedsYou+"/q/"):
		// The question was answered and the session has not reported its
		// next state yet: not news.
	case !n.machineNotifies(k.Machine):
		n.s.log.Debug("no notification: the machine has not named this hub its notifier", "machine", k.Machine, "session", k.Session, "section", sum.Section)
	case n.s.watched(k):
		n.s.log.Debug("no notification: a client follows the session", "machine", k.Machine, "session", k.Session, "section", sum.Section)
	default:
		st.seq = n.publish(ctx, k, sum)
	}
	n.state[k] = st
}

// publish sends the notification of a session's state with its buttons;
// it returns the sequence ID, or "" when it could not be sent.
func (n *notifier) publish(ctx context.Context, k SessionKey, sum *SessionSummary) string {
	seq := newSeq()
	opts := actionOptions(sum)
	var tokens []string
	var actions []Action
	now := n.s.now()
	for i, opt := range opts {
		tok, id, err := NewToken()
		if err != nil {
			return ""
		}
		tokens = append(tokens, tok)
		actions = append(actions, Action{ID: id, Machine: k.Machine, Session: k.Session, Question: sum.Question.ID, Choice: i + 1,
			Label: proto.Truncate(opt, 200), Command: newUUID(), Seq: seq, Expires: now.Add(ActionTTL)})
	}
	if len(actions) > 0 {
		if err := n.s.st.AddActions(actions); err != nil {
			n.s.log.Error("notifications: storing buttons", "err", err)
			return ""
		}
	}
	note := buildNotification(n.s.publicURL, n.c.cfg.Topic, seq, sum, tokens)
	if err := n.c.publish(ctx, note); err != nil {
		n.s.log.Warn("notifications: publishing failed", "machine", k.Machine, "session", k.Session, "err", err)
		_ = n.s.st.DeleteActions(seq)
		return ""
	}
	if err := n.s.st.PutNotification(NotificationRow{Machine: k.Machine, Session: k.Session, Key: notifyKey(sum), Seq: seq, Sent: now}); err != nil {
		n.s.log.Warn("notifications: recording", "err", err)
	}
	n.s.log.Info("notification published", "machine", k.Machine, "session", k.Session, "section", sum.Section, "buttons", len(tokens))
	return seq
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
	tok := r.PathValue("token")
	if !ValidToken(tok) {
		invalid()
		return
	}
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
	raw, err := l.forward(ctx, proto.MethodSessionAnswer, proto.AnswerParams{ID: a.Command, Session: a.Session, Question: a.Question, Choice: a.Choice})
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
		code, msg := rpcStatus(err)
		s.log.Info("notification button", "cert", id.Name, "machine", a.Machine, "session", a.Session, "err", msg)
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
