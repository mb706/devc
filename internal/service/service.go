// Package service installs the systemd user timer that ingests session
// events, banks every running box and commits the realms' context
// repositories every five minutes (work order C05 section 4, finding B3):
// devc-bank.service runs `devc ingest --all --quiet`, `devc bank --all
// --quiet` and `devc context commit --all`; devc-bank.timer starts it.
//
// Security rules: unit files are written only below the user's systemd
// directory, by rename; every value placed in a unit is checked for
// control characters and quoted for systemd, so no value can add a
// directive or a command (README rule 3). systemctl runs with an argument
// vector.
package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mb706/devc/internal/execx"
)

// Unit names.
const (
	Name        = "devc-bank"
	ServiceFile = Name + ".service"
	TimerFile   = Name + ".timer"
)

// Interval is how often the timer fires.
const Interval = 5 * time.Minute

// quote quotes one word for an ExecStart= or Environment= line: in double
// quotes, with backslash and quote escaped, "%" doubled (specifiers) and,
// for command lines, "$" doubled (variable expansion).
func quote(s string, command bool) (string, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%q contains a control character", s)
		}
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	q := r.Replace(s)
	if command {
		q = strings.ReplaceAll(q, "$", "$$")
	}
	return `"` + q + `"`, nil
}

// ServiceText renders devc-bank.service: exe is the devc binary, env the
// variables it needs (PATH, overridden directories).
func ServiceText(exe string, env []string) (string, error) {
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("the devc executable %q is not an absolute path", exe)
	}
	qexe, err := quote(exe, true)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Written by 'devc service install'; remove it with 'devc service uninstall'.\n")
	b.WriteString("[Unit]\nDescription=devc: ingest session events, bank every running box and commit the context repositories\n\n")
	b.WriteString("[Service]\nType=oneshot\n")
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t\"\\") {
			return "", fmt.Errorf("invalid environment entry %q", kv)
		}
		q, err := quote(kv, false)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "Environment=%s\n", q)
	}
	// "-": a failure must not keep the next steps from running. Session
	// events are ingested on their own first, so a bank that fails or
	// waits does not hold them back (finding B3).
	fmt.Fprintf(&b, "ExecStart=-%s ingest --all --quiet\n", qexe)
	fmt.Fprintf(&b, "ExecStart=-%s bank --all --quiet\n", qexe)
	fmt.Fprintf(&b, "ExecStart=%s context commit --all\n", qexe)
	return b.String(), nil
}

// TimerText renders devc-bank.timer.
func TimerText() string {
	m := int(Interval / time.Minute)
	return fmt.Sprintf("# Written by 'devc service install'; remove it with 'devc service uninstall'.\n"+
		"[Unit]\nDescription=devc: bank boxes every %d minutes\n\n"+
		"[Timer]\nOnBootSec=%dmin\nOnUnitActiveSec=%dmin\nAccuracySec=30s\n\n"+
		"[Install]\nWantedBy=timers.target\n", m, m, m)
}

// UserUnitDir is the systemd user unit directory: $XDG_CONFIG_HOME/systemd/user
// or ~/.config/systemd/user.
func UserUnitDir(getenv func(string) string) (string, error) {
	if x := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "systemd", "user"), nil
	}
	if h := getenv("HOME"); filepath.IsAbs(h) {
		return filepath.Join(h, ".config", "systemd", "user"), nil
	}
	return "", errors.New("cannot find the systemd user directory: set HOME or XDG_CONFIG_HOME")
}

// Manager installs, removes and inspects the units.
type Manager struct {
	// Dir is the systemd user unit directory.
	Dir    string
	Runner execx.Runner
	// Systemctl is the systemctl command (default "systemctl").
	Systemctl string
}

func (m *Manager) systemctl(ctx context.Context, args ...string) (execx.Result, error) {
	bin := m.Systemctl
	if bin == "" {
		bin = "systemctl"
	}
	return m.Runner.Run(ctx, execx.Cmd{Name: bin, Args: append([]string{"--user"}, args...), Dir: "/", Timeout: time.Minute})
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".new-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Install writes both units, reloads the user manager and enables and
// starts the timer.
func (m *Manager) Install(ctx context.Context, exe string, env []string) error {
	svc, err := ServiceText(exe, env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(m.Dir, ServiceFile), []byte(svc)); err != nil {
		return fmt.Errorf("writing %s: %w", ServiceFile, err)
	}
	if err := writeAtomic(filepath.Join(m.Dir, TimerFile), []byte(TimerText())); err != nil {
		return fmt.Errorf("writing %s: %w", TimerFile, err)
	}
	if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("reloading the systemd user manager: %w", err)
	}
	if _, err := m.systemctl(ctx, "enable", "--now", TimerFile); err != nil {
		return fmt.Errorf("enabling %s: %w", TimerFile, err)
	}
	return nil
}

// Uninstall stops and disables the timer and removes both units. Units
// that are not installed are not an error.
func (m *Manager) Uninstall(ctx context.Context) error {
	// disable fails when the unit is unknown; the files decide.
	_, disableErr := m.systemctl(ctx, "disable", "--now", TimerFile)
	removed := false
	for _, f := range []string{TimerFile, ServiceFile} {
		err := os.Remove(filepath.Join(m.Dir, f))
		switch {
		case err == nil:
			removed = true
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}
	if !removed {
		return nil
	}
	if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("reloading the systemd user manager: %w", err)
	}
	_ = disableErr
	return nil
}

// Installed reports whether the unit files exist.
func (m *Manager) Installed() bool {
	_, err1 := os.Stat(filepath.Join(m.Dir, ServiceFile))
	_, err2 := os.Stat(filepath.Join(m.Dir, TimerFile))
	return err1 == nil && err2 == nil
}

// Status returns systemd's view of the timer and the last run.
func (m *Manager) Status(ctx context.Context) (string, error) {
	var b strings.Builder
	res, err := m.systemctl(ctx, "list-timers", "--all", "--no-pager", TimerFile)
	b.Write(res.Stdout)
	if err != nil && len(res.Stdout) == 0 {
		return b.String(), err
	}
	// status exits 3 for an inactive oneshot service: that is its normal state.
	res, _ = m.systemctl(ctx, "status", "--no-pager", "--lines=5", ServiceFile)
	b.WriteString("\n")
	b.Write(res.Stdout)
	return b.String(), nil
}
