// Command tool builds the code explorer (explorer/README.md): it extracts
// facts from the module's code, loads the hand-written model in
// explorer/model, checks the model against the code, and writes a static
// site that works when opened from disk.
//
//	go run ./explorer/tool [--check] [--strict] [--only path]... [--out dir] [--model dir]
//	go run ./explorer/tool query help
//	go run ./explorer/tool comments-only
//
// Messages are "file: place: what is wrong", errors before warnings. The
// exit status is 1 when there are errors (with --strict, also warnings),
// 2 for a usage error.
//
// Files: load.go loads and type-checks the module; extract.go collects
// symbols, uses, the call graph and implementations; source.go describes
// files and builds the chunks; commands.go walks the CLI; tables.go reads
// the tables of entry points; reach.go computes where code runs and
// searches paths; docs.go indexes documents, findings and work orders;
// model.go loads the model; refs.go resolves references; check.go checks
// the model against the code; output.go writes the site; query.go and
// comments.go are the query and comments-only subcommands; schema.go is
// the data contract with the app.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options are the command line of a build.
type options struct {
	check, strict bool
	out, model    string
	only          stringList
}

// stringList is a flag that may be given several times.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// shown reports whether a diagnostic is about one of the files or
// directories of --only (all when none is given).
func (o options) shown(d Diagnostic) bool {
	if len(o.only) == 0 {
		return true
	}
	for _, p := range o.only {
		p = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(p)), "/")
		if d.File == p || strings.HasPrefix(d.File, p+"/") {
			return true
		}
	}
	return false
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "query":
			return runQuery(args[1:], stdout, stderr)
		case "comments-only":
			return runCommentsOnly(args[1:], stdout, stderr)
		}
	}
	fs := flag.NewFlagSet("explore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.BoolVar(&o.check, "check", false, "validate the model against the code; write nothing")
	fs.BoolVar(&o.strict, "strict", false, "treat warnings as errors")
	fs.StringVar(&o.out, "out", "", "output directory (default explorer/dist)")
	fs.StringVar(&o.model, "model", "", "model directory (default explorer/model)")
	fs.Var(&o.only, "only", "show only the messages about this file or directory (repeatable); errors elsewhere still fail")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: explore [--check] [--strict] [--only path]... [--out dir] [--model dir]\n       explore query help\n       explore comments-only")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "explore: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	return build(o, stdout, stderr)
}

// result is everything a build computes.
type result struct {
	core   *Core
	w      *world
	chunks map[string]*Chunk
	docs   []*docFile
	cmds   map[string]*commandInfo
	root   string
}

// analyze runs the whole extraction and the checks.
func analyze(modelDir string, stderr io.Writer) (*result, error) {
	start := time.Now()
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root, modPath, err := findRoot(cwd)
	if err != nil {
		return nil, err
	}
	if modelDir == "" {
		modelDir = filepath.Join(root, "explorer", "model")
	}
	modelDir, _ = filepath.Abs(modelDir)
	display := modelDir
	if rel, err := filepath.Rel(root, modelDir); err == nil && !strings.HasPrefix(rel, "..") {
		display = filepath.ToSlash(rel)
	}

	prog, err := loadProgram(root, modPath)
	if err != nil {
		return nil, err
	}
	w := newWorld(prog)
	w.collectSymbols()
	w.computeImplementations()
	lm := loadModel(modelDir, display)
	w.prepareWatch(lm.Model.Surfaces)
	w.walkUses()
	others, err := collectOtherFiles(root)
	if err != nil {
		return nil, err
	}
	files, pkgs, chunks, _ := w.buildFilesAndPackages(others)
	nUses := w.fillChunkGraph(chunks)

	tree, err := buildCommandTree()
	if err != nil {
		return nil, err
	}
	cmdList := w.extractCommands(tree)
	cmds := map[string]*commandInfo{}
	for _, c := range cmdList {
		cmds[c.Path] = c
	}

	surfaces := lm.Model.Surfaces
	rpc := w.rpcTable(surfaces)
	events := w.eventTable(surfaces)
	routes, routeProblems := w.routeTable(surfaces)
	config, configProblems := w.configTable(surfaces)
	env := w.envTable(others)

	docs, err := collectDocs(root)
	if err != nil {
		return nil, err
	}
	findings := indexFindings(docs)
	orders := indexWorkOrders(docs)
	citeInComments(w, findings, orders)

	reach, shares := w.computeReach(lm.Model.Actors, cmds)

	ix := &index{syms: w.syms, pkgs: map[string]bool{}, files: map[string]string{}, cmds: cmds,
		rpc: map[string]*RPCMethod{}, routes: map[string]*Route{}, events: map[string]*EventType{},
		config: map[string]*ConfigKey{}, env: map[string]*EnvVar{}, docs: map[string]*docFile{},
		findings: map[string]bool{}, orders: map[string]bool{}, model: map[string]map[string]bool{},
		diagrams: map[string][]string{}}
	for _, p := range prog.pkgs {
		ix.pkgs[p.Path] = true
	}
	for _, f := range prog.files {
		ix.files[f.Path] = string(f.Src)
	}
	for _, of := range others {
		ix.files[of.Path] = of.text
	}
	for _, d := range docs {
		ix.docs[d.Path] = d
		if _, ok := ix.files[d.Path]; !ok {
			ix.files[d.Path] = d.text
		}
	}
	for i := range rpc {
		ix.rpc[rpc[i].Name] = &rpc[i]
	}
	for i := range routes {
		ix.routes[routes[i].Key] = &routes[i]
	}
	for i := range events {
		ix.events[events[i].Name] = &events[i]
	}
	for i := range config {
		ix.config[config[i].Key] = &config[i]
	}
	for i := range env {
		ix.env[env[i].Name] = &env[i]
	}
	for _, f := range findings {
		ix.findings[f.ID] = true
	}
	for _, o := range orders {
		ix.orders[o.ID] = true
	}
	md := &lm.Model
	add := func(kind, id string) {
		if ix.model[kind] == nil {
			ix.model[kind] = map[string]bool{}
		}
		ix.model[kind][id] = true
	}
	for _, x := range md.Concerns {
		add("concern", x.ID)
		for _, d := range x.Diagrams {
			ix.diagrams[x.ID+"/"+d.ID] = nil
			ix.diagrams[d.ID] = append(ix.diagrams[d.ID], x.ID)
		}
	}
	for _, x := range md.Flows {
		add("flow", x.ID)
	}
	for _, x := range md.Terms {
		add("term", x.ID)
	}
	for _, x := range md.Actors {
		add("actor", x.ID)
	}
	for _, x := range md.Zones {
		add("zone", x.ID)
	}
	for _, x := range md.Channels {
		add("channel", x.ID)
	}
	for _, x := range md.Surfaces {
		add("surface", x.ID)
	}
	for _, x := range md.Layers {
		add("layer", x.ID)
	}
	for _, x := range md.Groups {
		add("group", x.ID)
	}

	c := &checker{w: w, ix: ix, m: lm, cmds: cmds, refs: map[string]ResolvedRef{}, through: map[string][]string{}}
	for _, s := range md.Surfaces {
		if s.Kind == "http" {
			c.through[s.ID] = s.Through
		}
	}
	c.checkModel()
	c.checkCode()
	for i := range md.Flows {
		c.connectFlow(&md.Flows[i], ix.rpc, ix.routes, ix.events)
	}
	ifile := filepath.ToSlash(filepath.Join(display, "interfaces.toml"))
	for _, p := range append(routeProblems, configProblems...) {
		c.errorf(ifile, "", "%s", p)
	}
	for _, te := range prog.typeErrors {
		c.warnf("", "", "type error: %s", te)
	}
	diags := append(lm.diags, c.diags...)
	sortDiagnostics(diags)

	// Assemble the core.
	core := &Core{Files: files, Implementations: w.impls, RPC: rpc, Routes: routes, Events: events, Config: config,
		Env: env, Findings: findings, WorkOrders: orders, Patterns: docPatterns(), Model: *md, Refs: c.refs,
		Diagnostics: diags}
	assigned := commandActors(md.Actors, cmds)
	for _, ci := range cmdList {
		cmd := ci.Command
		if a := assigned[cmd.Path]; a != "" {
			cmd.Actor = a
		}
		core.Commands = append(core.Commands, cmd)
	}
	for _, p := range pkgs {
		if e, ok := lm.packages[p.Path]; ok {
			p.Layer, p.Group, p.Role = e.Layer, e.Group, e.Role
		} else if p.XTest {
			if e, ok := lm.packages[p.ForPackage]; ok {
				p.Layer, p.Group, p.Role = e.Layer, e.Group, e.Role
			}
		}
		p.Actors = shares[p.Path]
		core.Packages = append(core.Packages, p)
	}
	for _, s := range w.symList {
		core.Symbols = append(core.Symbols, s.Symbol)
	}
	for _, d := range docs {
		core.Documents = append(core.Documents, d.Document)
	}
	core.Reach = Reach{Note: reachNote, Actors: reach}
	commit, dirty := gitInfo(root)
	core.Build = Build{Commit: commit, Dirty: dirty, Time: start.UTC().Format(time.RFC3339), GoVersion: runtime.Version(),
		Module: modPath, Root: root, TypeErrors: prog.typeErrors}
	if h := os.Getenv("EXPLORER_HOST_ROOT"); h != "" {
		core.Build.Root = h
	}
	cnt := &core.Build.Counts
	cnt.Packages = len(pkgs)
	for _, f := range prog.files {
		if f.Test {
			cnt.TestFiles++
		} else {
			cnt.GoFiles++
		}
	}
	for _, p := range pkgs {
		addLines(&cnt.Lines, p.Lines)
		addLines(&cnt.TestLines, p.TestLines)
	}
	cnt.Symbols, cnt.Uses, cnt.Commands = len(core.Symbols), nUses, len(core.Commands)
	cnt.RPC, cnt.Routes, cnt.Events, cnt.Config, cnt.Env = len(rpc), len(routes), len(events), len(config), len(env)
	core.Build.Seconds = float64(time.Since(start).Milliseconds()) / 1000
	return &result{core: core, w: w, chunks: chunks, docs: docs, cmds: cmds, root: root}, nil
}

func build(o options, stdout, stderr io.Writer) int {
	start := time.Now()
	res, err := analyze(o.model, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "explore: %v\n", err)
		return 1
	}
	core := res.core
	errs, warns, hiddenErrs, hiddenWarns := 0, 0, 0, 0
	for _, d := range core.Diagnostics {
		if !o.shown(d) {
			if d.Severity == "error" {
				hiddenErrs++
			} else {
				hiddenWarns++
			}
			continue
		}
		if d.Severity == "error" {
			errs++
		} else {
			warns++
		}
		fmt.Fprintln(stdout, formatDiag(d))
	}
	if len(o.only) > 0 {
		fmt.Fprintf(stdout, "explore: --only hides %d errors and %d warnings about other files\n", hiddenErrs, hiddenWarns)
	}
	failed := errs+hiddenErrs > 0 || (o.strict && warns > 0)
	cnt := core.Build.Counts
	fmt.Fprintf(stdout, "explore: %d packages, %d symbols, %d uses, %d commands, %d RPC methods, %d routes, %d event types, %d configuration keys, %d environment variables\n",
		cnt.Packages, cnt.Symbols, cnt.Uses, cnt.Commands, cnt.RPC, cnt.Routes, cnt.Events, cnt.Config, cnt.Env)
	if len(core.Build.TypeErrors) > 0 {
		fmt.Fprintf(stdout, "explore: %d type errors\n", len(core.Build.TypeErrors))
	}
	if o.check || failed {
		fmt.Fprintf(stdout, "explore: %d errors, %d warnings (%.1fs)\n", errs, warns, time.Since(start).Seconds())
		if failed {
			return 1
		}
		return 0
	}
	out := o.out
	if out == "" {
		out = filepath.Join(res.root, "explorer", "dist")
	}
	out, _ = filepath.Abs(out)
	sz, err := writeSite(out, filepath.Join(res.root, "explorer", "app"), core, res.chunks, res.docs)
	if err != nil {
		fmt.Fprintf(stderr, "explore: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "explore: %d errors, %d warnings; wrote %s in %.1fs: core %s, %d chunks %s, %d documents %s, total %s\n",
		errs, warns, displayPath(res.root, out), time.Since(start).Seconds(), human(sz.core), sz.nChunks, human(sz.chunks),
		sz.nDocs, human(sz.docs), human(sz.total))
	fmt.Fprintf(stdout, "explore: open file://%s\n", hostPath(res.root, os.Getenv("EXPLORER_HOST_ROOT"), filepath.Join(out, "index.html")))
	return 0
}

// formatDiag renders a diagnostic as "file: place: message".
func formatDiag(d Diagnostic) string {
	var parts []string
	if d.File != "" {
		parts = append(parts, d.File)
	}
	if d.Place != "" {
		parts = append(parts, d.Place)
	}
	prefix := ""
	if d.Severity == "warning" {
		prefix = "warning: "
	}
	parts = append(parts, prefix+d.Message)
	return strings.Join(parts, ": ")
}

// displayPath shows a path relative to the root when it is below it.
func displayPath(root, p string) string {
	if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}
