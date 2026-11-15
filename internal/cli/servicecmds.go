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
	install := &cobra.Command{
		Use:   "install",
		Short: "Install a systemd user timer that banks every running box every five minutes",
		Long: "Write devc-bank.service and devc-bank.timer to the systemd user directory and enable the timer. The " +
			"service runs 'devc bank --all --quiet' and 'devc context commit --all' with this devc binary and the " +
			"current PATH.",
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
			if err := m.Install(ctx, exe, env); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "installed and started %s in %s\n", service.TimerFile, m.Dir)
			if u := currentUserName(a); u != "" {
				if _, err := os.Stat(filepath.Join("/var/lib/systemd/linger", u)); err != nil {
					fmt.Fprintf(a.stderr, "devc: note: the timer runs only while you are logged in; 'loginctl enable-linger %s' keeps it running\n", u)
				}
			}
			return nil
		},
	}
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the banking timer",
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
			fmt.Fprintf(a.stdout, "removed %s and %s from %s\n", service.TimerFile, service.ServiceFile, m.Dir)
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the banking timer is installed and when it runs",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			if !m.Installed() {
				fmt.Fprintf(a.stdout, "not installed (run 'devc service install')\n")
				return exitCode(1)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			out, err := m.Status(ctx)
			fmt.Fprint(a.stdout, strings.TrimRight(out, "\n")+"\n")
			return err
		},
	}
	return group("service", "Manage the systemd user timer that banks boxes", false, install, uninstall, status)
}

func currentUserName(a *app) string {
	name, _ := currentUser(a.getenv)
	return name
}
