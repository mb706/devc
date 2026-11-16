// Package config loads, validates and answers questions about the devc
// configuration: config.toml, realms, projects, envs and harness manifests.
//
// Security rules: policy comes only from the config directory (README rule
// 1); nothing here reads from a project directory, a box home or any other
// box-writable place. Every name read from a directory name or a file is
// checked with internal/names before it is used (rule 4). Values that later
// become command arguments are validated so they cannot be mistaken for
// options (rule 3).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/mb706/devc/assets"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// Problem is one configuration error.
type Problem struct {
	File    string `json:"file"`
	Key     string `json:"key,omitempty"`
	Message string `json:"message"`
}

func (p Problem) String() string {
	if p.Key == "" {
		return p.File + ": " + p.Message
	}
	return p.File + ": " + p.Key + ": " + p.Message
}

// BuiltinPrefix marks problems in manifests embedded in the binary.
const BuiltinPrefix = "builtin:"

type problems struct {
	list []Problem
}

func (ps *problems) add(file, key, format string, args ...any) {
	ps.list = append(ps.list, Problem{File: file, Key: key, Message: fmt.Sprintf(format, args...)})
}

// Config is the loaded configuration. Entities that failed to parse are
// absent; entities that parsed but failed validation are present, and their
// problems are listed in Problems.
type Config struct {
	// Paths has config.toml's state_dir and projects_root applied.
	Paths    *paths.Paths
	Global   Global
	Problems []Problem

	realms   map[string]*Realm
	projects map[names.ProjectRef]*Project
	// services are the services of every realm by "<realm>/<name>" (work
	// order C18).
	services  map[string]*Service
	envs      map[string]*Env
	harnesses map[string]*Harness
	// overrides maps a harness id to the file in <config>/harness that
	// defines it, also when that file failed to load: an existing user
	// manifest is authoritative, and a broken one never falls back to the
	// built-in manifest of the same id.
	overrides map[string]string
	// harnessDirFailed is set when <config>/harness exists but cannot be
	// listed, so no harness can be trusted to be the intended one.
	harnessDirFailed bool
}

// maxConfigFile bounds every configuration file; anything larger is a mistake.
const maxConfigFile = 1 << 20

var typeErrRE = regexp.MustCompile(`^toml: (line \d+ )?\(last key "((?:[^"\\]|\\.)*)"\): (.*)$`)

// Load reads the global configuration, all realms, projects, envs and
// harness manifests, and returns every problem found rather than stopping at
// the first.
func Load(p *paths.Paths) (*Config, []Problem) {
	var ps problems
	c := &Config{
		Paths:     p,
		realms:    map[string]*Realm{},
		projects:  map[names.ProjectRef]*Project{},
		services:  map[string]*Service{},
		envs:      map[string]*Env{},
		harnesses: map[string]*Harness{},
		overrides: map[string]string{},
	}

	c.Global = defaultGlobal()
	if data, ok := readOptional(p.ConfigFile(), &ps); ok {
		decode(p.ConfigFile(), data, &c.Global, &ps)
	}
	stateDir, projectsRoot := c.Global.StateDir, c.Global.ProjectsRoot
	if _, err := paths.ExpandHome(stateDir, p.Home()); stateDir != "" && err != nil {
		ps.add(p.ConfigFile(), "state_dir", "%v", err)
		stateDir = ""
	}
	if _, err := paths.ExpandHome(projectsRoot, p.Home()); err != nil {
		ps.add(p.ConfigFile(), "projects_root", "%v", err)
		projectsRoot = ""
	}
	if q, err := p.WithGlobal(stateDir, projectsRoot); err == nil {
		c.Paths = q
	}

	c.loadHarnesses(&ps)
	c.loadEnvs(&ps)
	c.loadRealms(&ps)

	validateGlobal(c, &ps)
	bins := map[string]string{}
	for _, h := range c.Harnesses() {
		validateHarness(h, &ps)
		if other, dup := bins[h.Bin]; dup && h.Bin != "" {
			ps.add(h.File, "bin", "%q is also the bin of harness %q; shim names must be unique", h.Bin, other)
		}
		bins[h.Bin] = h.ID
	}
	for _, e := range c.Envs() {
		validateEnv(e, &ps)
	}
	for _, r := range c.Realms() {
		validateRealm(c, r, &ps)
		validateServices(c, r.Name, &ps)
	}
	for _, pr := range c.Projects("") {
		validateProject(c, pr, &ps)
	}

	c.Problems = ps.list
	return c, ps.list
}

// readOptional returns the file's content, or ok=false if it does not exist
// (no problem recorded) or cannot be read (problem recorded).
func readOptional(file string, ps *problems) ([]byte, bool) {
	fi, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	if err == nil && !fi.Mode().IsRegular() {
		ps.add(file, "", "not a regular file")
		return nil, false
	}
	if err == nil && fi.Size() > maxConfigFile {
		ps.add(file, "", "file is larger than %d bytes", maxConfigFile)
		return nil, false
	}
	var data []byte
	if err == nil {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		ps.add(file, "", "cannot read: %v", err)
		return nil, false
	}
	return data, true
}

// decode decodes TOML strictly into v. It reports syntax and type errors
// with the offending key, and every key that does not belong to the schema.
// It returns false only when v could not be decoded; unknown keys are
// problems but leave v usable, so the rest of the entity is still checked.
func decode(file string, data []byte, v any, ps *problems) bool {
	md, err := toml.Decode(string(data), v)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			ps.add(file, pe.LastKey, "line %d: %s", pe.Position.Line, pe.Message)
		} else if m := typeErrRE.FindStringSubmatch(err.Error()); m != nil {
			// Type mismatches are plain errors: `toml: line N (last key "k"): …`.
			key, uerr := strconv.Unquote(`"` + m[2] + `"`)
			if uerr != nil {
				key = m[2]
			}
			msg := m[3]
			if m[1] != "" {
				msg = strings.TrimSpace(m[1]) + ": " + msg
			}
			ps.add(file, key, "%s", msg)
		} else {
			ps.add(file, "", "%v", err)
		}
		return false
	}
	var reported []string
	for _, k := range md.Undecoded() {
		ks := k.String()
		parent := false
		for _, r := range reported {
			if strings.HasPrefix(ks, r+".") {
				parent = true
				break
			}
		}
		if parent {
			continue
		}
		reported = append(reported, ks)
		ps.add(file, ks, "unknown key")
	}
	return true
}

// subdirs lists the directories in dir, sorted. Hidden entries (a realm may
// be its own git repository) and plain files (README and the like) are
// skipped. A missing dir yields nothing.
func subdirs(dir string, ps *problems) []string {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		ps.add(dir, "", "cannot list: %v", err)
		return nil
	}
	var out []string
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".") {
			continue
		}
		isDir := de.IsDir()
		if de.Type()&fs.ModeSymlink != 0 {
			// The config directory is trusted; a symlinked realm or env
			// directory is allowed.
			if fi, err := os.Stat(filepath.Join(dir, de.Name())); err == nil && fi.IsDir() {
				isDir = true
			}
		}
		if isDir {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out
}

func (c *Config) loadHarnesses(ps *problems) {
	builtin, _ := fs.Glob(assets.Harness, "harness/*.toml")
	for _, name := range builtin {
		data, err := assets.Harness.ReadFile(name)
		file := BuiltinPrefix + name
		if err != nil {
			ps.add(file, "", "cannot read embedded manifest: %v", err)
			continue
		}
		h := defaultHarness()
		if !decode(file, data, &h, ps) {
			continue
		}
		h.Source, h.File = "builtin", file
		if want := strings.TrimSuffix(path.Base(name), ".toml"); h.ID != want {
			ps.add(file, "id", "id %q does not match the file name %q", h.ID, want)
			continue
		}
		c.harnesses[h.ID] = &h
	}

	dir := c.Paths.UserHarnessDir()
	des, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		ps.add(dir, "", "cannot list: %v", err)
		c.harnessDirFailed = true
	}
	for _, de := range des {
		if !strings.HasSuffix(de.Name(), ".toml") || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		file := filepath.Join(dir, de.Name())
		id := strings.TrimSuffix(de.Name(), ".toml")
		if err := names.Check(names.KindHarness, id); err != nil {
			ps.add(file, "", "file name: %v", err)
			continue
		}
		// From here on the file is the manifest of id, whether or not it
		// loads: the built-in one is gone either way.
		c.overrides[id] = file
		delete(c.harnesses, id)
		if de.IsDir() {
			ps.add(file, "", "not a regular file")
			continue
		}
		data, ok := readOptional(file, ps)
		if !ok {
			if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
				ps.add(file, "", "cannot read: the file vanished or is a dangling symlink")
			}
			continue
		}
		h := defaultHarness()
		if !decode(file, data, &h, ps) {
			continue
		}
		if h.ID != id {
			ps.add(file, "id", "id %q does not match the file name; it must be %q", h.ID, id)
			continue
		}
		h.Source, h.File = file, file
		c.harnesses[id] = &h
	}
}

func (c *Config) loadEnvs(ps *problems) {
	envsDir := c.Paths.EnvsDir()
	for _, name := range subdirs(envsDir, ps) {
		dir := filepath.Join(envsDir, name)
		if err := names.Check(names.KindEnv, name); err != nil {
			ps.add(dir, "", "%v", err)
			continue
		}
		e := &Env{Name: name, Dir: dir, File: c.Paths.EnvFile(name)}
		cf := c.Paths.EnvContainerfile(name)
		if fi, err := os.Stat(cf); err == nil {
			if !fi.Mode().IsRegular() {
				ps.add(cf, "", "not a regular file")
			} else {
				e.HasContainerfile = true
			}
		}
		data, ok := readOptional(e.File, ps)
		if ok && !decode(e.File, data, e, ps) {
			continue
		}
		c.envs[name] = e
	}
}

func (c *Config) loadRealms(ps *problems) {
	realmsDir := c.Paths.RealmsDir()
	for _, name := range subdirs(realmsDir, ps) {
		if err := names.Check(names.KindRealm, name); err != nil {
			ps.add(filepath.Join(realmsDir, name), "", "%v", err)
			continue
		}
		r := defaultRealm()
		r.Name, r.File = name, c.Paths.RealmFile(name)
		data, ok := readOptional(r.File, ps)
		if !ok {
			if _, err := os.Stat(r.File); errors.Is(err, fs.ErrNotExist) {
				ps.add(r.File, "", "missing: every realm directory needs a realm.toml (it may be empty)")
			}
			continue
		}
		if !decode(r.File, data, &r, ps) {
			continue
		}
		c.realms[name] = &r
		c.loadProjects(name, ps)
		c.loadServices(name, ps)
	}
}

func (c *Config) loadProjects(realm string, ps *problems) {
	dir := c.Paths.ProjectDefsDir(realm)
	for _, name := range subdirs(dir, ps) {
		if err := names.Check(names.KindProject, name); err != nil {
			ps.add(filepath.Join(dir, name), "", "%v", err)
			continue
		}
		ref := names.ProjectRef{Realm: realm, Project: name}
		pr := defaultProject()
		pr.Name, pr.Realm, pr.File = name, realm, c.Paths.ProjectFile(ref)
		data, ok := readOptional(pr.File, ps)
		if !ok {
			if _, err := os.Stat(pr.File); errors.Is(err, fs.ErrNotExist) {
				ps.add(pr.File, "", "missing: every project directory needs a project.toml")
			}
			continue
		}
		if !decode(pr.File, data, &pr, ps) {
			continue
		}
		for i := range pr.Mounts {
			if pr.Mounts[i].Mode == "" {
				pr.Mounts[i].Mode = "ro"
			}
		}
		c.projects[ref] = &pr
	}
}
