package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/service"
)

// serviceManager returns the manager of the banking timer's units.
func (a *app) serviceManager() (*service.Manager, error) {
	dir := a.getenv("DEVC_SYSTEMD_USER_DIR")
	if dir == "" {
		d, err := service.UserUnitDir(a.getenv)
		if err != nil {
			return nil, err
		}
		dir = d
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("the systemd user directory %q is not absolute", dir)
	}
	return &service.Manager{Dir: dir, Runner: a.runner}, nil
}

// serviceEnv is the environment the timer's service needs: the PATH devc
// found the engine and git on, and the directories set by flag or
// environment for this invocation.
func (a *app) serviceEnv() ([]string, error) {
	var env []string
	if v := a.getenv("PATH"); v != "" {
		env = append(env, "PATH="+v)
	}
	p, err := a.paths()
	if err != nil {
		return nil, err
	}
	if a.configDir != "" || a.getenv(paths.EnvConfigDir) != "" {
		env = append(env, paths.EnvConfigDir+"="+p.ConfigDir())
	}
	if a.stateDir != "" || a.getenv(paths.EnvStateDir) != "" {
		env = append(env, paths.EnvStateDir+"="+p.StateDir())
	}
	if v := a.getenv(paths.EnvRuntimeDir); v != "" {
		env = append(env, paths.EnvRuntimeDir+"="+v)
	}
	return env, nil
}

func newServiceCmd(a *app) *cobra.Command {
	var timerOnly bool
	install := &cobra.Command{
		Use:   "install [--timer-only]",
		Short: "Install the daemon as a systemd user service (or only the banking timer)",
		Long: "Write devc.service to the systemd user directory and enable and start it: it runs 'devc daemon' with " +
			"this devc binary, the current PATH and the configuration and state directories of this invocation. A " +
			"banking timer installed before is removed. --timer-only installs the banking timer instead, for machines " +
			"that should not run the daemon: devc-bank.service and devc-bank.timer run 'devc ingest --all', 'devc bank " +
			"--all' and 'devc context commit --all' every five minutes; the daemon unit is removed then.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			exe, err := a.executable()
			if err != nil {
				return err
			}
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			env, err := a.serviceEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			what := service.DaemonFile
			if timerOnly {
				what = service.TimerFile
				err = m.InstallTimer(ctx, exe, env)
			} else {
				err = m.Install(ctx, exe, env)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "installed and started %s in %s\n", what, m.Dir)
			if u := currentUserName(a); u != "" {
				if _, err := os.Stat(filepath.Join("/var/lib/systemd/linger", u)); err != nil {
					fmt.Fprintf(a.stderr, "devc: note: user services run only while you are logged in; 'loginctl enable-linger %s' keeps them running\n", u)
				}
			}
			return nil
		},
	}
	install.Flags().BoolVar(&timerOnly, "timer-only", false, "install the banking timer instead of the daemon")
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the daemon service or the banking timer",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			if err := m.Uninstall(ctx); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "removed devc's units from %s\n", m.Dir)
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show what is installed and whether it runs",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			switch m.Mode() {
			case service.ModeNone:
				fmt.Fprintf(a.stdout, "not installed (run 'devc service install')\n")
				return exitCode(1)
			case service.ModeDaemon:
				fmt.Fprintf(a.stdout, "installed: %s (the daemon)\n", service.DaemonFile)
			default:
				fmt.Fprintf(a.stdout, "installed: %s (banking timer only)\n", service.TimerFile)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			out, err := m.Status(ctx)
			fmt.Fprint(a.stdout, strings.TrimRight(out, "\n")+"\n")
			return err
		},
	}
	return group("service", "Manage devc's systemd user units (the daemon, or the banking timer)", false, install, uninstall, status)
}

func currentUserName(a *app) string {
	name, _ := currentUser(a.getenv)
	return name
}
