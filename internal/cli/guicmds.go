package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/gui"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/toolbox"
)

// guiManager is the desktop sidecar manager (work order C19) for a box
// environment; it is also the box manager's dependent, so down, rm and
// every start remove the sidecar first.
func (a *app) guiManager(e *boxEnv) *gui.Manager {
	return &gui.Manager{Box: e.mgr, Engine: e.mgr.Engine, Paths: e.cfg.Paths, Toolbox: e.mgr.Toolbox,
		ImageContext: filepath.Join(e.cfg.Paths.CacheDir(), "aux", "desktop"),
		Forwards:     cliForwards{a: a, e: e}, Out: a.stderr}
}

// cliForwards gives the sidecar manager the detached forwards of `devc
// forward --detach`.
type cliForwards struct {
	a *app
	e *boxEnv
}

func (f cliForwards) Start(ctx context.Context, ref names.ProjectRef, boxPort int, bind string, hostPort int, explicit bool) (string, int, error) {
	if _, err := execx.LookPath("nsenter"); err != nil {
		return "", 0, fmt.Errorf("forwarding needs nsenter (package util-linux): %w", err)
	}
	ln, err := netpolicy.Listen(bind, hostPort, explicit)
	if err != nil {
		return "", 0, err
	}
	addr := ln.Addr().(*net.TCPAddr)
	if !netpolicy.IsLoopback(bind) {
		fmt.Fprintf(f.a.stderr, "devc: warning: listening on %s makes the desktop of %s reachable from the network, not only from this machine\n",
			bind, names.BoxName(ref))
	}
	if addr.Port != hostPort {
		fmt.Fprintf(f.a.stderr, "devc: port %d is taken on the host; using %d instead\n", hostPort, addr.Port)
	}
	// Pinning the box's namespaces here checks the box before the
	// background forward takes over.
	fwd, err := f.a.forwarder(ctx, f.e, ref, boxPort, f.a.stderr)
	if err != nil {
		ln.Close()
		return "", 0, err
	}
	fwd.Box.Close()
	rec, err := f.a.spawnForward(f.e, ref, names.BoxName(ref), ln.(*net.TCPListener), boxPort, bind, addr.Port)
	if err != nil {
		return "", 0, err
	}
	return rec.ID, rec.HostPort, nil
}

func (f cliForwards) Stop(id string) error {
	reg, err := f.a.forwardRegistry(f.e)
	if err != nil {
		return err
	}
	rec, err := reg.Get(id)
	if err != nil {
		return nil // not running
	}
	return stopForward(reg, rec)
}

func (f cliForwards) Alive(id string) bool {
	reg, err := f.a.forwardRegistry(f.e)
	if err != nil {
		return false
	}
	_, err = reg.Get(id)
	return err == nil
}

func newGUICmd(a *app) *cobra.Command {
	var restart, rebuild bool
	var hostPort int
	var bind string
	up := &cobra.Command{
		Use:   "up [<realm>/<project>] [--restart] [--host-port N] [--bind ADDR] [--rebuild]",
		Short: "Start a desktop for a box and forward its web viewer to the host",
		Long: "Start the desktop sidecar of a project's box (starting the box if needed): a second container that shares only " +
			"the box's network namespace and runs a virtual X display with a VNC server and the noVNC web viewer, all on the " +
			"box's loopback interface. The viewer is forwarded to the host (127.0.0.1, port 6080 or a free one) and its address " +
			"is printed with the VNC password, which is new for every start. Programs in the box draw on the display with " +
			"DISPLAY=127.0.0.1:1 and the cookie in /run/devc/display.xauth; sessions and 'devc sh' started from then on have both " +
			"set. Anything in the box can therefore drive and read the display. The image localhost/devc-aux/desktop is built on " +
			"first use. A desktop that already runs is kept (its address is printed again) unless --restart is given.\n\n" +
			"--bind with an address other than a loopback address makes the viewer reachable from the network; treat that " +
			"like exposing the hub (see docs/guide.md).",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if hostPort < 0 || hostPort > 65535 {
				return usageErrorf("--host-port must be between 1 and 65535")
			}
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				v, err := a.guiManager(e).Up(ctx, ref, gui.UpOptions{Restart: restart, Rebuild: rebuild, Bind: bind,
					HostPort: hostPort, Explicit: hostPort != 0})
				if err != nil {
					return err
				}
				if v.Reused {
					fmt.Fprintf(a.stdout, "the desktop of %s already runs\n", v.Box)
				}
				fmt.Fprintf(a.stdout, "desktop of %s: %s\n", v.Box, v.URL())
				fmt.Fprintf(a.stdout, "(forward %s; the browser keeps the part after '#', with the password, to itself)\n", v.Forward)
				fmt.Fprintf(a.stdout, "programs in the box draw on it with DISPLAY=%s and XAUTHORITY=%s, which sessions and 'devc sh' started from now on have set\n",
					gui.Display, gui.InBoxXAuthority)
				fmt.Fprintf(a.stdout, "stop it with 'devc gui down %s'\n", ref)
				return nil
			})
		},
	}
	up.Flags().BoolVar(&restart, "restart", false, "replace a running desktop (new cookie and password)")
	up.Flags().IntVar(&hostPort, "host-port", 0, "port on the host (default: 6080, or a free one)")
	up.Flags().StringVar(&bind, "bind", "127.0.0.1", "host address to listen on")
	up.Flags().BoolVar(&rebuild, "rebuild", false, "build the desktop image again first (fresh base image, no layer cache)")

	down := &cobra.Command{
		Use:   "down [<realm>/<project>]",
		Short: "Stop a box's desktop",
		Long:  "Stop the viewer's forward, remove the desktop sidecar and the display files of the box. 'devc down' and 'devc rm' do this too.",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				return a.guiManager(e).Down(ctx, ref)
			})
		},
	}

	var stJSON bool
	status := &cobra.Command{
		Use:   "status [<realm>/<project>] [--json]",
		Short: "Show whether a box's desktop runs and where its viewer is",
		Long: "Report the desktop sidecar of a project's box: absent, running (with the viewer's address, without the password), " +
			"stopped (the container has exited), or stale (it belongs to an earlier start of the box, or the box is not running; " +
			"'devc gui down' or the next 'devc up' removes it).",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				st, err := a.guiManager(e).Status(ctx, ref)
				if err != nil {
					return err
				}
				if stJSON {
					return writeJSON(a.stdout, st)
				}
				line := fmt.Sprintf("desktop of %s: %s", st.Box, st.State)
				if st.Started != nil {
					line += " since " + st.Started.Local().Format("2006-01-02 15:04")
				}
				if st.Viewer != "" {
					line += fmt.Sprintf(", viewer %s (forward %s)", st.Viewer, st.Forward)
				}
				if st.Display != "" {
					line += ", DISPLAY=" + st.Display
				}
				if st.Reason != "" {
					line += "; " + st.Reason
				}
				fmt.Fprintln(a.stdout, line)
				return nil
			})
		},
	}
	status.Flags().BoolVar(&stJSON, "json", false, "print JSON")

	var buildRebuild bool
	build := &cobra.Command{
		Use:   "build [--rebuild]",
		Short: "Build the desktop image (done on first use otherwise)",
		Long: "Build localhost/devc-aux/desktop from the files embedded in devc when it is missing or out of date; --rebuild " +
			"builds it again with a fresh base image and no layer cache, for security updates.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.boxEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
			defer cancel()
			if err := toolbox.EnsureAuxImage(ctx, e.mgr.Engine, gui.AuxImage(), filepath.Join(e.cfg.Paths.CacheDir(), "aux", "desktop"),
				buildRebuild, a.stderr); err != nil {
				return fmt.Errorf("building %s: %w", gui.Image, err)
			}
			fmt.Fprintf(a.stdout, "%s is up to date\n", gui.Image)
			return nil
		},
	}
	build.Flags().BoolVar(&buildRebuild, "rebuild", false, "build again with a fresh base image and no layer cache")

	return group("gui", "Watch graphical programs in a box: a desktop with a web viewer", false, up, down, status, build)
}

// chromiumFlags start a Chromium in a box so that the host can watch it.
const chromiumFlags = "--remote-debugging-port=%d --remote-debugging-address=127.0.0.1 --no-sandbox"

func newBrowserCmd(a *app) *cobra.Command {
	var port, hostPort int
	var detach bool
	cmd := &cobra.Command{
		Use:   "browser [<realm>/<project>] [--port 9222] [--host-port N] [--detach]",
		Short: "Forward the debugging port of a browser in a box, to watch it from the host",
		Long: "Forward 127.0.0.1:<port> of a project's box (default 9222) to the host, like 'devc forward', and print what to " +
			"open in Chrome or Chromium on the host: chrome://inspect (add the printed address under \"Configure…\"), or the " +
			"/json endpoint. In the box, Chromium must run with " + fmt.Sprintf(chromiumFlags, 9222) + " (nested user " +
			"namespaces, which Chromium's own sandbox needs, are blocked in a box). Whoever can connect to the forwarded port " +
			"controls that browser; it listens on the host's loopback interface only.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if port < 1 || port > 65535 {
				return usageErrorf("--port must be between 1 and 65535")
			}
			if hostPort < 0 || hostPort > 65535 {
				return usageErrorf("--host-port must be between 1 and 65535")
			}
			return a.forward(cmd, args, port, hostPort, "127.0.0.1", detach, func(addr string) {
				a.browserHint(addr, port)
			})
		},
	}
	cmd.Flags().IntVar(&port, "port", 9222, "the debugging port in the box")
	cmd.Flags().IntVar(&hostPort, "host-port", 0, "port on the host (default: the same as in the box, or a free one)")
	cmd.Flags().BoolVar(&detach, "detach", false, "run the forward in the background (see 'devc forward ls')")
	return cmd
}

// browserHint says how to open the forwarded debugging port and whether a
// browser answers there yet.
func (a *app) browserHint(addr string, port int) {
	fmt.Fprintf(a.stdout, "DevTools endpoint: http://%s/json/version\n", addr)
	fmt.Fprintf(a.stdout, "in Chrome or Chromium on this machine open chrome://inspect, choose \"Configure…\" next to \"Discover network targets\", "+
		"add %s, and the box's pages appear under \"Remote Target\"\n", addr)
	if browserAnswers(addr) {
		return
	}
	fmt.Fprintf(a.stderr, "devc: no browser answers on port %d in the box yet; start Chromium there with "+chromiumFlags+"\n", port, port)
}

// browserAnswers probes the forwarded DevTools endpoint. Whatever listens
// there is the box's, so a redirect is never followed (finding D2-05): its
// target would be fetched from the host's network namespace, past the
// box's rules. A redirect means that no browser was found.
func browserAnswers(addr string) bool {
	cl := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := cl.Get("http://" + addr + "/json/version")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK && strings.Contains(string(b), "Browser")
}
