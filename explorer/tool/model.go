package main

// This file loads explorer/model strictly: unknown keys, unexpected files
// and duplicate ids are errors.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// packageEntry is one [package."<path>"] of packages.toml.
type packageEntry struct {
	Layer string `toml:"layer"`
	Group string `toml:"group"`
	Role  string `toml:"role"`
}

type systemFile struct {
	Zone    []Zone    `toml:"zone"`
	Actor   []Actor   `toml:"actor"`
	Channel []Channel `toml:"channel"`
}

type packagesFile struct {
	Layers  []string                `toml:"layers"`
	Layer   map[string]Layer        `toml:"layer"`
	Group   []Group                 `toml:"group"`
	Package map[string]packageEntry `toml:"package"`
}

type interfacesFile struct {
	Surface []Surface `toml:"surface"`
}

type layoutFile struct {
	Tree  []Tree  `toml:"tree"`
	Mount []Mount `toml:"mount"`
}

type glossaryFile struct {
	Term []Term `toml:"term"`
}

// loadedModel is the model with what the checks need.
type loadedModel struct {
	dir      string // the model directory, relative to the root when inside it
	Model    Model
	packages map[string]packageEntry
	// bodyLine is the line of each concern file where its Markdown begins.
	bodyLine map[string]int
	diags    []Diagnostic
}

func (m *loadedModel) errorf(file, place, format string, args ...any) {
	m.diags = append(m.diags, Diagnostic{Severity: "error", File: file, Place: place, Message: fmt.Sprintf(format, args...)})
}

// modelFiles are the files the model directory may hold, besides
// concerns/*.md and flows/*.toml.
var modelFiles = map[string]bool{"system.toml": true, "packages.toml": true, "interfaces.toml": true,
	"layout.toml": true, "glossary.toml": true, "overview.md": true}

// loadModel reads the model directory. display is how file names appear in
// messages.
func loadModel(dir, display string) *loadedModel {
	m := &loadedModel{dir: display, packages: map[string]packageEntry{}, bodyLine: map[string]int{}}
	name := func(rel string) string { return filepath.ToSlash(filepath.Join(display, rel)) }

	entries, err := os.ReadDir(dir)
	if err != nil {
		m.errorf(display, "", "cannot read the model: %v", err)
		return m
	}
	for _, e := range entries {
		switch {
		case e.IsDir() && (e.Name() == "concerns" || e.Name() == "flows"):
		case !e.IsDir() && modelFiles[e.Name()]:
		default:
			m.errorf(name(e.Name()), "", "unexpected file in the model")
		}
	}

	var sys systemFile
	m.decodeFile(dir, name, "system.toml", &sys)
	m.Model.Zones, m.Model.Actors, m.Model.Channels = sys.Zone, sys.Actor, sys.Channel

	var pk packagesFile
	m.decodeFile(dir, name, "packages.toml", &pk)
	for _, id := range pk.Layers {
		l, ok := pk.Layer[id]
		if !ok {
			m.errorf(name("packages.toml"), "layers", "layer %q has no [layer.%s] table", id, id)
		}
		l.ID = id
		m.Model.Layers = append(m.Model.Layers, l)
	}
	for id := range pk.Layer {
		if !contains(pk.Layers, id) {
			m.errorf(name("packages.toml"), "[layer."+id+"]", "layer %q is not in the layers list", id)
		}
	}
	m.Model.Groups = pk.Group
	m.packages = pk.Package
	if m.packages == nil {
		m.packages = map[string]packageEntry{}
	}

	var ifs interfacesFile
	m.decodeFile(dir, name, "interfaces.toml", &ifs)
	m.Model.Surfaces = ifs.Surface

	var lay layoutFile
	m.decodeFile(dir, name, "layout.toml", &lay)
	m.Model.Trees, m.Model.Mounts = lay.Tree, lay.Mount

	var gl glossaryFile
	m.decodeFile(dir, name, "glossary.toml", &gl)
	m.Model.Terms = gl.Term

	if b, err := os.ReadFile(filepath.Join(dir, "overview.md")); err == nil {
		m.Model.Overview = string(b)
	} else {
		m.errorf(name("overview.md"), "", "missing")
	}

	for _, f := range listDir(filepath.Join(dir, "concerns")) {
		rel := "concerns/" + f
		if !strings.HasSuffix(f, ".md") {
			m.errorf(name(rel), "", "unexpected file in the model")
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "concerns", f))
		if err != nil {
			m.errorf(name(rel), "", "%v", err)
			continue
		}
		front, body, err := splitFrontMatter(string(b))
		if err != nil {
			m.errorf(name(rel), "front matter", "%v", err)
			continue
		}
		var c Concern
		m.decode(name(rel), front, &c)
		c.Body, c.File = body, name(rel)
		m.bodyLine[c.File] = strings.Count(front, "\n") + 3
		if want := strings.TrimSuffix(f, ".md"); c.ID != want {
			m.errorf(name(rel), "id", "id %q does not match the file name %q", c.ID, want)
		}
		m.Model.Concerns = append(m.Model.Concerns, c)
	}
	sort.SliceStable(m.Model.Concerns, func(i, j int) bool {
		a, b := m.Model.Concerns[i], m.Model.Concerns[j]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.ID < b.ID
	})

	for _, f := range listDir(filepath.Join(dir, "flows")) {
		rel := "flows/" + f
		if !strings.HasSuffix(f, ".toml") {
			m.errorf(name(rel), "", "unexpected file in the model")
			continue
		}
		var fl Flow
		if !m.decodeFile(dir, name, rel, &fl) {
			continue
		}
		fl.File = name(rel)
		if want := strings.TrimSuffix(f, ".toml"); fl.ID != want {
			m.errorf(name(rel), "id", "id %q does not match the file name %q", fl.ID, want)
		}
		m.Model.Flows = append(m.Model.Flows, fl)
	}
	sort.Slice(m.Model.Flows, func(i, j int) bool { return m.Model.Flows[i].ID < m.Model.Flows[j].ID })
	m.checkIDs()
	return m
}

// listDir returns the names in a directory, sorted; none if it is missing.
func listDir(dir string) []string {
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// decodeFile decodes a TOML file of the model; a missing file is an error.
func (m *loadedModel) decodeFile(dir string, name func(string) string, rel string, v any) bool {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		m.errorf(name(rel), "", "missing")
		return false
	}
	return m.decode(name(rel), string(b), v)
}

// decode decodes TOML strictly.
func (m *loadedModel) decode(file, text string, v any) bool {
	md, err := toml.Decode(text, v)
	if err != nil {
		m.errorf(file, "", "%v", err)
		return false
	}
	for _, k := range md.Undecoded() {
		m.errorf(file, k.String(), "unknown key")
	}
	return true
}

// splitFrontMatter separates TOML front matter between +++ lines from the
// Markdown after it.
func splitFrontMatter(s string) (string, string, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "+++\n") {
		return "", "", fmt.Errorf("the file must start with a line +++")
	}
	rest := s[4:]
	end := -1
	if strings.HasPrefix(rest, "+++\n") || rest == "+++" {
		end = 0
	} else if i := strings.Index(rest, "\n+++\n"); i >= 0 {
		end = i + 1
	} else if strings.HasSuffix(rest, "\n+++") {
		end = len(rest) - 3
	}
	if end < 0 {
		return "", "", fmt.Errorf("no closing +++ line")
	}
	front := rest[:end]
	body := strings.TrimPrefix(rest[end:], "+++")
	body = strings.TrimPrefix(body, "\n")
	return front, body, nil
}

// checkIDs reports duplicate and empty ids.
func (m *loadedModel) checkIDs() {
	type obj struct{ kind, id, file string }
	var objs []obj
	sysFile := filepath.ToSlash(filepath.Join(m.dir, "system.toml"))
	for _, z := range m.Model.Zones {
		objs = append(objs, obj{"zone", z.ID, sysFile})
	}
	for _, a := range m.Model.Actors {
		objs = append(objs, obj{"actor", a.ID, sysFile})
	}
	for _, c := range m.Model.Channels {
		objs = append(objs, obj{"channel", c.ID, sysFile})
	}
	pf := filepath.ToSlash(filepath.Join(m.dir, "packages.toml"))
	for _, g := range m.Model.Groups {
		objs = append(objs, obj{"group", g.ID, pf})
	}
	for _, l := range m.Model.Layers {
		objs = append(objs, obj{"layer", l.ID, pf})
	}
	for _, s := range m.Model.Surfaces {
		objs = append(objs, obj{"surface", s.ID, filepath.ToSlash(filepath.Join(m.dir, "interfaces.toml"))})
	}
	for _, t := range m.Model.Terms {
		objs = append(objs, obj{"term", t.ID, filepath.ToSlash(filepath.Join(m.dir, "glossary.toml"))})
	}
	for _, t := range m.Model.Trees {
		objs = append(objs, obj{"tree", t.ID, filepath.ToSlash(filepath.Join(m.dir, "layout.toml"))})
	}
	for _, c := range m.Model.Concerns {
		objs = append(objs, obj{"concern", c.ID, c.File})
		for _, d := range c.Diagrams {
			objs = append(objs, obj{"diagram", c.ID + "/" + d.ID, c.File})
			for _, n := range d.Nodes {
				objs = append(objs, obj{"node", c.ID + "/" + d.ID + "/" + n.ID, c.File})
			}
		}
	}
	for _, f := range m.Model.Flows {
		objs = append(objs, obj{"flow", f.ID, f.File})
	}
	seen := map[string]bool{}
	for _, o := range objs {
		if o.id == "" || strings.HasSuffix(o.id, "/") || strings.Contains(o.id, "//") {
			m.errorf(o.file, "[["+o.kind+"]]", "a %s without an id", o.kind)
			continue
		}
		k := o.kind + ":" + o.id
		if seen[k] {
			m.errorf(o.file, "[["+o.kind+"]] "+o.id, "duplicate id %q", o.id)
		}
		seen[k] = true
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
