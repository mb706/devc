// This file implements the daemon's input to session panes: listing the
// clients, pasting text and sending keys.

package boxside

// Input to a session's pane (work order C09 sections 3 and 4): what the box
// agent does when the daemon asks over the channel. The text of a paste
// reaches tmux on standard input (load-buffer -), never as an argument or
// through a shell, and key names are checked against a strict pattern, so
// no part of user text can become tmux or shell syntax.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/tmux"
)

// TmuxStdinRunner runs tmux with args and data on its standard input.
type TmuxStdinRunner func(args []string, stdin []byte) (string, error)

// ExecTmuxStdin is the production TmuxStdinRunner.
func ExecTmuxStdin(environ []string) TmuxStdinRunner {
	return func(args []string, stdin []byte) (string, error) {
		cmd := exec.Command(tmux.Binary, args...)
		cmd.Env = tmux.Env(environ)
		cmd.Dir = "/"
		cmd.Stdin = bytes.NewReader(stdin)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return out.String(), &TmuxError{Args: args, Stderr: errb.String(), Err: err}
		}
		return out.String(), nil
	}
}

// Input performs input operations on session panes.
type Input struct {
	Tmux      TmuxRunner
	TmuxStdin TmuxStdinRunner
	// Pause is the wait between a paste and its submit key (150 ms).
	Pause time.Duration
	Now   func() time.Time
	Sleep func(time.Duration)
}

func (in *Input) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

// keyRE is what a tmux key name may look like: letters, digits and
// dashes, starting with a letter or digit ("Enter", "C-c", "M-Enter", "1").
var keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,31}$`)

// ValidKeyName reports whether k may be passed to send-keys.
func ValidKeyName(k string) bool { return keyRE.MatchString(k) }

// ErrNoPane is returned when the session has no live pane in this box.
var ErrNoPane = errors.New("the session has no live window in this box")

// pane finds the live pane and window of a session.
func (in *Input) pane(session string) (tmux.Pane, error) {
	if !names.ValidSessionID(session) {
		return tmux.Pane{}, fmt.Errorf("invalid session %q", session)
	}
	out, err := in.Tmux(tmux.ListPanes())
	if err != nil {
		if tmux.NoServer(tmuxStderr(err)) {
			return tmux.Pane{}, ErrNoPane
		}
		return tmux.Pane{}, err
	}
	for _, p := range tmux.ParsePanes(out) {
		if p.Session == session && !p.Dead {
			return p, nil
		}
	}
	return tmux.Pane{}, ErrNoPane
}

// Clients reports the tmux clients showing the session's window and their
// latest activity.
func (in *Input) Clients(session string) (proto.BoxClients, error) {
	p, err := in.pane(session)
	if err != nil {
		return proto.BoxClients{}, err
	}
	res := proto.BoxClients{Window: p.Window, Now: in.now().Unix()}
	out, err := in.Tmux(tmux.ListClients())
	if err != nil {
		if tmux.NoServer(tmuxStderr(err)) {
			return res, nil
		}
		return res, err
	}
	for _, c := range tmux.ParseClients(out) {
		if c.Window != p.Window {
			continue
		}
		res.Clients++
		res.LastActivity = max(res.LastActivity, c.Activity)
	}
	return res, nil
}

// Paste pastes text into the session's pane as one bracketed paste
// (bracketed when the program asked for it) from a uniquely named buffer,
// then, after a pause, sends the submit key.
func (in *Input) Paste(session, text, submit string) error {
	if err := proto.CheckInputText(text); err != nil {
		return err
	}
	if submit != "" && !ValidKeyName(submit) {
		return fmt.Errorf("invalid submit key %q", submit)
	}
	p, err := in.pane(session)
	if err != nil {
		return err
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	buf := "devc-in-" + hex.EncodeToString(b)
	if _, err := in.TmuxStdin(tmux.LoadBuffer(buf), []byte(text)); err != nil {
		return fmt.Errorf("loading the text: %w", err)
	}
	args, err := tmux.PasteBuffer(buf, p.Pane)
	if err != nil {
		return err
	}
	if _, err := in.Tmux(args); err != nil {
		_, _ = in.Tmux(tmux.DeleteBuffer(buf))
		return fmt.Errorf("pasting: %w", err)
	}
	if submit == "" {
		return nil
	}
	pause := in.Pause
	if pause <= 0 {
		pause = 150 * time.Millisecond
	}
	sleep := time.Sleep
	if in.Sleep != nil {
		sleep = in.Sleep
	}
	sleep(pause)
	return in.sendKeys(p.Pane, []string{submit})
}

// Keys sends named keys to the session's pane.
func (in *Input) Keys(session string, keys []string) error {
	if len(keys) == 0 || len(keys) > 32 {
		return errors.New("between 1 and 32 keys")
	}
	for _, k := range keys {
		if !ValidKeyName(k) {
			return fmt.Errorf("invalid key %q", k)
		}
	}
	p, err := in.pane(session)
	if err != nil {
		return err
	}
	return in.sendKeys(p.Pane, keys)
}

func (in *Input) sendKeys(pane string, keys []string) error {
	args, err := tmux.SendKeys(pane, keys)
	if err != nil {
		return err
	}
	if _, err := in.Tmux(args); err != nil {
		return fmt.Errorf("sending keys: %w", err)
	}
	return nil
}

// recentlyActive reports whether a client showed the window within d.
func recentlyActive(c proto.BoxClients, d time.Duration) bool {
	return c.Clients > 0 && c.LastActivity > 0 && c.Now-c.LastActivity < int64(d/time.Second)
}
