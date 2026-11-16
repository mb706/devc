package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitFrontMatter(t *testing.T) {
	front, body, err := splitFrontMatter("+++\nid = \"x\"\n+++\n## Body\n")
	if err != nil || front != "id = \"x\"\n" || body != "## Body\n" {
		t.Errorf("got %q %q %v", front, body, err)
	}
	front, body, err = splitFrontMatter("+++\n+++\ntext")
	if err != nil || front != "" || body != "text" {
		t.Errorf("empty front matter: %q %q %v", front, body, err)
	}
	if _, _, err := splitFrontMatter("+++\nid = \"x\"\n"); err == nil {
		t.Error("an unclosed front matter was accepted")
	}
	if _, _, err := splitFrontMatter("id = \"x\"\n+++\n"); err == nil {
		t.Error("a file without a leading +++ was accepted")
	}
}

// TestLoadModelStrict checks that unknown keys, duplicate ids, unexpected
// files and mismatched ids are errors that name the file and the place.
func TestLoadModelStrict(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "system.toml"), "[[zone]]\nid = \"box\"\ntitle = \"Box\"\ntrust = \"untrusted\"\ncolour = \"red\"\n")
	writeFile(t, filepath.Join(dir, "packages.toml"), "layers = [\"a\"]\n[layer.a]\ntitle = \"A\"\n[layer.b]\ntitle = \"B\"\n")
	writeFile(t, filepath.Join(dir, "interfaces.toml"), "")
	writeFile(t, filepath.Join(dir, "layout.toml"), "")
	writeFile(t, filepath.Join(dir, "glossary.toml"), "[[term]]\nid = \"relay\"\n[[term]]\nid = \"relay\"\n")
	writeFile(t, filepath.Join(dir, "overview.md"), "Hello.\n")
	writeFile(t, filepath.Join(dir, "notes.txt"), "stray")
	writeFile(t, filepath.Join(dir, "concerns", "net.md"), "+++\nid = \"network\"\ntitle = \"N\"\nbogus = 1\n+++\n## What it is for\n")
	writeFile(t, filepath.Join(dir, "flows", "f.toml"), "id = \"f\"\nkind = \"flow\"\n[[step]]\nat = \"x\"\nref = \"a.B\"\nwhen = \"now\"\n")
	m := loadModel(dir, "model")
	want := []string{
		"model/system.toml: zone.colour: unknown key",
		"model/packages.toml: [layer.b]: layer \"b\" is not in the layers list",
		"model/glossary.toml: [[term]] relay: duplicate id \"relay\"",
		"model/notes.txt: unexpected file in the model",
		"model/concerns/net.md: bogus: unknown key",
		"model/concerns/net.md: id: id \"network\" does not match the file name \"net\"",
		"model/flows/f.toml: step.when: unknown key",
	}
	var got []string
	for _, d := range m.diags {
		if d.Severity != "error" {
			t.Errorf("not an error: %+v", d)
		}
		got = append(got, formatDiag(d))
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in\n%s", w, strings.Join(got, "\n"))
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d diagnostics, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	if c := m.Model.Concerns; len(c) != 1 || c[0].Body != "## What it is for\n" || m.bodyLine[c[0].File] != 6 {
		t.Errorf("concern: %+v, body line %d", c, m.bodyLine["model/concerns/net.md"])
	}
}
