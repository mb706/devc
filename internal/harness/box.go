package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/toolbox"
)

// Describe builds the launch description of an installed harness for a
// project (work order C03 section 3). exe is the executable in the box,
// in the concrete version directory of the current installation
// (toolbox.CurrentExe), so a session keeps the version it started with.
func Describe(r *config.Resolved, m *config.Harness, rh config.ResolvedHarness, exe string) *boxside.Launch {
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
		Exe: exe, Env: env,
		Args: nonNil(m.Launch.Args), Resume: nonNil(m.Launch.Resume),
		Auth:  boxside.LaunchAuth{Method: rh.AuthMethod},
		Home:  boxside.LaunchHome{Dir: home, Strategy: m.Home.Strategy, Shared: nonNil(m.Home.Shared), PerBox: nonNil(m.Home.PerBox)},
		Seeds: []boxside.LaunchSeed{}, Secrets: []boxside.LaunchSecret{},
		Hooks:  m.Hooks.Kind,
		Status: boxside.LaunchStatus{OSC7501: m.Status.OSC7501},
		Input:  boxside.LaunchInput{Adapter: m.Input.Adapter, SubmitKey: m.Input.SubmitKey, QuestionWait: m.Input.Wait()},
		Memory: m.Memory.Kind, Transcripts: nonNil(m.Transcripts.Globs),
	}
	if r.SecretsDelivery == config.DeliveryDaemon {
		l.Delivery = boxside.DeliveryDaemon
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
// or "" when the manifest has no baseline. Validation guarantees that all
// baseline paths share it.
func BaselineDir(m *config.Harness) string {
	if s := m.Baseline.Settings; s != nil {
		return path.Dir(s.Path)
	}
	if in := m.Baseline.Instructions; in != nil {
		return path.Dir(in.Path)
	}
	if len(m.Baseline.Files) > 0 {
		return path.Dir(m.Baseline.Files[0].Path)
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
	// AllowPersistentSecrets permits delivering secrets to a runtime
	// directory that is not memory-backed (DEVC_ALLOW_PERSISTENT_SECRETS=1).
	AllowPersistentSecrets bool
	// LockTimeout bounds the wait for a box lock in RefreshLaunches.
	LockTimeout time.Duration
	// Extra adds secrets devc generates for a box (the SSH gate's key);
	// they are delivered as files in either delivery mode.
	Extra []SecretSource
}

// SecretSource adds per-box secrets that devc itself generates.
type SecretSource interface {
	BoxSecrets(ctx context.Context, r *config.Resolved) (map[string][]byte, error)
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
	// The homes of two harnesses must not nest: one harness's writable
	// state mount would hold the other's mount points.
	for j, m := range ms {
		for _, o := range ms[:j] {
			if m.Home.Dir == o.Home.Dir || paths.Within(o.Home.Dir, m.Home.Dir) || paths.Within(m.Home.Dir, o.Home.Dir) {
				return fmt.Errorf("the harnesses %s and %s use overlapping home directories (%s and %s); a project cannot list both", o.ID, m.ID, o.Home.Dir, m.Home.Dir)
			}
		}
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
		exe, ok, err := h.Toolbox.CurrentExe(m)
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
		if err := h.writeLaunch(etc, r, m, byID[m.ID], exe); err != nil {
			return fmt.Errorf("writing the launch description of %s: %w", m.ID, err)
		}
		keep[m.ID+".json"], keep[m.ID] = true, len(m.Home.Seed) > 0
	}
	if err := prune(etc, keep); err != nil {
		return err
	}
	if err := h.deliverSecrets(ctx, p, r, installed, byID); err != nil {
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
	write := func(inBox string, b []byte) error {
		name := path.Base(inBox)
		keep[name] = true
		return writeFile(filepath.Join(dir, name), b, 0o644)
	}
	instr, err := Instructions(h.Config, r, m)
	if err != nil {
		return err
	}
	if s := m.Baseline.Settings; s != nil {
		b, err := Settings(h.Config, r, m, instr)
		if err != nil {
			return err
		}
		for _, target := range append([]string{s.Path}, s.Also...) {
			if err := write(target, b); err != nil {
				return err
			}
		}
	}
	if in := m.Baseline.Instructions; in != nil {
		if err := write(in.Path, []byte(instr)); err != nil {
			return err
		}
	}
	for _, f := range m.Baseline.Files {
		b, err := RenderBaselineFile(h.Config, r, m, f, instr)
		if err != nil {
			return err
		}
		if err := write(f.Path, b); err != nil {
			return err
		}
	}
	return prune(dir, keep)
}

func (h *BoxHooks) writeLaunch(etc string, r *config.Resolved, m *config.Harness, rh config.ResolvedHarness, exe string) error {
	l := Describe(r, m, rh, exe)
	if err := l.Validate(); err != nil {
		return err
	}
	if len(m.Home.Seed) > 0 {
		stage, err := os.MkdirTemp(etc, "."+m.ID+"-new-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
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
		if err := box.ReplaceDir(stage, filepath.Join(etc, m.ID)); err != nil {
			return err
		}
	}
	return writeDescription(etc, l)
}

func writeDescription(etc string, l *boxside.Launch) error {
	b, err := marshalIndent(l)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(etc, l.ID+".json"), b, 0o644)
}

// RefreshLaunches rewrites the launch description of harness m in every
// box that already has one, under the box's lock, so that the next launch
// in a running box uses the current installation (finding E1). Projects
// with configuration problems are skipped with a warning; their boxes keep
// the version they name, which harness gc keeps.
func (h *BoxHooks) RefreshLaunches(ctx context.Context, m *config.Harness) error {
	exe, ok, err := h.Toolbox.CurrentExe(m)
	if err != nil || !ok {
		return err
	}
	p := h.Config.Paths
	var errs []error
	for _, pr := range h.Config.Projects("") {
		ref := pr.Ref()
		if !slices.Contains(pr.Harnesses, m.ID) {
			continue
		}
		etc := filepath.Join(p.BoxEtcDir(ref), "harness")
		if _, err := os.Lstat(filepath.Join(etc, m.ID+".json")); err != nil {
			continue
		}
		if probs := h.Config.ProblemsFor(ref); len(probs) > 0 {
			h.warnf("%s has configuration problems; its boxes keep the previous %s until the next 'devc up'", ref, m.ID)
			continue
		}
		if err := h.refreshOne(ctx, ref, m, etc, exe); err != nil {
			errs = append(errs, fmt.Errorf("updating the launch description of %s in %s: %w", m.ID, ref, err))
		}
	}
	return errors.Join(errs...)
}

func (h *BoxHooks) refreshOne(ctx context.Context, ref names.ProjectRef, m *config.Harness, etc, exe string) error {
	timeout := h.LockTimeout
	if timeout <= 0 {
		timeout = box.DefaultLockTimeout
	}
	unlock, err := box.LockBox(ctx, h.Config.Paths, ref, timeout, h.Out)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := h.Config.Resolved(ref)
	if err != nil {
		return err
	}
	var rh config.ResolvedHarness
	for _, x := range r.Harnesses {
		if x.ID == m.ID {
			rh = x
		}
	}
	l := Describe(r, m, rh, exe)
	if err := l.Validate(); err != nil {
		return err
	}
	return writeDescription(etc, l)
}

// NamedVersions returns the version stamps of harness id that the launch
// descriptions of all boxes name. The descriptions are devc's own files
// (mounted read-only into boxes); they are still parsed defensively.
func NamedVersions(p *paths.Paths, id string) (map[string]bool, error) {
	out := map[string]bool{}
	root := filepath.Join(p.LocalDir(), "boxes")
	realms, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, rd := range realms {
		if !rd.IsDir() || !names.ValidRealm(rd.Name()) {
			continue
		}
		projects, err := os.ReadDir(filepath.Join(root, rd.Name()))
		if err != nil {
			return nil, err
		}
		for _, pd := range projects {
			if !pd.IsDir() || !names.Valid(pd.Name()) {
				continue
			}
			ref := names.ProjectRef{Realm: rd.Name(), Project: pd.Name()}
			file := filepath.Join(p.BoxEtcDir(ref), "harness", id+".json")
			b, err := readConfigFile(file)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			var l boxside.Launch
			if err := json.Unmarshal(b, &l); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			if s := toolbox.StampOfExe(id, l.Exe); s != "" {
				out[s] = true
			}
		}
	}
	return out, nil
}

// deliverSecrets writes the credentials of installed token harnesses and
// the granted secrets into the box's secrets directory.
func (h *BoxHooks) deliverSecrets(ctx context.Context, p *paths.Paths, r *config.Resolved, installed []*config.Harness, byID map[string]config.ResolvedHarness) error {
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
	if r.SecretsDelivery == config.DeliveryDaemon {
		// The launcher asks the daemon (work order C10a); nothing is
		// written, and what an earlier start delivered goes away.
		values = map[string][]byte{}
	}
	// Generated secrets are files in both modes: the SSH client needs its
	// key as a file (work order C10b).
	for _, src := range h.Extra {
		extra, err := src.BoxSecrets(ctx, r)
		if err != nil {
			return err
		}
		for name, v := range extra {
			values[name] = v
		}
	}
	return secrets.Deliver(p, dir, values, h.AllowPersistentSecrets)
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
