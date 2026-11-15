package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/boxside"
)

// requireBox refuses to run an in-box command on the host: fix-identity
// rewrites /etc/passwd and must never touch the host's.
func requireBox(getenv func(string) string) error {
	if getenv("DEVC_BOX") != "1" {
		return errors.New("this command runs only inside a devc box (DEVC_BOX is not 1)")
	}
	if _, err := os.Stat(boxside.InfoFile); err != nil {
		return fmt.Errorf("this command runs only inside a devc box (%s is missing)", boxside.InfoFile)
	}
	return nil
}

// newBoxCmd is the hidden group for subcommands that run inside a box
// (plan 3.3). Later chunks add launch, hook, session, snapshot, dial and the
// helper-container workers.
func newBoxCmd(a *app) *cobra.Command {
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Container main process: prepare the home, then wait",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			// SIGHUP must not end the box.
			signal.Ignore(syscall.SIGHUP)
			return boxside.Init(ctx, boxside.DefaultInitOptions(a.stderr))
		},
	}

	fixIdentity := &cobra.Command{
		Use:   "fix-identity",
		Short: "Make UID 1000 the user dev and 2000 the user vault (run as root)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			configured := ""
			if info, err := boxside.ReadInfo("/"); err == nil {
				configured = info.Shell
			} else {
				fmt.Fprintf(a.stderr, "devc box fix-identity: warning: %v\n", err)
			}
			shell := boxside.ChooseShell("/", configured)
			changed, err := boxside.FixIdentity("/", shell)
			if err != nil {
				return err
			}
			if changed {
				fmt.Fprintf(a.stdout, "updated /etc/passwd and /etc/group (shell %s)\n", shell)
			}
			return nil
		},
	}

	shell := &cobra.Command{
		Use:   "shell",
		Short: "Replace this process with the box's login shell",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			configured := ""
			if info, err := boxside.ReadInfo("/"); err == nil {
				configured = info.Shell
			}
			sh := boxside.ChooseShell("/", configured)
			// A leading "-" in argv[0] makes it a login shell.
			err := syscall.Exec(sh, []string{"-" + filepath.Base(sh)}, os.Environ())
			return fmt.Errorf("starting %s: %w", sh, err)
		},
	}

	var stJSON bool
	selftest := &cobra.Command{
		Use:   "selftest",
		Short: "Check the box's hardening by trying what it must refuse",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			info, err := boxside.ReadInfo("/")
			if err != nil {
				fmt.Fprintf(a.stderr, "devc box selftest: warning: %v\n", err)
				info = nil
			}
			checks := boxside.SelfTest(info)
			failed := false
			for _, c := range checks {
				failed = failed || !c.OK
			}
			if stJSON {
				if err := writeJSON(a.stdout, checks); err != nil {
					return err
				}
			} else {
				tw := table(a.stdout, "CHECK", "RESULT", "DETAIL")
				for _, c := range checks {
					res := "ok"
					if !c.OK {
						res = "FAIL"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", c.ID, res, c.Detail)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			if failed {
				return exitCode(1)
			}
			return nil
		},
	}
	selftest.Flags().BoolVar(&stJSON, "json", false, "print JSON")

	return group("box", "Commands that run inside a box (internal)", true, initCmd, fixIdentity, shell, selftest)
}
