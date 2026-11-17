// This file implements the commands that type into live sessions through the
// daemon.

package cli

// Commands that act on live sessions through the daemon (work order C09
// section 6): send, answer, interrupt, keys and seen. Each carries a
// command ID (generated, or --id for a retry that must not act twice) and
// prints the receipt.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/daemon"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/session"
)

// commandFlags are the flags every command shares.
type commandFlags struct {
	id   string
	wait bool
	json bool
}

func (f *commandFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.id, "id", "", "command ID (default: a new one); repeating an ID returns its receipt and does nothing else")
	cmd.Flags().BoolVar(&f.wait, "wait", false, "wait until the outcome is final")
	cmd.Flags().BoolVar(&f.json, "json", false, "print the receipt as JSON")
}

// commandID returns the flag's ID or a new one.
func (f *commandFlags) commandID() (string, error) {
	if f.id == "" {
		return names.NewSessionID(), nil
	}
	if !proto.ValidCommandID(f.id) {
		return "", usageErrorf("--id must look like a session ID or a UUID")
	}
	return f.id, nil
}

// errNeedsDaemon is the message when no daemon answers.
var errNeedsDaemon = errors.New("this command needs the devc daemon, which is not running: start it with 'devc daemon' or 'devc service install'")

// commandClient loads the configuration and connects to the daemon.
func (a *app) commandClient(ctx context.Context) (*config.Config, *daemon.Client, error) {
	c, probs, err := a.load()
	if err != nil {
		return nil, nil, err
	}
	a.warnProblems(probs)
	cl, err := a.daemonClient(ctx, c, nil)
	if err != nil {
		return nil, nil, errNeedsDaemon
	}
	return c, cl, nil
}

// resolveSession turns a session argument into a project and session ID:
// <realm>/<project>/<id> names both; a full ID alone gets the project whose
// journal holds it (none yet: ref "", and the daemon looks for it in the
// boxes and refuses an unknown one with a receipt; two: an error); a
// prefix is looked up in the journals.
func (a *app) resolveSession(c *config.Config, arg string) (string, string, error) {
	if ref, sid, err := proto.ParseSessionKey(arg); err == nil {
		if ref != (names.ProjectRef{}) {
			return ref.String(), sid, nil
		}
		var holders []names.ProjectRef
		for _, r := range allRefs(c) {
			if _, ok := session.FindJournal(c.Paths, []names.ProjectRef{r}, sid); ok {
				holders = append(holders, r)
			}
		}
		switch len(holders) {
		case 0:
			return "", sid, nil
		case 1:
			return holders[0].String(), sid, nil
		}
		return "", "", fmt.Errorf("the session ID %s exists in %s and %s: name it as <realm>/<project>/%s", sid, holders[0], holders[1], sid)
	}
	ref, sid, err := a.findAnySession(c, arg)
	if err != nil {
		return "", "", err
	}
	return ref.String(), sid, nil
}

// runCommand sends one command, waits for its outcome when asked, and
// prints the receipt. Refused and typed-not-submitted exit 1.
func (a *app) runCommand(cmd *cobra.Command, f *commandFlags, method string, params any) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
	defer cancel()
	_, cl, err := a.commandClient(ctx)
	if err != nil {
		return err
	}
	defer cl.Close()
	return a.callCommand(ctx, cl, f, method, params)
}

// callCommand sends a command on an open connection and, with --wait,
// asks command.get (60 seconds at a time) until the receipt is final or
// ctx ends (runCommand gives it 15 minutes); the daemon's work goes on
// either way.
func (a *app) callCommand(ctx context.Context, cl *daemon.Client, f *commandFlags, method string, params any) error {
	var r proto.Receipt
	if err := cl.Call(ctx, method, params, &r); err != nil {
		return fmt.Errorf("the daemon: %w", err)
	}
	for f.wait && !r.Final {
		var next proto.Receipt
		if err := cl.Call(ctx, proto.MethodCommandGet, proto.CommandGetParams{ID: r.Command, Wait: 60}, &next); err != nil {
			return fmt.Errorf("waiting for the outcome: %w", err)
		}
		r = next
		if ctx.Err() != nil {
			break
		}
	}
	if err := a.printReceipt(r, f.json); err != nil {
		return err
	}
	if r.Outcome == proto.OutcomeRefused || r.Outcome == proto.OutcomeTypedNotSubmitted {
		return exitCode(1)
	}
	return nil
}

// printReceipt prints `<id> <outcome>[ session <sid>][: detail]`, the
// detail made printable, or the receipt as JSON.
func (a *app) printReceipt(r proto.Receipt, asJSON bool) error {
	if asJSON {
		return writeJSON(a.stdout, r)
	}
	line := r.Command + " " + r.Outcome
	if r.Session != "" && r.Kind == proto.CommandStart {
		line += " session " + r.Session
	}
	if r.Detail != "" {
		line += ": " + proto.Printable(r.Detail)
	}
	fmt.Fprintln(a.stdout, line)
	return nil
}

// readText reads the text of send from stdin.
func (a *app) readText() (string, error) {
	if a.isTerminal() {
		return "", usageErrorf("give the text as an argument, or pipe it and pass -")
	}
	b, err := io.ReadAll(io.LimitReader(a.stdinReader(), proto.MaxInputText+1))
	if err != nil {
		return "", err
	}
	if len(b) > proto.MaxInputText {
		return "", fmt.Errorf("the text is longer than %d bytes", proto.MaxInputText)
	}
	return strings.TrimRight(string(b), "\n"), nil
}

var choiceRE = regexp.MustCompile(`^[0-9]{1,2}$`)

// newInputCmds returns send, answer, interrupt, keys and seen.
func newInputCmds(a *app) []*cobra.Command {
	var sendF commandFlags
	var steer bool
	send := &cobra.Command{
		Use:   "send <session> [text | -]",
		Short: "Send a prompt to a live session",
		Long: "Type a prompt into a live session through the daemon: pasted as one block and submitted, confirmed by " +
			"the harness's prompt report. By default the prompt waits until the session is idle or done; --steer " +
			"delivers it at once. Without text, or with -, the text is read from stdin. Never typed while someone is " +
			"typing in the session's terminal. Prints the receipt; --wait waits for the final outcome.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageErrorf("usage: devc send <session> [text | -]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := sendF.commandID()
			if err != nil {
				return err
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			ref, sid, err := a.resolveSession(c, args[0])
			if err != nil {
				return err
			}
			var text string
			if len(args) == 1 || args[1] == "-" {
				if text, err = a.readText(); err != nil {
					return err
				}
			} else {
				text = args[1]
			}
			mode := proto.ModeQueue
			if steer {
				mode = proto.ModeSteer
			}
			return a.runCommand(cmd, &sendF, proto.MethodSessionSend, proto.SendParams{ID: id, Session: sid, Ref: ref, Text: text, Mode: mode})
		},
	}
	sendF.add(send)
	send.Flags().BoolVar(&steer, "steer", false, "deliver now instead of when the session is idle or done")

	var answerF commandFlags
	var question string
	var asText bool
	answer := &cobra.Command{
		Use:   "answer <session> <choice | text>",
		Short: "Answer the question a session is asking",
		Long: "Answer the pending question of a session: a number chooses that option, anything else (or --text) is " +
			"free text. A question hook that waits for an answer gets it directly; otherwise the answer is typed into " +
			"the dialog on screen.",
		Args: exactArgs(2, "devc answer <session> <choice | text>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := answerF.commandID()
			if err != nil {
				return err
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			ref, sid, err := a.resolveSession(c, args[0])
			if err != nil {
				return err
			}
			p := proto.AnswerParams{ID: id, Session: sid, Ref: ref, Question: question}
			if !asText && choiceRE.MatchString(args[1]) {
				p.Choice, _ = strconv.Atoi(args[1])
			} else {
				p.Text = args[1]
			}
			return a.runCommand(cmd, &answerF, proto.MethodSessionAnswer, p)
		},
	}
	answerF.add(answer)
	answer.Flags().StringVar(&question, "question", "", "the question's ID (refused when another question is pending)")
	answer.Flags().BoolVar(&asText, "text", false, "send the answer as text even when it is a number")

	var interruptF commandFlags
	interrupt := &cobra.Command{
		Use:   "interrupt <session>",
		Short: "Stop the current turn of a session",
		Args:  exactArgs(1, "devc interrupt <session>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := interruptF.commandID()
			if err != nil {
				return err
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			ref, sid, err := a.resolveSession(c, args[0])
			if err != nil {
				return err
			}
			return a.runCommand(cmd, &interruptF, proto.MethodSessionInterrupt, proto.SessionCommandParams{ID: id, Session: sid, Ref: ref})
		},
	}
	interruptF.add(interrupt)

	var keysF commandFlags
	keys := &cobra.Command{
		Use:   "keys <session> <key>...",
		Short: "Press keys in a session's terminal",
		Long:  "Send named keys to a session: " + strings.Join(proto.NamedKeys, ", ") + ".",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return usageErrorf("usage: devc keys <session> <key>...")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := keysF.commandID()
			if err != nil {
				return err
			}
			for _, k := range args[1:] {
				if !proto.ValidNamedKey(k) {
					return usageErrorf("unknown key %q; known: %s", k, strings.Join(proto.NamedKeys, " "))
				}
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			ref, sid, err := a.resolveSession(c, args[0])
			if err != nil {
				return err
			}
			return a.runCommand(cmd, &keysF, proto.MethodSessionKeys, proto.KeysParams{ID: id, Session: sid, Ref: ref, Keys: args[1:]})
		},
	}
	keysF.add(keys)

	var seenF commandFlags
	var turn int64
	seen := &cobra.Command{
		Use:   "seen <session> [--turn N]",
		Short: "Mark a session's turns as seen, on every device",
		Args:  exactArgs(1, "devc seen <session>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := seenF.commandID()
			if err != nil {
				return err
			}
			if turn < 0 {
				return usageErrorf("--turn must be positive")
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			ref, sid, err := a.resolveSession(c, args[0])
			if err != nil {
				return err
			}
			return a.runCommand(cmd, &seenF, proto.MethodSessionSeen, proto.SeenParams{ID: id, Session: sid, Ref: ref, Turn: turn})
		},
	}
	seenF.add(seen)
	seen.Flags().Int64Var(&turn, "turn", 0, "the last turn seen (default: the current turn)")

	return []*cobra.Command{send, answer, interrupt, keys, seen}
}
