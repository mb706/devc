package daemon

// Input adapters (work order C09 section 4): one interface, chosen by the
// manifest's input.adapter. Only paste can be exercised on this machine;
// the structured adapters for Claude Code (a mod), Codex (a second
// app-server client) and pi (an extension) are described in
// docs/dev/LEFTOVER.md and report themselves unavailable, in which case
// the daemon falls back to paste and says so in the receipt.

import (
	"context"
	"errors"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/proto"
)

// Capabilities describe what an adapter can do.
type Capabilities struct {
	Available bool `json:"available"`
	// Reason says why an adapter is not available.
	Reason string `json:"reason,omitempty"`
	// Confirms says how delivery is confirmed ("prompt-hash", …).
	Confirms string `json:"confirms,omitempty"`
	Steer    bool   `json:"steer"`
	Answer   bool   `json:"answer"`
}

// Adapter delivers input to one live session. Each method returns a final
// outcome and a detail for the receipt. Before its first key, after the
// input gate, every method calls t.begin and returns what it says when it
// says anything: nothing may be typed then (findings C1-06, C1-13,
// C1-14).
type Adapter interface {
	Name() string
	Capabilities() Capabilities
	Send(ctx context.Context, t *target, text string) (string, string)
	Interrupt(ctx context.Context, t *target) (string, string)
	Answer(ctx context.Context, t *target, choice int, text string) (string, string)
}

// ErrNotAvailable is what a structured adapter answers while it cannot be
// used.
var ErrNotAvailable = errors.New("not available")

// adapterFor picks the manifest's adapter, or paste (with a note for the
// receipt) when that one is not available.
func adapterFor(m *config.Harness) (Adapter, string) {
	var a Adapter
	switch m.Input.Adapter {
	case "mod":
		a = unavailable{name: "mod", reason: "the Claude Code mod is not built yet (docs/dev/LEFTOVER.md)"}
	case "app-server":
		a = unavailable{name: "app-server", reason: "the Codex app-server client is not built yet (docs/dev/LEFTOVER.md)"}
	case "extension":
		a = unavailable{name: "extension", reason: "the pi extension is not built yet (docs/dev/LEFTOVER.md)"}
	default:
		return paste{}, ""
	}
	if caps := a.Capabilities(); !caps.Available {
		return paste{}, "the " + a.Name() + " adapter is " + ErrNotAvailable.Error() + " (" + caps.Reason + "); used paste"
	}
	return a, ""
}

// unavailable stands for a structured adapter that cannot be used yet.
type unavailable struct{ name, reason string }

func (u unavailable) Name() string { return u.name }
func (u unavailable) Capabilities() Capabilities {
	return Capabilities{Available: false, Reason: u.reason}
}
func (u unavailable) Send(context.Context, *target, string) (string, string) {
	return proto.OutcomeRefused, u.name + " adapter " + ErrNotAvailable.Error()
}
func (u unavailable) Interrupt(context.Context, *target) (string, string) {
	return proto.OutcomeRefused, u.name + " adapter " + ErrNotAvailable.Error()
}
func (u unavailable) Answer(context.Context, *target, int, string) (string, string) {
	return proto.OutcomeRefused, u.name + " adapter " + ErrNotAvailable.Error()
}

// paste types into the session's tmux pane: a bracketed paste of the text,
// a pause, the submit key, confirmed by the harness's prompt event.
type paste struct{}

func (paste) Name() string { return "paste" }

func (paste) Capabilities() Capabilities {
	return Capabilities{Available: true, Confirms: "prompt-hash", Steer: true, Answer: true}
}

// Send pastes text and submits it. The text is typed once; when no
// matching prompt event follows, only the submit key is sent again.
func (paste) Send(ctx context.Context, t *target, text string) (string, string) {
	c := t.c
	if err := t.gate(ctx); err != nil {
		return proto.OutcomeRefused, boxErr(err)
	}
	if outcome, detail := t.begin(); outcome != "" {
		return outcome, detail
	}
	before := c.lastSeq(t.key())
	submit := t.m.Input.SubmitKey
	if err := t.w.call(ctx, proto.MethodBoxPaste, proto.BoxInputParams{Session: t.session, Text: text, Submit: submit}, nil); err != nil {
		return refusedOrUnknown(err)
	}
	if t.m.Hooks.Kind == "" {
		return proto.OutcomeUnknown, "typed and submitted; the harness reports no prompts, so delivery cannot be confirmed"
	}
	hashes := promptHashes(text)
	if c.waitPrompt(ctx, t.key(), before, hashes, confirmWait) {
		return proto.OutcomeDelivered, ""
	}
	if ctx.Err() != nil {
		return proto.OutcomeUnknown, "the daemon stopped before the delivery was confirmed"
	}
	if err := t.gate(ctx); err != nil {
		return proto.OutcomeTypedNotSubmitted, "typed, but no prompt was reported and the window is gone"
	}
	if err := t.w.call(ctx, proto.MethodBoxKeys, proto.BoxInputParams{Session: t.session, Keys: []string{submit}}, nil); err != nil {
		return proto.OutcomeTypedNotSubmitted, "typed, but no prompt was reported; sending the submit key again failed: " + boxErr(err)
	}
	if c.waitPrompt(ctx, t.key(), before, hashes, retryWait) {
		return proto.OutcomeDelivered, "after a second submit key"
	}
	return proto.OutcomeTypedNotSubmitted, "typed, but no prompt was reported after two submit keys"
}

// Interrupt sends the manifest's interrupt keys and waits for a state
// change.
func (paste) Interrupt(ctx context.Context, t *target) (string, string) {
	c := t.c
	if err := t.gate(ctx); err != nil {
		return proto.OutcomeRefused, boxErr(err)
	}
	if outcome, detail := t.begin(); outcome != "" {
		return outcome, detail
	}
	before := c.lastSeq(t.key())
	if err := t.w.call(ctx, proto.MethodBoxKeys, proto.BoxInputParams{Session: t.session, Keys: t.m.Input.Interrupt()}, nil); err != nil {
		return refusedOrUnknown(err)
	}
	if c.waitState(ctx, t.key(), before, stateWait) {
		return proto.OutcomeDelivered, ""
	}
	return proto.OutcomeUnknown, "keys sent; the session reported no state change"
}

// Answer answers a dialog on screen: the keys for option choice, or the
// text pasted and submitted; confirmed by a state change.
func (paste) Answer(ctx context.Context, t *target, choice int, text string) (string, string) {
	c := t.c
	if err := t.gate(ctx); err != nil {
		return proto.OutcomeRefused, boxErr(err)
	}
	if outcome, detail := t.begin(); outcome != "" {
		return outcome, detail
	}
	before := c.lastSeq(t.key())
	var err error
	if choice > 0 {
		err = t.w.call(ctx, proto.MethodBoxKeys, proto.BoxInputParams{Session: t.session, Keys: answerKeys(t.m, choice)}, nil)
	} else {
		err = t.w.call(ctx, proto.MethodBoxPaste, proto.BoxInputParams{Session: t.session, Text: text, Submit: t.m.Input.SubmitKey}, nil)
	}
	if err != nil {
		return refusedOrUnknown(err)
	}
	if c.waitState(ctx, t.key(), before, stateWait) {
		return proto.OutcomeDelivered, "typed into the dialog"
	}
	return proto.OutcomeUnknown, "typed into the dialog; the session reported no state change"
}
