package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/toolbox"
)

// Auth is a harness's authentication in one realm: the manifest with the
// realm's override applied.
type Auth struct {
	Method string
	Env    string
	Secret string
}

// EffectiveAuth applies the realm's [harness.<id>] override.
func EffectiveAuth(c *config.Config, realm string, m *config.Harness) Auth {
	a := Auth{Method: m.Auth.Method, Env: m.Auth.Env, Secret: m.Auth.Secret}
	if r, ok := c.Realm(realm); ok {
		if o, ok := r.Harness[m.ID]; ok {
			if o.Auth != "" {
				a.Method = o.Auth
			}
			if o.Secret != "" {
				a.Secret = o.Secret
			}
		}
	}
	return a
}

// LoginRunner runs vendor login and status commands in throwaway
// containers from the installer image, with the toolbox mounted read-only.
type LoginRunner struct {
	Config       *config.Config
	Toolbox      *toolbox.Toolbox
	Engine       engine.Engine
	SecurityOpts []string
	// Term is the host's TERM, passed to interactive logins.
	Term string
	TTY  bool
	Out  io.Writer
}

// command maps a manifest command to the container: a first word equal to
// the harness's bin is its real executable.
func command(m *config.Harness, argv []string) []string {
	out := append([]string(nil), argv...)
	if len(out) > 0 && out[0] == m.Bin {
		out[0] = toolbox.InBoxExe(m)
	}
	return out
}

// options builds the container for a login or status command. A
// temporary home is mounted at /home/dev; with sharedHome the realm's
// shared harness home is mounted at the harness home, so the vendor's own
// credential file lands in realm state.
func (l *LoginRunner) options(realm string, m *config.Harness, argv []string, tmpHome string, sharedHome bool) (engine.RunOptions, error) {
	if ok, err := l.Toolbox.IsInstalled(m); err != nil {
		return engine.RunOptions{}, err
	} else if !ok {
		return engine.RunOptions{}, fmt.Errorf("%s is not installed: run 'devc harness install %s' first", m.ID, m.ID)
	}
	if len(argv) == 0 {
		return engine.RunOptions{}, fmt.Errorf("the %s manifest has no command for this", m.ID)
	}
	if m.Install.Method == "none" {
		return engine.RunOptions{}, fmt.Errorf("%s comes with the project image, not the toolbox; log in from a box instead", m.ID)
	}
	mounts := []engine.RunMount{
		{Source: l.Toolbox.Dir, Target: paths.InBoxTools, ReadOnly: true},
		{Source: tmpHome, Target: paths.InBoxHome},
	}
	if sharedHome {
		dir := filepath.Join(l.Config.Paths.HarnessStateDir(realm, m.ID), "home")
		if err := l.Config.Paths.Ensure(dir); err != nil {
			return engine.RunOptions{}, err
		}
		mounts = append(mounts, engine.RunMount{Source: dir, Target: path.Join(paths.InBoxHome, m.Home.Dir)})
		// Further state directories (home.also): Cursor writes its login
		// to ~/.config/cursor.
		for _, a := range m.Home.Also {
			adir := AlsoStateDir(l.Config.Paths, realm, m.ID, a)
			if err := l.Config.Paths.Ensure(adir); err != nil {
				return engine.RunOptions{}, err
			}
			mounts = append(mounts, engine.RunMount{Source: adir, Target: path.Join(paths.InBoxHome, a)})
		}
	}
	env := []string{"HOME=" + paths.InBoxHome}
	// The toolbox is mounted whole; the components the harness needs come
	// first in PATH (install.needs).
	if dirs, missing := l.Toolbox.NeedsPath(m); missing != "" {
		return engine.RunOptions{}, fmt.Errorf("%s needs the toolbox component %s: run 'devc toolbox install %s'", m.ID, missing, missing)
	} else if len(dirs) > 0 {
		env = append(env, "PATH="+strings.Join(dirs, ":")+":"+toolbox.SystemPATH)
	}
	if l.Term != "" && !strings.ContainsAny(l.Term, "\x00\n") {
		env = append(env, "TERM="+l.Term)
	}
	merged := map[string]string{}
	for k, v := range m.Env {
		merged[k] = v
	}
	for k, v := range m.Home.Env {
		merged[k] = v
	}
	for _, k := range sortedKeys(merged) {
		env = append(env, k+"="+merged[k])
	}
	return engine.RunOptions{
		Image: toolbox.InstallerImage, SecurityOpts: l.SecurityOpts,
		Labels: map[string]string{toolbox.LabelRole: toolbox.RoleAux, "devc.aux": "login"},
		Mounts: mounts, Tmpfs: toolbox.InstallerTmpfs, Env: env, Workdir: paths.InBoxHome, Cmd: command(m, argv),
	}, nil
}

// tempHome creates a private temporary home below local/tmp.
func (l *LoginRunner) tempHome() (string, func(), error) {
	p := l.Config.Paths
	if err := p.Ensure(p.TmpDir()); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(p.TmpDir(), "login-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// Interactive runs the manifest's login command attached to the terminal.
// The vendor's output is shown to the user and not parsed.
func (l *LoginRunner) Interactive(ctx context.Context, realm string, m *config.Harness, sharedHome bool) error {
	home, cleanup, err := l.tempHome()
	if err != nil {
		return err
	}
	defer cleanup()
	o, err := l.options(realm, m, m.Auth.Login, home, sharedHome)
	if err != nil {
		return err
	}
	o.Interactive, o.TTY = true, l.TTY
	code, err := l.Engine.RunStream(ctx, o)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the login command of %s exited with status %d", m.ID, code)
	}
	return nil
}

// Status runs the manifest's status command with the realm's shared home
// and returns its output.
func (l *LoginRunner) Status(ctx context.Context, realm string, m *config.Harness) (string, int, error) {
	home, cleanup, err := l.tempHome()
	if err != nil {
		return "", -1, err
	}
	defer cleanup()
	o, err := l.options(realm, m, m.Auth.Status, home, true)
	if err != nil {
		return "", -1, err
	}
	o.Timeout = 2 * time.Minute
	res, err := l.Engine.Run(ctx, o)
	out := strings.TrimSpace(string(res.Stdout) + "\n" + string(res.Stderr))
	var ee *execx.ExitError
	if errors.As(err, &ee) {
		return out, ee.Result.ExitCode, nil
	}
	return out, res.ExitCode, err
}
