package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
)

// forwardRegistry is the bookkeeping of detached forwards in the runtime
// directory, which no box can write.
func (a *app) forwardRegistry(e *boxEnv) (*netpolicy.Registry, error) {
	rt, err := e.cfg.Paths.RuntimeDir()
	if err != nil {
		return nil, err
	}
	return netpolicy.NewRegistry(filepath.Join(rt, "forwards")), nil
}

// hostExecutable is the devc binary the forwarder runs in the box's
// namespaces, with symlinks resolved.
func (a *app) hostExecutable() (string, error) {
	exe, err := a.executable()
	if err != nil {
		return "", fmt.Errorf("locating the devc executable: %w", err)
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

func newForwardCmd(a *app) *cobra.Command {
	var hostPort int
	var bind string
	var detach bool
	cmd := &cobra.Command{
		Use:   "forward <port> [<realm>/<project>] [--host-port N] [--bind ADDR] [--detach]",
		Short: "Forward a TCP port of a box to the host",
		Long: "Listen on the host (by default 127.0.0.1 and the same port number; if that port is taken, a free one is " +
			"chosen and printed) and relay every connection to 127.0.0.1:<port> inside the box. This reaches services " +
			"that listen only on the box's loopback interface, needs nothing in the image and works at every network " +
			"level, including none. Without --detach the forward runs until Ctrl-C; with --detach it runs in the " +
			"background (see 'devc forward ls' and 'devc forward stop'). A forward ends when its box stops.\n\n" +
			"--bind with an address other than a loopback address makes the port reachable from the network.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageErrorf("usage: devc forward <port> [<realm>/<project>]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := strconv.Atoi(args[0])
			if err != nil || port < 1 || port > 65535 {
				return usageErrorf("%q is not a port number (1-65535)", args[0])
			}
			if hostPort < 0 || hostPort > 65535 {
				return usageErrorf("--host-port must be between 1 and 65535")
			}
			return a.forward(cmd, args[1:], port, hostPort, bind, detach)
		},
	}
	cmd.Flags().IntVar(&hostPort, "host-port", 0, "port on the host (default: the same as in the box, or a free one)")
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "host address to listen on")
	cmd.Flags().BoolVar(&detach, "detach", false, "run the forward in the background")
	cmd.AddCommand(newForwardLsCmd(a), newForwardStopCmd(a), newForwardServeCmd(a))
	return cmd
}

func (a *app) forward(cmd *cobra.Command, refArgs []string, port, hostPort int, bind string, detach bool) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, err := a.resolveRef(e.cfg, refArgs)
	if err != nil {
		return err
	}
	if _, err := execx.LookPath("nsenter"); err != nil {
		return fmt.Errorf("devc forward needs nsenter (package util-linux): %w", err)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
	name, err := e.mgr.EnsureRunning(ctx, ref)
	cancel()
	if err != nil {
		return a.boxError(e.cfg, err)
	}
	want := hostPort
	if want == 0 {
		want = port
	}
	ln, err := netpolicy.Listen(bind, want, hostPort != 0)
	if err != nil {
		return err
	}
	addr := ln.Addr().(*net.TCPAddr)
	if !netpolicy.IsLoopback(bind) {
		fmt.Fprintf(a.stderr, "devc: warning: listening on %s makes port %d of %s reachable from the network, not only from this machine\n", bind, port, name)
	}
	if addr.Port != want {
		fmt.Fprintf(a.stderr, "devc: port %d is taken on the host; using %d instead\n", want, addr.Port)
	}
	fwd, err := a.forwarder(cmd.Context(), e, ref, port, a.stderr)
	if err != nil {
		ln.Close()
		return err
	}
	defer fwd.Box.Close()
	if detach {
		return a.detachForward(e, ref, name, ln.(*net.TCPListener), port, bind, addr.Port)
	}
	fmt.Fprintf(a.stdout, "forwarding %s to port %d in %s; press Ctrl-C to stop\n", addr, port, name)
	sctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := fwd.Serve(sctx, ln); errors.Is(err, netpolicy.ErrBoxGone) {
		return fmt.Errorf("%s stopped; the forward ended", name)
	} else if err != nil {
		return err
	}
	return nil
}

// forwarder prepares the relay to a running box.
func (a *app) forwarder(ctx context.Context, e *boxEnv, ref names.ProjectRef, port int, log io.Writer) (*netpolicy.Forwarder, error) {
	exe, err := a.hostExecutable()
	if err != nil {
		return nil, err
	}
	nsenter, err := execx.LookPath("nsenter")
	if err != nil {
		return nil, fmt.Errorf("devc forward needs nsenter (package util-linux): %w", err)
	}
	ci, err := e.mgr.Find(ctx, ref)
	if err != nil {
		return nil, err
	}
	if ci == nil || !ci.State.Running || ci.State.Pid <= 0 {
		return nil, fmt.Errorf("%s is not running: start it with 'devc up %s'", names.BoxName(ref), ref)
	}
	// The box's namespaces are pinned now and used for every connection
	// (finding B5).
	box, err := netpolicy.Pin(ctx, e.mgr.Engine, names.BoxName(ref), netpolicy.ProcFS{})
	if err != nil {
		return nil, err
	}
	return &netpolicy.Forwarder{Runner: a.runner, Engine: e.mgr.Engine, Nsenter: nsenter, Exe: exe,
		Container: names.BoxName(ref), Box: box, Port: port, Log: log}, nil
}

// detachForward hands the listener to a background devc process (`devc
// forward serve`, file descriptor 3) and records it.
func (a *app) detachForward(e *boxEnv, ref names.ProjectRef, name string, ln *net.TCPListener, port int, bind string, hostPort int) error {
	defer ln.Close()
	reg, err := a.forwardRegistry(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(reg.Dir, 0o700); err != nil {
		return err
	}
	exe, err := a.hostExecutable()
	if err != nil {
		return err
	}
	lf, err := ln.File()
	if err != nil {
		return err
	}
	defer lf.Close()
	id := netpolicy.NewForwardID()
	log, err := os.OpenFile(reg.LogFile(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	var args []string
	if a.configDir != "" {
		args = append(args, "--config-dir", a.configDir)
	}
	if a.stateDir != "" {
		args = append(args, "--state-dir", a.stateDir)
	}
	args = append(args, "forward", "serve", "--id", id, "--port", strconv.Itoa(port), ref.String())
	// The forward is devc itself in the background, not an external tool:
	// it is started directly, in its own session, from "/".
	c := exec.Command(exe, args...)
	c.Dir = "/"
	c.ExtraFiles = []*os.File{lf}
	c.Stdout, c.Stderr = log, log
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return fmt.Errorf("starting the background forward: %w", err)
	}
	rec := netpolicy.Record{ID: id, Ref: ref.String(), Container: name, BoxPort: port, Bind: bind, HostPort: hostPort,
		PID: c.Process.Pid, Started: time.Now().UTC().Truncate(time.Second)}
	if err := reg.Save(rec); err != nil {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
		return err
	}
	_ = c.Process.Release()
	fmt.Fprintf(a.stdout, "forwarding %s to port %d in %s in the background (forward %s; stop it with 'devc forward stop %s')\n",
		net.JoinHostPort(bind, strconv.Itoa(hostPort)), port, name, id, id)
	return nil
}

// newForwardServeCmd is the background side of `devc forward --detach`.
func newForwardServeCmd(a *app) *cobra.Command {
	var id string
	var port int
	cmd := &cobra.Command{
		Use:    "serve --id <id> --port <port> <realm>/<project>",
		Short:  "Serve a detached forward on the listener passed as file descriptor 3 (internal)",
		Args:   exactArgs(1, "devc forward serve --id <id> --port <port> <realm>/<project>"),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !netpolicy.ValidForwardID(id) {
				return usageErrorf("bad forward id %q", id)
			}
			ln, err := net.FileListener(os.NewFile(3, "listener"))
			if err != nil {
				return fmt.Errorf("forward %s: no listener on file descriptor 3: %w", id, err)
			}
			e, err := a.boxEnv()
			if err != nil {
				ln.Close()
				return err
			}
			ref, err := names.Parse(args[0])
			if err != nil {
				ln.Close()
				return err
			}
			reg, err := a.forwardRegistry(e)
			if err != nil {
				ln.Close()
				return err
			}
			defer func() { _ = reg.Remove(id, os.Getpid()) }()
			fwd, err := a.forwarder(cmd.Context(), e, ref, port, os.Stderr)
			if err != nil {
				ln.Close()
				return err
			}
			defer fwd.Box.Close()
			sctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			signal.Ignore(syscall.SIGHUP)
			err = fwd.Serve(sctx, ln)
			if errors.Is(err, netpolicy.ErrBoxGone) {
				fmt.Fprintf(os.Stderr, "devc forward %s: %s stopped; exiting\n", id, fwd.Container)
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "forward id")
	cmd.Flags().IntVar(&port, "port", 0, "port in the box")
	return cmd
}

func newForwardLsCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List detached forwards",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.load()
			if err != nil {
				return err
			}
			reg, err := a.forwardRegistry(&boxEnv{cfg: c})
			if err != nil {
				return err
			}
			recs, err := reg.List()
			if err != nil {
				return err
			}
			if recs == nil {
				recs = []netpolicy.Record{}
			}
			if asJSON {
				return writeJSON(a.stdout, recs)
			}
			tw := table(a.stdout, "ID", "PROJECT", "LISTEN", "BOX PORT", "PID", "STARTED")
			for _, r := range recs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", r.ID, r.Ref, net.JoinHostPort(r.Bind, strconv.Itoa(r.HostPort)), r.BoxPort, r.PID,
					r.Started.Local().Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newForwardStopCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop a detached forward",
		Args:  exactArgs(1, "devc forward stop <id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.load()
			if err != nil {
				return err
			}
			reg, err := a.forwardRegistry(&boxEnv{cfg: c})
			if err != nil {
				return err
			}
			rec, err := reg.Get(args[0])
			if err != nil {
				return err
			}
			if err := syscall.Kill(rec.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("stopping forward %s (pid %d): %w", rec.ID, rec.PID, err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for netpolicy.ProcessAlive(rec) && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			if netpolicy.ProcessAlive(rec) {
				_ = syscall.Kill(rec.PID, syscall.SIGKILL)
			}
			if err := reg.Remove(rec.ID, rec.PID); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "stopped forward %s (%s port %d)\n", rec.ID, rec.Ref, rec.BoxPort)
			return nil
		},
	}
}
