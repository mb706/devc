package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/toolbox"
)

// Describe builds the launch description of an installed harness for a
// project (work order C03 section 3).
func Describe(r *config.Resolved, m *config.Harness, rh config.ResolvedHarness) *boxside.Launch {
	home := path.Join(paths.InBoxHome, m.Home.Dir)
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	for k, v := range m.Home.Env {
		env[k] = v
	}
	l := &boxside.Launch{
		Schema: boxside.LaunchSchema, ID: m.ID, Bin: m.Bin, Display: m.Display,
		Realm: r.Realm, Project: r.Project,
		Exe: toolbox.InBoxExe(m), Env: env,
		Args: nonNil(m.Launch.Args), Resume: nonNil(m.Launch.Resume),
		Auth:  boxside.LaunchAuth{Method: rh.AuthMethod},
		Home:  boxside.LaunchHome{Dir: home, Strategy: m.Home.Strategy, Shared: nonNil(m.Home.Shared), PerBox: nonNil(m.Home.PerBox)},
		Seeds: []boxside.LaunchSeed{}, Secrets: []boxside.LaunchSecret{},
		Hooks:  m.Hooks.Kind,
		Status: boxside.LaunchStatus{OSC7501: m.Status.OSC7501},
		Input:  boxside.LaunchInput{Adapter: m.Input.Adapter, SubmitKey: m.Input.SubmitKey},
		Memory: m.Memory.Kind, Transcripts: nonNil(m.Transcripts.Globs),
	}
	if rh.AuthMethod == "env-token" || rh.AuthMethod == "api-key" {
		l.Auth.Env, l.Auth.Secret = rh.AuthEnv, rh.Secret
		if rh.Secret != "" {
			l.Auth.File = path.Join(paths.InBoxSecret, rh.Secret)
		}
	}
	for _, s := range m.Home.Seed {
		l.Seeds = append(l.Seeds, boxside.LaunchSeed{
			Source: path.Join(paths.InBoxHarnessEtc, m.ID, "seed", s.Path),
			Target: path.Join(home, s.Path),
			When:   s.When,
		})
	}
	for _, s := range r.Grants.Secrets {
		ls := boxside.LaunchSecret{Name: s, File: path.Join(paths.InBoxSecret, s)}
		if envVarOK(s) {
			ls.Env = s
		}
		l.Secrets = append(l.Secrets, ls)
	}
	return l
}

func envVarOK(s string) bool {
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for _, c := range s {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// BaselineDir is the in-box directory a harness's baseline is mounted at,
// or "" when the manifest has no baseline. Validation guarantees that the
// settings and instructions paths share it.
func BaselineDir(m *config.Harness) string {
	if s := m.Baseline.Settings; s != nil {
		return path.Dir(s.Path)
	}
	if in := m.Baseline.Instructions; in != nil {
		return path.Dir(in.Path)
	}
	return ""
}

// BoxHooks connects harnesses to the box lifecycle: it contributes the
// mounts (box.Contributor), renders everything at `devc up`
// (box.Preparer) and removes the delivered secrets at down and rm
// (box.Cleaner).
type BoxHooks struct {
	Config  *config.Config
	Toolbox *toolbox.Toolbox
	Secrets secrets.Store
	Out     io.Writer
}

var (
	_ box.Contributor = (*BoxHooks)(nil)
	_ box.Preparer    = (*BoxHooks)(nil)
	_ box.Cleaner     = (*BoxHooks)(nil)
)

func (h *BoxHooks) warnf(format string, args ...any) {
	if h.Out != nil {
		fmt.Fprintf(h.Out, "devc: warning: "+format+"\n", args...)
	}
}

// manifests returns the manifests of the project's harnesses in order.
func (h *BoxHooks) manifests(r *config.Resolved) ([]*config.Harness, error) {
	var out []*config.Harness
	for _, rh := range r.Harnesses {
		m, ok := h.Config.Harness(rh.ID)
		if !ok {
			return nil, fmt.Errorf("no harness manifest with id %q", rh.ID)
		}
		out = append(out, m)
	}
	return out, nil
}

// Contribute implements box.Contributor. The mounts depend only on the
// configuration, never on what is installed, so installing a harness does
// not change the box definition.
func (h *BoxHooks) Contribute(in *box.Inputs, s *box.Spec) error {
	r := in.Resolved
	ref := names.ProjectRef{Realm: r.Realm, Project: r.Project}
	p := in.Paths
	ms, err := h.manifests(r)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if dir := BaselineDir(m); dir != "" {
			if err := s.AddMount(box.Mount{Source: p.BoxBaselineDir(ref, m.ID), Target: dir, ReadOnly: true}); err != nil {
				return err
			}
		}
		home := path.Join(paths.InBoxHome, m.Home.Dir)
		state := p.HarnessStateDir(r.Realm, m.ID)
		switch m.Home.Strategy {
		case "private":
			for _, sub := range m.Home.Shared {
				if err := s.AddMount(box.Mount{Source: filepath.Join(state, filepath.FromSlash(sub)), Target: path.Join(home, sub)}); err != nil {
					return err
				}
			}
		case "shared":
			if err := s.AddMount(box.Mount{Source: filepath.Join(state, "home"), Target: home}); err != nil {
				return err
			}
			for _, sub := range m.Home.PerBox {
				src := filepath.Join(p.BoxHarnessDir(ref, m.ID), filepath.FromSlash(sub))
				if err := s.AddMount(box.Mount{Source: src, Target: path.Join(home, sub)}); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("harness %s: unknown home strategy %q", m.ID, m.Home.Strategy)
		}
	}
	// Always mounted, even when empty, so that adding a secret later does
	// not change the box definition.
	dir, err := p.BoxSecretsDir(r.BoxID)
	if err != nil {
		return err
	}
	return s.AddMount(box.Mount{Source: dir, Target: paths.InBoxSecret, ReadOnly: true})
}

// Prepare implements box.Preparer: it renders the baselines, writes the
// launch descriptions and seeds of installed harnesses, delivers secrets
// and makes sure the shims exist.
func (h *BoxHooks) Prepare(ctx context.Context, in *box.Inputs, s *box.Spec) error {
	r := in.Resolved
	ref := names.ProjectRef{Realm: r.Realm, Project: r.Project}
	p := in.Paths
	ms, err := h.manifests(r)
	if err != nil {
		return err
	}
	byID := map[string]config.ResolvedHarness{}
	for _, rh := range r.Harnesses {
		byID[rh.ID] = rh
	}

	etc := filepath.Join(p.BoxEtcDir(ref), "harness")
	if err := ensureDir(p, etc); err != nil {
		return err
	}
	if err := cleanBaselines(p, ref, ms); err != nil {
		return err
	}
	keep := map[string]bool{}
	var installed []*config.Harness
	for _, m := range ms {
		if err := h.renderBaseline(p, ref, r, m); err != nil {
			return fmt.Errorf("rendering the baseline of %s: %w", m.ID, err)
		}
		ok, err := h.Toolbox.IsInstalled(m)
		if err != nil {
			return err
		}
		if !ok {
			if m.Install.Method == "none" {
				h.warnf("harness %s has install.method \"none\" but no install.exe; it cannot be started", m.ID)
			} else {
				h.warnf("harness %s is listed for %s but not installed: run 'devc harness install %s'", m.ID, ref, m.ID)
			}
			continue
		}
		installed = append(installed, m)
		if err := h.Toolbox.EnsureShim(m.Bin); err != nil {
			return err
		}
		if err := h.writeLaunch(p, etc, r, m, byID[m.ID]); err != nil {
			return fmt.Errorf("writing the launch description of %s: %w", m.ID, err)
		}
		keep[m.ID+".json"], keep[m.ID] = true, len(m.Home.Seed) > 0
	}
	if err := prune(etc, keep); err != nil {
		return err
	}
	if err := h.deliverSecrets(p, r, installed, byID); err != nil {
		return err
	}
	h.warnTmux(p)
	return nil
}

// Cleanup implements box.Cleaner: the delivered secrets go away when the
// box stops or is removed.
func (h *BoxHooks) Cleanup(_ context.Context, ref names.ProjectRef) error {
	dir, err := h.Config.Paths.BoxSecretsDir(names.BoxID(ref))
	if err != nil {
		return err
	}
	return secrets.Remove(dir)
}

// ensureDir creates dir with mode 0755, so the box's root can read it too.
// Everything below the box's etc and baseline directories is devc's own.
func ensureDir(p *paths.Paths, dir string) error {
	if err := p.Ensure(dir); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// prune removes the entries of dir that keep does not name. Files are
// written before stale ones are pruned, so a running box never sees a
// description or baseline file missing.
func prune(dir string, keep map[string]bool) error {
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, de := range des {
		if keep[de.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, de.Name())); err != nil {
			return err
		}
	}
	return nil
}

// replaceDir moves the freshly written src over dst.
func replaceDir(src, dst string) error {
	old := dst + ".old"
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	if err := os.Rename(dst, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(old)
}

// cleanBaselines removes rendered baselines of harnesses the project no
// longer lists.
func cleanBaselines(p *paths.Paths, ref names.ProjectRef, ms []*config.Harness) error {
	dir := filepath.Dir(p.BoxBaselineDir(ref, "x"))
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, m := range ms {
		keep[m.ID] = true
	}
	for _, de := range des {
		if !keep[de.Name()] {
			if err := os.RemoveAll(filepath.Join(dir, de.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *BoxHooks) renderBaseline(p *paths.Paths, ref names.ProjectRef, r *config.Resolved, m *config.Harness) error {
	if BaselineDir(m) == "" {
		return nil
	}
	dir := p.BoxBaselineDir(ref, m.ID)
	if err := ensureDir(p, dir); err != nil {
		return err
	}
	keep := map[string]bool{}
	instr, err := Instructions(h.Config, r, m)
	if err != nil {
		return err
	}
	if s := m.Baseline.Settings; s != nil {
		b, err := Settings(h.Config, r, m, instr)
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, path.Base(s.Path)), b, 0o644); err != nil {
			return err
		}
		keep[path.Base(s.Path)] = true
	}
	if in := m.Baseline.Instructions; in != nil {
		if err := writeFile(filepath.Join(dir, path.Base(in.Path)), []byte(instr), 0o644); err != nil {
			return err
		}
		keep[path.Base(in.Path)] = true
	}
	return prune(dir, keep)
}

func (h *BoxHooks) writeLaunch(p *paths.Paths, etc string, r *config.Resolved, m *config.Harness, rh config.ResolvedHarness) error {
	l := Describe(r, m, rh)
	if err := l.Validate(); err != nil {
		return err
	}
	if len(m.Home.Seed) > 0 {
		stage := filepath.Join(etc, "."+m.ID+".new")
		if err := os.RemoveAll(stage); err != nil {
			return err
		}
		seedDir := filepath.Join(stage, "seed")
		for _, sd := range m.Home.Seed {
			b, err := RenderSeed(h.Config, r, m, sd)
			if err != nil {
				return err
			}
			target := filepath.Join(seedDir, filepath.FromSlash(sd.Path))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFile(target, b, 0o644); err != nil {
				return err
			}
		}
		if err := chmodTree(stage); err != nil {
			return err
		}
		if err := replaceDir(stage, filepath.Join(etc, m.ID)); err != nil {
			return err
		}
	}
	b, err := marshalIndent(l)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(etc, m.ID+".json"), b, 0o644)
}

// deliverSecrets writes the credentials of installed token harnesses and
// the granted secrets into the box's secrets directory.
func (h *BoxHooks) deliverSecrets(p *paths.Paths, r *config.Resolved, installed []*config.Harness, byID map[string]config.ResolvedHarness) error {
	dir, err := p.BoxSecretsDir(r.BoxID)
	if err != nil {
		return err
	}
	values := map[string][]byte{}
	for _, m := range installed {
		rh := byID[m.ID]
		if (rh.AuthMethod != "env-token" && rh.AuthMethod != "api-key") || rh.Secret == "" {
			continue
		}
		v, err := h.Secrets.Get(r.Realm, rh.Secret)
		switch {
		case err == nil:
			values[rh.Secret] = v
		case errors.Is(err, secrets.ErrNotFound):
			h.warnf("no credential for %s in realm %s: run 'devc login %s %s'", m.ID, r.Realm, r.Realm, m.ID)
		default:
			return fmt.Errorf("reading the credential of %s: %w", m.ID, err)
		}
	}
	for _, name := range r.Grants.Secrets {
		v, err := h.Secrets.Get(r.Realm, name)
		switch {
		case err == nil:
			values[name] = v
		case errors.Is(err, secrets.ErrNotFound):
			h.warnf("the granted secret %s is not set in realm %s: run 'devc secret set %s %s'", name, r.Realm, r.Realm, name)
		default:
			return fmt.Errorf("reading the secret %s: %w", name, err)
		}
	}
	return secrets.Deliver(p, dir, values)
}

// warnTmux says once (per state directory) that tmux is missing.
func (h *BoxHooks) warnTmux(p *paths.Paths) {
	marker := filepath.Join(p.LocalDir(), "warned-tmux-missing")
	if h.Toolbox.InstalledTmux() != "" {
		_ = os.Remove(marker)
		return
	}
	if _, err := os.Stat(marker); err == nil {
		return
	}
	h.warnf("the toolbox has no tmux yet; sessions need it: run 'devc toolbox build tmux' (this warning is shown once)")
	if err := p.Ensure(p.LocalDir()); err == nil {
		_ = os.WriteFile(marker, nil, 0o600)
	}
}

func chmodTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o755)
		}
		return nil
	})
}
