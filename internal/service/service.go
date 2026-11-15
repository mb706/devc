// Package service installs devc's systemd user units. By default that is
// devc.service, which runs the machine daemon (work order C08): it ingests
// session events continuously and banks and commits every five minutes
// itself. On machines that should not run the daemon, the banking timer of
// work order C05 (finding B3) does the periodic part instead:
// devc-bank.service runs `devc ingest --all --quiet`, `devc bank --all
// --quiet` and `devc context commit --all`; devc-bank.timer starts it. The
// two modes exclude each other: installing one removes the other.
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
	// DaemonFile runs `devc daemon`.
	DaemonFile = "devc.service"
)

// Modes of an installation.
const (
	ModeNone   = ""
	ModeDaemon = "daemon"
	ModeTimer  = "timer"
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

// unitHead writes the header and [Service] lines shared by both service
// units and returns the quoted executable.
func unitHead(b *strings.Builder, exe, description, typ string, env []string) (string, error) {
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("the devc executable %q is not an absolute path", exe)
	}
	qexe, err := quote(exe, true)
	if err != nil {
		return "", err
	}
	b.WriteString("# Written by 'devc service install'; remove it with 'devc service uninstall'.\n")
	fmt.Fprintf(b, "[Unit]\nDescription=%s\n\n", description)
	fmt.Fprintf(b, "[Service]\nType=%s\n", typ)
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t\"\\") {
			return "", fmt.Errorf("invalid environment entry %q", kv)
		}
		q, err := quote(kv, false)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(b, "Environment=%s\n", q)
	}
	return qexe, nil
}

// DaemonText renders devc.service, which runs the daemon: exe is the devc
// binary, env the variables it needs (PATH, overridden directories).
func DaemonText(exe string, env []string) (string, error) {
	var b strings.Builder
	qexe, err := unitHead(&b, exe, "devc machine daemon: box channels, session events, banking", "simple", env)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "ExecStart=%s daemon\n", qexe)
	b.WriteString("Restart=on-failure\nRestartSec=5s\n\n[Install]\nWantedBy=default.target\n")
	return b.String(), nil
}

// ServiceText renders devc-bank.service: exe is the devc binary, env the
// variables it needs (PATH, overridden directories).
func ServiceText(exe string, env []string) (string, error) {
	var b strings.Builder
	qexe, err := unitHead(&b, exe, "devc: ingest session events, bank every running box and commit the context repositories", "oneshot", env)
	if err != nil {
		return "", err
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

// Install installs the daemon unit, removes the timer units if present,
// reloads the user manager and enables and starts the daemon.
func (m *Manager) Install(ctx context.Context, exe string, env []string) error {
	svc, err := DaemonText(exe, env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	if err := m.removeTimer(ctx); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(m.Dir, DaemonFile), []byte(svc)); err != nil {
		return fmt.Errorf("writing %s: %w", DaemonFile, err)
	}
	if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("reloading the systemd user manager: %w", err)
	}
	// restart (not just start) so that a reinstall runs the new binary.
	if _, err := m.systemctl(ctx, "enable", DaemonFile); err != nil {
		return fmt.Errorf("enabling %s: %w", DaemonFile, err)
	}
	if _, err := m.systemctl(ctx, "restart", DaemonFile); err != nil {
		return fmt.Errorf("starting %s: %w", DaemonFile, err)
	}
	return nil
}

// InstallTimer writes the timer units (for machines without the daemon),
// removes the daemon unit if present, reloads the user manager and enables
// and starts the timer.
func (m *Manager) InstallTimer(ctx context.Context, exe string, env []string) error {
	svc, err := ServiceText(exe, env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	if err := m.removeDaemon(ctx); err != nil {
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

// removeTimer stops, disables and removes the timer units if installed.
func (m *Manager) removeTimer(ctx context.Context) error {
	if !exists(filepath.Join(m.Dir, TimerFile)) && !exists(filepath.Join(m.Dir, ServiceFile)) {
		return nil
	}
	// disable fails when the unit is unknown; the files decide.
	_, _ = m.systemctl(ctx, "disable", "--now", TimerFile)
	return removeFiles(m.Dir, TimerFile, ServiceFile)
}

// removeDaemon stops, disables and removes the daemon unit if installed.
func (m *Manager) removeDaemon(ctx context.Context) error {
	if !exists(filepath.Join(m.Dir, DaemonFile)) {
		return nil
	}
	_, _ = m.systemctl(ctx, "disable", "--now", DaemonFile)
	return removeFiles(m.Dir, DaemonFile)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func removeFiles(dir string, files ...string) error {
	for _, f := range files {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Uninstall stops and disables whatever is installed and removes the unit
// files. Units that are not installed are not an error.
func (m *Manager) Uninstall(ctx context.Context) error {
	mode := m.Mode()
	if mode == ModeNone {
		// Leftovers of a partial installation are removed quietly.
		return removeFiles(m.Dir, DaemonFile, TimerFile, ServiceFile)
	}
	if err := m.removeTimer(ctx); err != nil {
		return err
	}
	if err := m.removeDaemon(ctx); err != nil {
		return err
	}
	if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("reloading the systemd user manager: %w", err)
	}
	return nil
}

// Mode reports what is installed: the daemon, the timer, or nothing.
func (m *Manager) Mode() string {
	switch {
	case exists(filepath.Join(m.Dir, DaemonFile)):
		return ModeDaemon
	case exists(filepath.Join(m.Dir, ServiceFile)) && exists(filepath.Join(m.Dir, TimerFile)):
		return ModeTimer
	}
	return ModeNone
}

// Installed reports whether either mode is installed.
func (m *Manager) Installed() bool { return m.Mode() != ModeNone }

// Status returns systemd's view of what is installed.
func (m *Manager) Status(ctx context.Context) (string, error) {
	var b strings.Builder
	if m.Mode() == ModeDaemon {
		// status exits 3 for an inactive service; the output says so.
		res, err := m.systemctl(ctx, "status", "--no-pager", "--lines=5", DaemonFile)
		b.Write(res.Stdout)
		if err != nil && len(res.Stdout) == 0 {
			return b.String(), err
		}
		return b.String(), nil
	}
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
