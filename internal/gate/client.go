package gate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/execx"
)

// checkTimeout bounds `ssh -O check` and `ssh -O exit`.
const checkTimeout = 10 * time.Second

// MinTTL and MaxTTL bound `devc ssh login --ttl`.
const (
	MinTTL = time.Minute
	MaxTTL = 30 * 24 * time.Hour
)

// Client builds and runs the host's ssh commands of the gate.
type Client struct {
	Runner execx.Runner
	// Bin is the ssh executable ("ssh" when empty).
	Bin string
	// Config is passed with -F when set (DEVC_SSH_CONFIG).
	Config string
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "ssh"
	}
	return c.Bin
}

func (c *Client) base() []string {
	if c.Config != "" {
		return []string{"-F", c.Config}
	}
	return nil
}

// controlPath escapes a socket path for -S, where ssh expands %-tokens.
func controlPath(sock string) string { return strings.ReplaceAll(sock, "%", "%%") }

// LoginCmd is `devc ssh login`: a master connection that stays in the
// background for ttl after the first session ends. It runs attached to the
// user's terminal, so password and second-factor prompts work.
func (c *Client) LoginCmd(sock, host string, ttl time.Duration) (execx.Cmd, error) {
	if err := CheckHost(host); err != nil {
		return execx.Cmd{}, err
	}
	if ttl < MinTTL || ttl > MaxTTL {
		return execx.Cmd{}, fmt.Errorf("the time to live must be between %s and %s", MinTTL, MaxTTL)
	}
	args := append(c.base(), "-M", "-S", controlPath(sock),
		"-o", "ControlPersist="+strconv.FormatInt(int64(ttl/time.Second), 10),
		"-o", "ServerAliveInterval=30",
		host, "true")
	return execx.Cmd{Name: c.bin(), Args: args}, nil
}

// controlCmd is `ssh -S <sock> -O <op> <host>`.
func (c *Client) controlCmd(sock, host, op string) execx.Cmd {
	args := append(c.base(), "-S", controlPath(sock), "-O", op, host)
	return execx.Cmd{Name: c.bin(), Args: args, Timeout: checkTimeout, Setsid: true}
}

// Check reports whether the master for host answers on sock.
func (c *Client) Check(ctx context.Context, sock, host string) bool {
	if !ValidHost(host) {
		return false
	}
	_, err := c.Runner.Run(ctx, c.controlCmd(sock, host, "check"))
	return err == nil
}

// Exit asks the master for host to end.
func (c *Client) Exit(ctx context.Context, sock, host string) error {
	if err := CheckHost(host); err != nil {
		return err
	}
	_, err := c.Runner.Run(ctx, c.controlCmd(sock, host, "exit"))
	var ee *execx.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("ssh -O exit %s: %s", host, strings.TrimSpace(string(ee.Result.Stderr)))
	}
	return err
}

// Mode is what a box asked the gate for.
type Mode string

// Modes of a gate session.
const (
	ModeShell Mode = "shell"
	ModeSFTP  Mode = "sftp"
	ModeExec  Mode = "exec"
)

// ModeOf classifies SSH_ORIGINAL_COMMAND. sshd appends the subsystem's
// arguments to its command after a blank (OpenSSH 9.x passes
// "devc-sftp-marker " for none), so blanks around the marker are ignored.
func ModeOf(original string) Mode {
	switch {
	case original == "":
		return ModeShell
	case strings.Trim(original, " \t") == SFTPMarker:
		return ModeSFTP
	}
	return ModeExec
}

// ExecArgs are the arguments of the ssh that gate-exec runs over the
// master: a multiplexing client only. ProxyCommand=false makes sure that
// a master that died after the check is not replaced by a connection of
// its own (which could log in with the user's keys or agent and outlive
// `devc ssh close`); every forwarding, the escape character and any
// RemoteCommand from the user's configuration are switched off.
//
// The command is the box's text as one argument after "--": the remote
// shell parses exactly what the box typed. tty asks for a terminal as the
// box's client did.
func (c *Client) ExecArgs(sock, host string, mode Mode, command string, tty bool) ([]string, error) {
	if err := CheckHost(host); err != nil {
		return nil, err
	}
	args := append(c.base(),
		"-S", controlPath(sock),
		"-o", "ControlMaster=no",
		"-o", "ProxyCommand=false",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "Tunnel=no",
		"-o", "PermitLocalCommand=no",
		"-o", "RemoteCommand=none",
		"-o", "LogLevel=ERROR",
		"-e", "none",
	)
	ttyFlag := "-T"
	if tty {
		ttyFlag = "-tt"
	}
	switch mode {
	case ModeShell:
		args = append(args, ttyFlag, host)
	case ModeSFTP:
		args = append(args, "-T", "-s", host, "sftp")
	case ModeExec:
		if command == "" {
			return nil, errors.New("empty command")
		}
		args = append(args, ttyFlag, host, "--", command)
	default:
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
	return args, nil
}
