package gate

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
)

// ExitNoMaster is gate-exec's status when the host has no master
// connection: ssh's own status for a connection that failed.
const ExitNoMaster = 255

// exitRefused is gate-exec's status for a refused command.
const exitRefused = 1

// Exec is one run of `devc gate-exec`, the forced command of the gate's
// sshd. It runs as the user, from sshd, with sshd's environment.
type Exec struct {
	Config *config.Config
	Client *Client
	// BoxID and Host come from the forced command, which the daemon wrote
	// for the box whose channel carried the stream.
	BoxID string
	Host  string
	// RuntimeDir is the gate runtime directory (<runtime>/gate).
	RuntimeDir string
	// Getenv reads SSH_ORIGINAL_COMMAND and SSH_TTY.
	Getenv         func(string) string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Now            func() time.Time
	// Log records an entry (AppendLog when nil).
	Log func(realm string, e Entry) error
}

// box finds the project whose box has the ID.
func (x *Exec) box() (names.ProjectRef, bool) {
	for _, p := range x.Config.Projects("") {
		ref := names.ProjectRef{Realm: p.Realm, Project: p.Name}
		if ref.Validate() == nil && names.BoxID(ref) == x.BoxID {
			return ref, true
		}
	}
	return names.ProjectRef{}, false
}

// Run serves the session and returns gate-exec's exit status: the remote
// command's, or 255 when there is no master, or 1 when the gate refused.
func (x *Exec) Run(ctx context.Context) int {
	now := x.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	original := x.Getenv("SSH_ORIGINAL_COMMAND")
	tty := x.Getenv("SSH_TTY") != ""
	mode := ModeOf(original)
	e := Entry{Time: start.UTC(), Box: x.BoxID, Host: x.Host, Mode: mode, TTY: tty}
	if mode == ModeExec {
		e.Command = original
	}
	ref, ok := x.box()
	if !ok || !ValidHost(x.Host) {
		// No realm to log to: the box is gone from the configuration.
		fmt.Fprintf(x.Stderr, "devc gate: this box is no longer configured on the host; nothing was run\n")
		return exitRefused
	}
	e.Box, e.Project = names.BoxName(ref), ref.String()
	refuse := func(code int, reason, message string) int {
		fmt.Fprintf(x.Stderr, "devc gate: %s\n", message)
		e.Exit, e.Refused = code, reason
		e.Duration = now().Sub(start).Seconds()
		x.log(ref.Realm, e)
		return code
	}
	if probs := x.Config.ProblemsFor(ref); len(probs) > 0 {
		return refuse(exitRefused, "configuration problems", fmt.Sprintf("the configuration of %s has problems on the host; the SSH gate is closed until 'devc config check' is clean (ask the user)", ref))
	}
	r, err := x.Config.Resolved(ref)
	if err != nil {
		return refuse(exitRefused, "not configured", fmt.Sprintf("%s is not configured on the host", ref))
	}
	if _, ok := r.Grants.SSHGrantFor(x.Host); !ok {
		return refuse(exitRefused, "not granted", fmt.Sprintf("%s is not granted to %s (grants.ssh); ask the user", x.Host, ref))
	}
	// In force is what the box was started with and the configuration
	// still grants (finding C1-02).
	caps, err := box.ReadCapabilities(x.Config.Paths, ref)
	if err != nil {
		return refuse(exitRefused, "no capability record", fmt.Sprintf("this box has no usable record of what it was started with (%v); ask the user to restart it ('devc down %s' and 'devc up %s')", err, ref, ref))
	}
	grant, allowed, ok := caps.SSHGrant(r, x.Host)
	if !ok {
		return refuse(exitRefused, "granted after the start", fmt.Sprintf("%s was granted to %s after its box started; ask the user to bring the box up again", x.Host, ref))
	}
	if grant.ReadOnly {
		switch mode {
		case ModeShell:
			return refuse(exitRefused, "read-only: shell", fmt.Sprintf("%s is granted read-only: an interactive shell is not allowed; run one of the allowed commands", x.Host))
		case ModeSFTP:
			return refuse(exitRefused, "read-only: sftp", fmt.Sprintf("%s is granted read-only: sftp, scp and rsync are not allowed", x.Host))
		default:
			if err := CheckReadOnly(original, allowed); err != nil {
				return refuse(exitRefused, "read-only: "+err.Error(), fmt.Sprintf("%s: %v", x.Host, err))
			}
		}
	}
	sock, err := SocketPath(x.RuntimeDir, x.Host)
	if err != nil {
		return refuse(ExitNoMaster, "no master", err.Error())
	}
	if !x.Client.Check(ctx, sock, x.Host) {
		return refuse(ExitNoMaster, "no master", fmt.Sprintf("no SSH connection to %s is established on the host; ask the user to run 'devc ssh login %s'", x.Host, x.Host))
	}
	args, err := x.Client.ExecArgs(sock, x.Host, mode, original, tty)
	if err != nil {
		return refuse(exitRefused, err.Error(), err.Error())
	}
	code, err := x.Client.Runner.Interactive(ctx, execx.Cmd{Name: x.Client.bin(), Args: args, Stdin: x.Stdin, Stdout: x.Stdout, Stderr: x.Stderr})
	if err != nil || code < 0 {
		code = ExitNoMaster
	}
	e.Exit = code
	e.Duration = now().Sub(start).Seconds()
	x.log(ref.Realm, e)
	return code
}

func (x *Exec) log(realm string, e Entry) {
	logf := x.Log
	if logf == nil {
		logf = func(realm string, e Entry) error { return AppendLog(x.Config.Paths, realm, e) }
	}
	if err := logf(realm, e); err != nil {
		fmt.Fprintf(x.Stderr, "devc gate: warning: writing the gate log: %v\n", err)
	}
}
