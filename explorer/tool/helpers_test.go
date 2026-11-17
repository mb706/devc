package main

import (
	"os"
	"path/filepath"
	"testing"
)

// testModule is the synthetic module under testdata/mod.
const testModule = "example.com/m"

// testSurfaces are the surfaces the synthetic module is read with.
var testSurfaces = []Surface{
	{ID: "x", Kind: "rpc", Consts: "pkg:proto", Prefix: "Method", HandlersIn: []string{"server.Server.handlers"}},
}

// loadTestWorld parses, type-checks and walks testdata/mod without go list:
// it imports nothing outside itself, so no export data is needed.
func loadTestWorld(t *testing.T) *world {
	t.Helper()
	root, err := filepath.Abs("testdata/mod")
	if err != nil {
		t.Fatal(err)
	}
	mod := &struct{ Path string }{testModule}
	listed := []*listedPackage{
		{ImportPath: testModule + "/proto", Dir: root + "/proto", Name: "proto", GoFiles: []string{"proto.go"}, Module: mod},
		{ImportPath: testModule + "/server", Dir: root + "/server", Name: "server", GoFiles: []string{"server.go"},
			Imports: []string{testModule + "/proto"}, Module: mod},
		{ImportPath: testModule + "/client", Dir: root + "/client", Name: "client", GoFiles: []string{"client.go", "flows.go"},
			TestGoFiles: []string{"client_test.go"}, Imports: []string{testModule + "/proto"}, Module: mod},
	}
	prog, err := buildProgram(root, testModule, listed)
	if err != nil {
		t.Fatal(err)
	}
	if len(prog.typeErrors) > 0 {
		t.Fatalf("type errors: %v", prog.typeErrors)
	}
	w := newWorld(prog)
	w.collectSymbols()
	w.computeImplementations()
	w.prepareWatch(testSurfaces)
	w.walkUses()
	return w
}

// testIndex builds an index over the synthetic module and a few invented
// documents, findings, work orders and model objects.
func testIndex(t *testing.T, w *world) *index {
	t.Helper()
	ix := &index{syms: w.syms, pkgs: map[string]bool{}, files: map[string]string{}, cmds: map[string]*commandInfo{},
		rpc: map[string]*RPCMethod{}, routes: map[string]*Route{}, events: map[string]*EventType{},
		config: map[string]*ConfigKey{}, env: map[string]*EnvVar{}, docs: map[string]*docFile{},
		findings: map[string]bool{"C1-05": true}, orders: map[string]bool{"C10a": true},
		model:    map[string]map[string]bool{"concern": {"network": true}, "actor": {"daemon": true}},
		diagrams: map[string][]string{"network/states": nil, "states": {"network"}}}
	for _, p := range w.prog.pkgs {
		ix.pkgs[p.Path] = true
	}
	for _, f := range w.prog.files {
		ix.files[f.Path] = string(f.Src)
	}
	ix.cmds["devc up"] = &commandInfo{Command: Command{Path: "devc up"}}
	for _, m := range w.rpcTable(testSurfaces) {
		m := m
		ix.rpc[m.Name] = &m
	}
	text := "# Plan\n\n## 3. Decisions\n\n### 3.6 Events and journal\n"
	ix.docs["docs/plan.md"] = &docFile{Document: Document{Path: "docs/plan.md", Headings: parseHeadings(text)}, text: text}
	return ix
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
