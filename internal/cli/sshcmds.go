package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/gate"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// sshClient is the host's ssh for the gate commands; DEVC_SSH_CONFIG
// replaces the user's configuration (tests).
func (a *app) sshClient() *gate.Client {
	return &gate.Client{Runner: a.runner, Config: a.sshConfigFile()}
}

// sshConfigFile is DEVC_SSH_CONFIG made absolute (host commands run
// from /).
func (a *app) sshConfigFile() string {
	f := a.getenv(gate.EnvSSHConfig)
	if f == "" || filepath.IsAbs(f) {
		return f
	}
	if wd, err := a.getwd(); err == nil {
		return filepath.Join(wd, f)
	}
	return f
}

// gateDir is <runtime>/gate, created 0700.
func (a *app) gateDir(p *paths.Paths) (string, error) {
	dir, err := p.GateRuntimeDir()
	if err != nil {
		return "", err
	}
	if err := p.Ensure(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// removeStaleSocket removes a socket that no master answers on any more:
// ssh refuses to become a master over an existing path.
func removeStaleSocket(sock string) error {
	fi, err := os.Lstat(sock)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; remove it by hand", sock)
	}
	return os.Remove(sock)
}

func newSSHCmd(a *app) *cobra.Command {
	var ttl time.Duration
	login := &cobra.Command{
		Use:   "login <host> [--ttl 12h]",
		Short: "Log in to an SSH host on the host and keep the connection for boxes with a grant",
		Long: "Run ssh to <host> attached to this terminal, so a password or second factor can be typed, and keep the " +
			"connection open in the background as a master for --ttl. Boxes whose project lists the host in grants.ssh " +
			"then reach it with plain 'ssh <host>' through devc's gate, without ever holding a credential: the gate runs " +
			"their commands over this connection, logs them, and allows no forwarding of any kind. A grant is full use of " +
			"the remote account. 'devc ssh close <host>' ends the connection and with it every box's access.\n\n" +
			"A grant written \"<host>:ro\" allows only the commands listed for the host under [grants.ssh_allow], without " +
			"shell syntax, no interactive shell and no sftp; " + gate.ReadOnlyNote + ".",
		Args: exactArgs(1, "login <host>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]
			if err := gate.CheckHost(host); err != nil {
				return usageErrorf("%v", err)
			}
			if ttl < gate.MinTTL || ttl > gate.MaxTTL {
				return usageErrorf("--ttl must be between %s and %s", gate.MinTTL, gate.MaxTTL)
			}
			p, err := a.paths()
			if err != nil {
				return err
			}
			dir, err := a.gateDir(p)
			if err != nil {
				return err
			}
			sock, err := gate.SocketPath(dir, host)
			if err != nil {
				return err
			}
			c := a.sshClient()
			ctx := cmd.Context()
			if c.Check(ctx, sock, host) {
				fmt.Fprintf(a.stdout, "already logged in to %s (end it with 'devc ssh close %s')\n", host, host)
				return nil
			}
			if err := removeStaleSocket(sock); err != nil {
				return err
			}
			lc, err := c.LoginCmd(sock, host, ttl)
			if err != nil {
				return usageErrorf("%v", err)
			}
			if err := os.WriteFile(gate.HostFile(dir, host), []byte(host+"\n"), 0o600); err != nil {
				return err
			}
			code, err := a.runner.Interactive(ctx, lc)
			if err != nil {
				return err
			}
			if code != 0 || !c.Check(ctx, sock, host) {
				_ = os.Remove(gate.HostFile(dir, host))
				return fmt.Errorf("ssh %s failed (exit status %d); no connection is kept", host, code)
			}
			fmt.Fprintf(a.stdout, "logged in to %s for %s; boxes with a grant for it can use it now\n", host, ttl)
			return nil
		},
	}
	login.Flags().DurationVar(&ttl, "ttl", 12*time.Hour, "how long the connection stays open after its last use")

	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [--json]",
		Short: "List the SSH connections kept for boxes and whether they answer",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.paths()
			if err != nil {
				return err
			}
			dir, err := p.GateRuntimeDir()
			if err != nil {
				return err
			}
			rows, err := sshRows(cmd.Context(), a.sshClient(), dir)
			if err != nil {
				return err
			}
			if lsJSON {
				return writeJSON(a.stdout, rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(a.stdout, "no SSH connections; start one with 'devc ssh login <host>'")
				return nil
			}
			tw := table(a.stdout, "HOST", "STATE")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\n", r.Host, r.State)
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	closeCmd := &cobra.Command{
		Use:   "close <host>",
		Short: "End the SSH connection to a host, and with it every box's access",
		Args:  exactArgs(1, "close <host>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]
			if err := gate.CheckHost(host); err != nil {
				return usageErrorf("%v", err)
			}
			p, err := a.paths()
			if err != nil {
				return err
			}
			dir, err := p.GateRuntimeDir()
			if err != nil {
				return err
			}
			sock, err := gate.SocketPath(dir, host)
			if err != nil {
				return err
			}
			c := a.sshClient()
			if !c.Check(cmd.Context(), sock, host) {
				_ = removeStaleSocket(sock)
				_ = os.Remove(gate.HostFile(dir, host))
				fmt.Fprintf(a.stdout, "no connection to %s\n", host)
				return nil
			}
			if err := c.Exit(cmd.Context(), sock, host); err != nil {
				return err
			}
			_ = os.Remove(gate.HostFile(dir, host))
			fmt.Fprintf(a.stdout, "closed the connection to %s\n", host)
			return nil
		},
	}
	return group("ssh", "Keep SSH connections on the host for boxes with a grant (the SSH gate)", false, login, ls, closeCmd)
}

// sshRow is one line of `devc ssh ls`.
type sshRow struct {
	Host  string `json:"host"`
	State string `json:"state"` // alive or dead
}

// sshRows lists the hosts devc ssh login recorded in the gate directory.
func sshRows(ctx context.Context, c *gate.Client, dir string) ([]sshRow, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []sshRow{}, nil
	}
	if err != nil {
		return nil, err
	}
	rows := []sshRow{}
	for _, de := range des {
		if !strings.HasPrefix(de.Name(), "m-") || !strings.HasSuffix(de.Name(), ".host") || !de.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil || len(b) > 512 {
			continue
		}
		host := strings.TrimSpace(string(b))
		if !gate.ValidHost(host) || gate.HostFile(dir, host) != filepath.Join(dir, de.Name()) {
			continue
		}
		sock, err := gate.SocketPath(dir, host)
		if err != nil {
			continue
		}
		state := "dead"
		if c.Check(ctx, sock, host) {
			state = "alive"
		}
		rows = append(rows, sshRow{Host: host, State: state})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Host < rows[j].Host })
	return rows, nil
}

// newGateExecCmd is the forced command of the gate's sshd (hidden): it
// runs one box's SSH session over the master connection.
func newGateExecCmd(a *app) *cobra.Command {
	var boxID, host, runtimeDir, sshConfig string
	cmd := &cobra.Command{
		Use:    "gate-exec --box <box-id> --host <host> --runtime-dir <dir>",
		Short:  "Run one SSH session of a box over the host's master connection (run by the gate's sshd)",
		Hidden: true,
		Args:   noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !names.ValidBoxID(boxID) || !gate.ValidHost(host) || !filepath.IsAbs(runtimeDir) {
				return usageErrorf("gate-exec needs a valid --box, --host and an absolute --runtime-dir")
			}
			p, err := paths.Resolve(paths.Options{ConfigDir: a.configDir, StateDir: a.stateDir, RuntimeDir: runtimeDir, Getenv: a.getenv, Home: a.getenv("HOME")})
			if err != nil {
				return err
			}
			c, _ := config.Load(p)
			dir, err := c.Paths.GateRuntimeDir()
			if err != nil {
				return err
			}
			// SIGHUP or SIGTERM from sshd ends the remote command through
			// the context, and the session is still logged.
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			x := &gate.Exec{
				Config: c, Client: &gate.Client{Runner: a.runner, Config: sshConfig},
				BoxID: boxID, Host: host, RuntimeDir: dir, Getenv: a.getenv,
				Stdin: a.stdin, Stdout: a.stdout, Stderr: a.stderr,
			}
			if code := x.Run(ctx); code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&boxID, "box", "", "box ID")
	f.StringVar(&host, "host", "", "SSH host alias")
	f.StringVar(&runtimeDir, "runtime-dir", "", "runtime directory of the daemon")
	f.StringVar(&sshConfig, "ssh-config", "", "ssh configuration file (DEVC_SSH_CONFIG of the daemon)")
	return cmd
}

// newBoxGateConnectCmd is the ProxyCommand of the generated SSH
// configuration in a box.
func newBoxGateConnectCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:    "gate-connect <host>",
		Short:  "Connect ssh to the host's SSH gate (ProxyCommand)",
		Hidden: true,
		Args:   exactArgs(1, "gate-connect <host>"),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			if code := boxside.GateConnect(boxside.AgentAbstract, args[0], a.stdin, a.stdout, a.stderr); code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
}
