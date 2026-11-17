// This file implements question hooks, which may wait for an answer
// from the host and hand it back to the harness.

package boxside

// Questions a harness asks through a hook that can carry an answer back
// (work order C09 section 5): Claude Code's AskUserQuestion reaches a
// PreToolUse hook, which may return the answers; the fake harness has an
// equivalent. The hook records the question and, when nobody is at the
// session's terminal, waits for an answer through the box channel. If
// someone is there, the wait runs out, no daemon is connected, or anything
// fails, it returns nothing and the harness shows its own dialog.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mb706/devc/internal/proto"
)

// Question-path limits.
const (
	// presenceWindow: a client active on the window this recently means a
	// person is at the terminal, and the hook does not wait.
	presenceWindow = 60 * time.Second
	// MaxQuestionWait bounds input.question_wait.
	MaxQuestionWait = 30 * 60
	// questionPoll is how often a waiting hook looks for a person at the
	// terminal.
	questionPoll = 2 * time.Second
)

// questionSpec is a question in the harness-independent form.
type questionSpec struct {
	id      string
	text    string
	options []string
}

// runQuestionHook handles a question hook of the claude (PreToolUse of
// AskUserQuestion) or fake kind.
func runQuestionHook(kind, session string, in []byte, e HookEnv) {
	switch kind {
	case "claude":
		claudeQuestion(session, in, e)
	case "fake":
		fakeQuestion(session, in, e)
	}
}

// fakeQuestion: input {"id","text","options"}; output {"choice","text"}.
func fakeQuestion(session string, in []byte, e HookEnv) {
	var q struct {
		ID      string   `json:"id"`
		Text    string   `json:"text"`
		Options []string `json:"options"`
	}
	if err := json.Unmarshal(in, &q); err != nil {
		_ = AppendEvents(e.SpoolDir, session, e.now(), noteF("fake", "ask", "malformed hook input"))
		return
	}
	spec := questionSpec{id: short(q.ID, proto.MaxID), text: q.Text, options: q.Options}
	reply := askQuestion(session, spec, e)
	if reply == nil || e.Stdout == nil {
		return
	}
	b, _ := json.Marshal(reply)
	_, _ = e.Stdout.Write(append(b, '\n'))
}

// claudeQuestion handles Claude Code's PreToolUse hook for AskUserQuestion:
// tool_input {questions: [{question, header, options: [{label,
// description}], multiSelect}]}. An answer is returned as the vendor's
// hook reference describes: permissionDecision "allow" and updatedInput
// with the original input plus answers {question text: option label}.
// Only a single question is answered remotely; several at once show the
// dialog.
func claudeQuestion(session string, in []byte, e HookEnv) {
	var m map[string]json.RawMessage
	if json.Unmarshal(in, &m) != nil || str(m, "tool_name") != "AskUserQuestion" {
		return
	}
	input := obj(m, "tool_input")
	var questions []struct {
		Question string `json:"question"`
		Options  []struct {
			Label string `json:"label"`
		} `json:"options"`
	}
	if raw, ok := input["questions"]; !ok || json.Unmarshal(raw, &questions) != nil || len(questions) == 0 {
		_ = AppendEvents(e.SpoolDir, session, e.now(), noteF("claude", "PreToolUse", "AskUserQuestion without questions"))
		return
	}
	id := short(str(m, "tool_use_id"), proto.MaxID)
	if id == "" {
		id = questionID()
	}
	first := questions[0]
	spec := questionSpec{id: id, text: first.Question}
	for _, o := range first.Options {
		spec.options = append(spec.options, o.Label)
	}
	if len(questions) > 1 {
		spec.text += fmt.Sprintf(" (and %d more questions)", len(questions)-1)
		recordQuestion(session, spec, e)
		return
	}
	reply := askQuestion(session, spec, e)
	if reply == nil || e.Stdout == nil {
		return
	}
	label := resolveAnswer(spec.options, reply)
	if label == "" {
		return
	}
	updated := map[string]json.RawMessage{}
	for k, v := range input {
		updated[k] = v
	}
	answers, _ := json.Marshal(map[string]string{first.Question: label})
	updated["answers"] = answers
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":      "PreToolUse",
		"permissionDecision": "allow",
		"updatedInput":       updated,
	}}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	_, _ = e.Stdout.Write(append(b, '\n'))
}

// resolveAnswer turns a reply into an option label: the chosen option, an
// option whose label matches the text, or the text itself.
func resolveAnswer(options []string, r *proto.QuestionReply) string {
	if r.Choice >= 1 && r.Choice <= len(options) {
		return options[r.Choice-1]
	}
	if r.Choice != 0 {
		return ""
	}
	for _, o := range options {
		if strings.EqualFold(strings.TrimSpace(o), strings.TrimSpace(r.Text)) {
			return o
		}
	}
	return r.Text
}

// questionPayloads is the question within the event limits, and state
// blocked.
func questionPayloads(spec questionSpec) []Payload {
	q := proto.Question{ID: spec.id, Text: proto.Truncate(spec.text, proto.MaxQuestionText)}
	for i, o := range spec.options {
		if i == proto.MaxOptions {
			break
		}
		q.Options = append(q.Options, proto.Truncate(o, proto.MaxOptionText))
	}
	if q.ID == "" {
		q.ID = questionID()
	}
	if utf8.RuneCountInString(q.ID) > proto.MaxID {
		q.ID = short(q.ID, proto.MaxID)
	}
	return []Payload{
		{proto.TypeQuestion, q},
		// Written even while the status wrapper reports states: the harness
		// still counts as working while its hook waits.
		{proto.TypeState, proto.State{State: proto.StateBlocked, Kind: proto.KindQuestion, Source: proto.SourceHook}},
	}
}

// recordQuestion appends the question to the spool.
func recordQuestion(session string, spec questionSpec, e HookEnv) {
	_ = AppendEvents(e.SpoolDir, session, e.now(), questionPayloads(spec))
}

// askQuestion records the question and waits for a remote answer when
// nobody is at the terminal; nil means "show the dialog".
func askQuestion(session string, spec questionSpec, e HookEnv) *proto.QuestionReply {
	if spec.id == "" {
		spec.id = questionID()
	}
	wait := 0
	if e.Launch != nil {
		if l, err := e.Launch(e.Getenv("DEVC_HARNESS")); err == nil {
			wait = min(l.Input.QuestionWait, MaxQuestionWait)
		}
	}
	in := &Input{Tmux: e.Tmux, Now: e.Now}
	present := func(since time.Duration) bool {
		if e.Tmux == nil {
			return true
		}
		c, err := in.Clients(session)
		return err != nil || recentlyActive(c, since)
	}
	if wait <= 0 || e.Agent == nil || present(presenceWindow) {
		recordQuestion(session, spec, e)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second+5*time.Second)
	defer cancel()
	type result struct {
		r   proto.QuestionReply
		err error
	}
	done := make(chan result, 1)
	// The request is started before the question is recorded, but nothing
	// waits for it, so the daemon may ingest the question first; an answer
	// then waits a moment for the hook (commander.answerByHook in the
	// daemon) before it is typed on screen.
	go func() {
		var r proto.QuestionReply
		err := e.Agent(ctx, proto.MethodHostQuestion, proto.QuestionParams{Session: session, Question: spec.id, Timeout: wait}, &r)
		done <- result{r, err}
	}()
	recordQuestion(session, spec, e)
	started := e.now()
	tick := time.NewTicker(questionPoll)
	defer tick.Stop()
	for {
		select {
		case res := <-done:
			if res.err != nil || (res.r.Choice == 0 && res.r.Text == "") {
				return nil
			}
			_ = AppendEvents(e.SpoolDir, session, e.now(), []Payload{{proto.TypeState, proto.State{State: proto.StateWorking, Source: proto.SourceHook}}})
			return &res.r
		case <-tick.C:
			// Someone started using the terminal: the dialog is theirs.
			if present(e.now().Sub(started)) {
				return nil
			}
		}
	}
}
