package boxside

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"
	"unicode/utf8"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// MaxHookInput bounds what a hook receiver reads from stdin.
const MaxHookInput = 1 << 20

// HookEnv is what the hook receiver reads from its process.
type HookEnv struct {
	Getenv   func(string) string
	Stdin    io.Reader
	SpoolDir string
	Now      func() time.Time
}

// RunHook is `devc box hook <kind> <event>`: it maps the harness's hook
// input to events and appends them to the session's spool file. It never
// fails and never prints: some harnesses add hook output to the model's
// context, and a failing hook must never disturb a session. Without a valid
// DEVC_SESSION it does nothing.
func RunHook(kind, event string, e HookEnv) {
	defer func() { _ = recover() }()
	session := e.Getenv("DEVC_SESSION")
	in, err := io.ReadAll(io.LimitReader(e.Stdin, MaxHookInput+1))
	// Drain the rest so the harness never blocks writing to the hook.
	_, _ = io.Copy(io.Discard, e.Stdin)
	if !names.ValidSessionID(session) {
		return
	}
	var ps []Payload
	switch {
	case err != nil:
		ps = noteF(kind, event, "reading the hook input failed")
	case len(in) > MaxHookInput:
		ps = noteF(kind, event, "hook input larger than 1 MiB ignored")
	default:
		ps = MapHook(kind, event, in)
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	_ = AppendEvents(e.SpoolDir, session, now(), ps)
}

func noteF(kind, event, msg string) []Payload {
	return []Payload{{proto.TypeNote, proto.Note{Text: proto.Truncate(kind+" hook "+event+": "+msg, proto.MaxNoteText)}}}
}

// MapHook maps one hook input of a harness kind to event payloads.
func MapHook(kind, event string, in []byte) []Payload {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(in, &m); err != nil || m == nil {
		return noteF(kind, safeLabel(event), "malformed hook input")
	}
	switch kind {
	case "claude":
		return mapClaude(event, m)
	case "codex":
		return mapCodex(event, m)
	case "fake":
		return mapFake(event, in)
	}
	return noteF(safeLabel(kind), safeLabel(event), "unknown hook kind")
}

// safeLabel shortens a label taken from arguments for a note.
func safeLabel(s string) string {
	return proto.Truncate(s, 64)
}

// str returns the string field k of m, or "" when it is absent or not a
// string.
func str(m map[string]json.RawMessage, k string) string {
	var s string
	if raw, ok := m[k]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// obj returns the object field k of m, or nil.
func obj(m map[string]json.RawMessage, k string) map[string]json.RawMessage {
	var o map[string]json.RawMessage
	if raw, ok := m[k]; ok {
		_ = json.Unmarshal(raw, &o)
	}
	return o
}

// absPath keeps p when it looks like an absolute path within limits.
func absPath(p string) string {
	if len(p) == 0 || p[0] != '/' || len(p) > proto.MaxPath {
		return ""
	}
	for _, r := range p {
		if r == 0 {
			return ""
		}
	}
	return p
}

func short(s string, max int) string {
	if utf8.RuneCountInString(s) > max {
		return string([]rune(s)[:max])
	}
	return s
}

func harnessPayloads(m map[string]json.RawMessage) []Payload {
	return []Payload{
		{proto.TypeHarness, proto.Harness{
			ID:         short(str(m, "session_id"), proto.MaxID),
			Transcript: absPath(str(m, "transcript_path")),
			Cwd:        absPath(str(m, "cwd")),
			Source:     short(str(m, "source"), proto.MaxKind),
		}},
		{proto.TypeState, proto.State{State: proto.StateIdle}},
	}
}

// PromptData describes a submitted prompt: hash and length of the whole
// text, and its beginning.
func PromptData(text string) proto.Prompt {
	sum := sha256.Sum256([]byte(text))
	return proto.Prompt{SHA256: hex.EncodeToString(sum[:]), Length: utf8.RuneCountInString(text), Text: proto.Truncate(text, proto.MaxPromptText)}
}

func promptPayloads(m map[string]json.RawMessage) []Payload {
	return []Payload{
		{proto.TypePrompt, PromptData(str(m, "prompt"))},
		{proto.TypeState, proto.State{State: proto.StateWorking}},
	}
}

func stopPayloads(m map[string]json.RawMessage) []Payload {
	var ps []Payload
	if msg := str(m, "last_assistant_message"); msg != "" {
		ps = append(ps, Payload{proto.TypeMessage, proto.Message{Text: proto.Truncate(msg, proto.MaxMessageText)}})
	}
	return append(ps, Payload{proto.TypeState, proto.State{State: proto.StateDone}})
}

func endPayloads(m map[string]json.RawMessage) []Payload {
	reason := short(str(m, "reason"), proto.MaxReason)
	if reason == "" {
		reason = "other"
	}
	return []Payload{{proto.TypeSessionEnd, proto.SessionEnd{Reason: reason}}}
}

// questionID returns a random identifier for a question the harness did not
// number itself.
func questionID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "q" + hex.EncodeToString(b)
}

func blocked(kind, text string) []Payload {
	return []Payload{
		{proto.TypeQuestion, proto.Question{ID: questionID(), Text: proto.Truncate(text, proto.MaxQuestionText)}},
		{proto.TypeState, proto.State{State: proto.StateBlocked, Kind: kind}},
	}
}

// mapClaude maps Claude Code's hooks (field names from the vendor's hook
// reference: session_id, transcript_path, cwd, source, prompt,
// last_assistant_message, message, title, notification_type, reason).
func mapClaude(event string, m map[string]json.RawMessage) []Payload {
	switch event {
	case "SessionStart":
		return harnessPayloads(m)
	case "UserPromptSubmit":
		return promptPayloads(m)
	case "Stop":
		return stopPayloads(m)
	case "Notification":
		msg := str(m, "message")
		switch t := str(m, "notification_type"); t {
		case "permission_prompt":
			return blocked(proto.KindPermission, msg)
		case "elicitation_dialog", "elicitation_url_dialog", "agent_needs_input":
			return blocked(proto.KindQuestion, msg)
		case "idle_prompt":
			return []Payload{{proto.TypeState, proto.State{State: proto.StateDone}}}
		default:
			text := msg
			if t != "" {
				text = t + ": " + msg
			}
			if text == "" {
				text = "notification"
			}
			return []Payload{{proto.TypeNote, proto.Note{Text: proto.Truncate(text, proto.MaxNoteText)}}}
		}
	case "SessionEnd":
		return endPayloads(m)
	}
	return noteF("claude", safeLabel(event), "unhandled event")
}

// mapCodex maps Codex's hooks (field names from the vendor's hook
// reference: session_id, transcript_path, cwd, source, prompt,
// last_assistant_message, tool_name, tool_input.description, reason).
// PermissionRequest is Codex's counterpart of Claude's permission prompt.
func mapCodex(event string, m map[string]json.RawMessage) []Payload {
	switch event {
	case "SessionStart":
		return harnessPayloads(m)
	case "UserPromptSubmit":
		return promptPayloads(m)
	case "Stop":
		return stopPayloads(m)
	case "PermissionRequest":
		input := obj(m, "tool_input")
		text := str(input, "description")
		if text == "" {
			text = "permission to use " + short(str(m, "tool_name"), proto.MaxToolName)
			if cmd := str(input, "command"); cmd != "" {
				text += ": " + cmd
			}
		}
		return blocked(proto.KindPermission, text)
	case "SessionEnd":
		return endPayloads(m)
	}
	return noteF("codex", safeLabel(event), "unhandled event")
}

// mapFake passes the fake harness's events through: {"type": …, "data": …}.
func mapFake(event string, in []byte) []Payload {
	var f struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(in, &f); err != nil || !proto.Known(f.Type) {
		return noteF("fake", safeLabel(event), "invalid input")
	}
	data, err := proto.ValidateData(f.Type, f.Data)
	if err != nil {
		return noteF("fake", safeLabel(event), "invalid "+f.Type+" data")
	}
	return []Payload{{f.Type, data}}
}
