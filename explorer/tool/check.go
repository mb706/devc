package main

// This file checks the model against the code: every reference resolves,
// every package has an entry, layers are respected, files belong to
// concerns, code is commented, and the steps of a flow are connected.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// checker resolves and checks the model.
type checker struct {
	w     *world
	ix    *index
	m     *loadedModel
	cmds  map[string]*commandInfo
	refs  map[string]ResolvedRef
	diags []Diagnostic
	// through maps an http surface to the functions every request of its
	// routes passes before the handler (Surface.Through).
	through map[string][]string
}

func (c *checker) errorf(file, place, format string, args ...any) {
	c.diags = append(c.diags, Diagnostic{Severity: "error", File: file, Place: place, Message: fmt.Sprintf(format, args...)})
}

func (c *checker) warnf(file, place, format string, args ...any) {
	c.diags = append(c.diags, Diagnostic{Severity: "warning", File: file, Place: place, Message: fmt.Sprintf(format, args...)})
}

// ref resolves a reference and records it; an unresolved one is an error.
func (c *checker) ref(file, place, ref, concern string) bool {
	r, err := c.ix.resolve(ref, concern)
	if err != nil {
		c.errorf(file, place, "%v", err)
		return false
	}
	c.refs[ref] = r
	return true
}

// refAs resolves an id of a known kind ("zone", "actor"...).
func (c *checker) refAs(file, place, kind, id string) bool {
	if id == "" {
		c.errorf(file, place, "missing %s", kind)
		return false
	}
	return c.ref(file, place, kind+":"+id, "")
}

func (c *checker) refs2(file, place string, refs []string, concern string) {
	for i, r := range refs {
		c.ref(file, fmt.Sprintf("%s[%d]", place, i), r, concern)
	}
}

// markdown resolves the [[references]] in a text field. firstLine is the
// file line of the text's first line, 0 when unknown.
func (c *checker) markdown(file, place, text, concern string, firstLine int) {
	for _, r := range markdownRefs(text) {
		p := place
		if firstLine > 0 {
			p = fmt.Sprintf("line %d", firstLine+r.line-1)
		} else if strings.Contains(text, "\n") {
			p = fmt.Sprintf("%s, line %d of the text", place, r.line)
		}
		c.ref(file, p, r.ref, concern)
	}
}

func (c *checker) oneOf(file, place, value string, allowed ...string) {
	for _, a := range allowed {
		if value == a {
			return
		}
	}
	c.errorf(file, place, "%q is not one of %s", value, strings.Join(allowed, ", "))
}

// checkModel resolves every reference of the model and checks its shape.
func (c *checker) checkModel() {
	m := &c.m.Model
	dir := c.m.dir
	fn := func(rel string) string { return filepath.ToSlash(filepath.Join(dir, rel)) }

	sys := fn("system.toml")
	for _, z := range m.Zones {
		p := "[[zone]] " + z.ID
		c.oneOf(sys, p+" trust", z.Trust, "untrusted", "host", "aux", "hub", "device", "external")
		c.markdown(sys, p+" summary", z.Summary, "", 0)
		if z.Col < 1 || z.Row < 1 {
			c.errorf(sys, p, "col and row must be at least 1")
		}
	}
	for _, a := range m.Actors {
		p := "[[actor]] " + a.ID
		c.refAs(sys, p+" zone", "zone", a.Zone)
		c.oneOf(sys, p+" kind", a.Kind, "process", "store", "person", "service")
		for i, cmd := range a.Commands {
			c.ref(sys, fmt.Sprintf("%s commands[%d]", p, i), "cmd:"+cmd, "")
		}
		c.refs2(sys, p+" entry", a.Entry, "")
		c.markdown(sys, p+" summary", a.Summary, "", 0)
	}
	for _, ch := range m.Channels {
		p := "[[channel]] " + ch.ID
		c.refAs(sys, p+" from", "actor", ch.From)
		c.refAs(sys, p+" to", "actor", ch.To)
		c.refs2(sys, p+" checks", ch.Checks, "")
		for i, id := range ch.Concerns {
			c.refAs(sys, fmt.Sprintf("%s concerns[%d]", p, i), "concern", id)
		}
		c.markdown(sys, p+" transport", ch.Transport, "", 0)
		c.markdown(sys, p+" carries", ch.Carries, "", 0)
	}

	pf := fn("packages.toml")
	for _, l := range m.Layers {
		c.markdown(pf, "[layer."+l.ID+"] summary", l.Summary, "", 0)
	}
	for _, g := range m.Groups {
		c.markdown(pf, "[[group]] "+g.ID+" summary", g.Summary, "", 0)
	}
	for _, path := range keysOf(c.m.packages) {
		e := c.m.packages[path]
		p := `[package."` + path + `"]`
		c.ref(pf, p, "pkg:"+path, "")
		c.refAs(pf, p+" layer", "layer", e.Layer)
		c.refAs(pf, p+" group", "group", e.Group)
		if strings.TrimSpace(e.Role) == "" {
			c.errorf(pf, p+" role", "missing role")
		}
		c.markdown(pf, p+" role", e.Role, "", 0)
	}
	for _, pk := range c.w.prog.pkgs {
		if pk.XTest {
			continue
		}
		if _, ok := c.m.packages[pk.Path]; !ok {
			c.errorf(pf, `[package."`+pk.Path+`"]`, "package %s has no entry", pk.Path)
		}
	}

	ifile := fn("interfaces.toml")
	for _, s := range m.Surfaces {
		p := "[[surface]] " + s.ID
		c.oneOf(ifile, p+" kind", s.Kind, "rpc", "http", "commands", "events", "config", "env")
		c.markdown(ifile, p+" summary", s.Summary, "", 0)
		c.refs2(ifile, p+" handlers_in", s.HandlersIn, "")
		c.refs2(ifile, p+" listed_in", s.ListedIn, "")
		switch s.Kind {
		case "rpc", "events":
			if c.ref(ifile, p+" consts", s.Consts, "") && !strings.HasPrefix(s.Consts, "pkg:") {
				c.errorf(ifile, p+" consts", "consts must be a package reference (pkg:…)")
			}
			if s.Prefix == "" {
				c.errorf(ifile, p+" prefix", "missing prefix")
			}
		case "http":
			c.ref(ifile, p+" table", s.Table, "")
			c.refs2(ifile, p+" through", s.Through, "")
			for _, role := range []string{"method", "path", "access", "xsite", "handler"} {
				if s.Fields[role] == "" {
					c.errorf(ifile, p+" fields", "missing field name for %q", role)
				}
			}
		case "config":
			c.ref(ifile, p+" root", s.Root, "")
			if s.Key == "" {
				c.errorf(ifile, p+" key", "missing key")
			}
		case "env":
			if s.Pattern != "" {
				if _, err := regexp.Compile(s.Pattern); err != nil {
					c.errorf(ifile, p+" pattern", "%v", err)
				}
			}
		}
	}

	lf := fn("layout.toml")
	for _, t := range m.Trees {
		p := "[[tree]] " + t.ID
		c.markdown(lf, p+" summary", t.Summary, "", 0)
		for i, e := range t.Entries {
			pe := fmt.Sprintf("%s entry[%d] (%s)", p, i, e.Path)
			if e.Ref != "" {
				c.ref(lf, pe+" ref", e.Ref, "")
			}
			if e.Writer != "" {
				c.oneOf(lf, pe+" writer", e.Writer, "host", "box", "both", "user")
			}
			c.markdown(lf, pe+" text", e.Text, "", 0)
		}
	}
	for i, mt := range m.Mounts {
		p := fmt.Sprintf("[[mount]] %d (%s)", i, mt.Target)
		if mt.Ref != "" {
			c.ref(lf, p+" ref", mt.Ref, "")
		}
		c.markdown(lf, p+" text", mt.Text, "", 0)
	}

	gf := fn("glossary.toml")
	for _, t := range m.Terms {
		p := "[[term]] " + t.ID
		c.refs2(gf, p+" refs", t.Refs, "")
		for i, s := range t.See {
			c.refAs(gf, fmt.Sprintf("%s see[%d]", p, i), "term", s)
		}
		c.markdown(gf, p+" text", t.Text, "", 0)
	}

	for _, cn := range m.Concerns {
		f, id := cn.File, cn.ID
		c.markdown(f, "summary", cn.Summary, id, 0)
		for i, p := range cn.Packages {
			c.ref(f, fmt.Sprintf("packages[%d]", i), "pkg:"+p, id)
		}
		for i, p := range cn.Files {
			c.ref(f, fmt.Sprintf("files[%d]", i), "file:"+p, id)
		}
		c.refs2(f, "key", cn.Key, id)
		c.refs2(f, "spec", cn.Spec, id)
		c.refs2(f, "tests", cn.Tests, id)
		for i, r := range cn.Related {
			c.refAs(f, fmt.Sprintf("related[%d]", i), "concern", r)
		}
		for i, inv := range cn.Invariants {
			p := fmt.Sprintf("invariant[%d]", i)
			c.markdown(f, p+" text", inv.Text, id, 0)
			c.refs2(f, p+" refs", inv.Refs, id)
			c.refs2(f, p+" tests", inv.Tests, id)
		}
		for _, d := range cn.Diagrams {
			p := "[[diagram]] " + d.ID
			c.oneOf(f, p+" kind", d.Kind, "graph")
			nodes := map[string]bool{}
			for _, n := range d.Nodes {
				nodes[n.ID] = true
				if n.Ref != "" {
					c.ref(f, p+" node "+n.ID+" ref", n.Ref, id)
				}
				if n.Zone != "" {
					c.refAs(f, p+" node "+n.ID+" zone", "zone", n.Zone)
				}
				if n.Col < 1 || n.Row < 1 {
					c.errorf(f, p+" node "+n.ID, "col and row must be at least 1")
				}
			}
			for i, e := range d.Edges {
				pe := fmt.Sprintf("%s edge[%d]", p, i)
				if !nodes[e.From] {
					c.errorf(f, pe+" from", "no node %q", e.From)
				}
				if !nodes[e.To] {
					c.errorf(f, pe+" to", "no node %q", e.To)
				}
				if e.Ref != "" {
					c.ref(f, pe+" ref", e.Ref, id)
				}
			}
		}
		c.markdown(f, "", cn.Body, id, c.m.bodyLine[f])
		if n := proseWords(cn.Body); n > concernWordLimit {
			c.warnf(f, "", "the page has %d words of prose, more than the guide's %d (code blocks and tables are not counted)", n, concernWordLimit)
		}
	}

	for fi := range m.Flows {
		c.checkFlowShape(&m.Flows[fi])
	}
	c.markdown(fn("overview.md"), "", m.Overview, "", 1)
}

// concernWordLimit is the length of a concern page, in words of prose,
// beyond which --check warns (docs/dev/chunks/X-content-guide.md).
const concernWordLimit = 1200

// proseWords counts the words of a Markdown text that are prose: fenced
// code blocks and tables are left out, a [[reference]] counts as one word
// and [[reference|words]] as its words.
func proseWords(md string) int {
	n := 0
	fence := ""
	for _, l := range strings.Split(md, "\n") {
		t := strings.TrimSpace(l)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}
		if strings.HasPrefix(t, "|") {
			continue
		}
		t = mdRefRE.ReplaceAllStringFunc(t, func(m string) string {
			if sub := mdRefRE.FindStringSubmatch(m); sub[2] != "" {
				return sub[2]
			}
			return "ref"
		})
		for _, w := range strings.Fields(t) {
			if strings.IndexFunc(w, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
				n++
			}
		}
	}
	return n
}

// viaKinds are the kinds of a message step's via.
var viaKinds = map[string]bool{"exec": true, "file": true, "signal": true, "stream": true, "net": true, "reply": true}

// checkFlowShape checks the fields of a flow and its steps.
func (c *checker) checkFlowShape(fl *Flow) {
	f := fl.File
	c.oneOf(f, "kind", fl.Kind, "flow", "tour")
	c.markdown(f, "summary", fl.Summary, "", 0)
	if fl.Concern != "" {
		c.refAs(f, "concern", "concern", fl.Concern)
	}
	lifelines := map[string]bool{}
	for i, a := range fl.Actors {
		c.refAs(f, fmt.Sprintf("actors[%d]", i), "actor", a)
		lifelines[a] = true
	}
	for i := range fl.Steps {
		s := &fl.Steps[i]
		p := stepPlace(fl, i)
		shapes := 0
		if s.At != "" {
			shapes++
		}
		if s.From != "" || s.To != "" {
			shapes++
		}
		if s.Section != "" {
			shapes++
		}
		if shapes != 1 && !(fl.Kind == "tour" && shapes == 0 && s.Ref != "") {
			c.errorf(f, p, "a step has exactly one of at, from and to, or section")
			continue
		}
		if s.Section != "" {
			continue
		}
		for _, a := range []string{s.At, s.From, s.To} {
			if a == "" {
				continue
			}
			if fl.Kind == "flow" && !lifelines[a] {
				c.errorf(f, p, "actor %q is not one of the flow's actors", a)
			} else {
				c.refAs(f, p, "actor", a)
			}
		}
		if (s.From == "") != (s.To == "") {
			c.errorf(f, p, "a message needs both from and to")
		}
		if s.Via != "" {
			if s.From == "" {
				c.errorf(f, p+" via", "only a message (from, to) has a via")
			} else if kind, _, _ := strings.Cut(s.Via, ":"); kind == "rpc" || kind == "http" || kind == "event" || kind == "cmd" {
				c.ref(f, p+" via", s.Via, "")
			} else if !viaKinds[s.Via] {
				c.errorf(f, p+" via", "%q is not one of exec, rpc:<method>, http:<route>, event:<type>, cmd:<command>, reply, file, signal, stream, net", s.Via)
			}
		}
		switch {
		case s.External && s.Ref != "":
			c.errorf(f, p, "an external step has no ref")
		case !s.External && s.Ref == "":
			c.errorf(f, p, "a step needs a ref, or external = true")
		case s.Ref != "":
			c.ref(f, p+" ref", s.Ref, fl.Concern)
		}
		c.refs2(f, p+" checks", s.Checks, fl.Concern)
		c.markdown(f, p+" text", s.Text, fl.Concern, 0)
		c.markdown(f, p+" detail", s.Detail, fl.Concern, 0)
	}
}

// layerIndex maps layer IDs to their position.
func (c *checker) layerIndex() map[string]int {
	idx := map[string]int{}
	for i, l := range c.m.Model.Layers {
		idx[l.ID] = i
	}
	return idx
}

// checkCode runs the checks that compare the code with the model: layers,
// concern coverage, comments.
func (c *checker) checkCode() {
	pf := filepath.ToSlash(filepath.Join(c.m.dir, "packages.toml"))
	idx := c.layerIndex()
	for _, pk := range c.w.prog.pkgs {
		e, ok := c.m.packages[pk.Path]
		if !ok {
			continue
		}
		li, ok := idx[e.Layer]
		if !ok {
			continue
		}
		for _, imp := range pk.Imports {
			ie, ok := c.m.packages[imp]
			if !ok {
				continue
			}
			if lj, ok := idx[ie.Layer]; ok && lj > li {
				c.warnf(pf, `[package."`+pk.Path+`"]`, "imports %s from the higher layer %s", imp, ie.Layer)
			}
		}
	}

	covered := map[string]bool{}
	coveredPkg := map[string]bool{}
	for _, cn := range c.m.Model.Concerns {
		for _, p := range cn.Packages {
			coveredPkg[p] = true
		}
		for _, f := range cn.Files {
			covered[f] = true
		}
	}
	for _, f := range c.w.prog.files {
		if f.Test || f.Ignored || covered[f.Path] || coveredPkg[f.Pkg.Path] {
			continue
		}
		c.warnf(f.Path, "", "belongs to no concern")
	}

	for _, pk := range c.w.prog.pkgs {
		var nonTest []*fileInfo
		hasDoc := false
		for _, f := range pk.Files {
			if !f.Test {
				nonTest = append(nonTest, f)
				if f.AST.Doc != nil {
					hasDoc = true
				}
			}
		}
		if len(nonTest) == 0 {
			continue
		}
		if !hasDoc {
			c.warnf(pk.Path, "", "the package has no package comment")
		}
		if len(nonTest) < 2 {
			continue
		}
		for _, f := range nonTest {
			if f.AST.Doc == nil && fileComment(f) == "" {
				c.warnf(f.Path, "", "no file comment (a comment above the package clause, separated from it by a blank line)")
			}
		}
	}
}

// ----- flows

// codePoint is where an actor's code may continue from.
type codePoint struct {
	step int
	refs []string
	// cmd is the command whose run function this code is: the code that
	// receives a cmd: message, and a step naming the function that holds
	// the command's run literal. A later step naming that function is the
	// same code.
	cmd *commandInfo
}

// pendingMsg is a message that arrived at an actor: its next step must be
// reachable from entries.
type pendingMsg struct {
	step    int
	entries []string
	kind    string
	via     string
	cmd     *commandInfo // for a cmd: message
	broken  string       // why the next step cannot be connected, if it cannot
}

// actorState is what connectFlow keeps for one actor.
type actorState struct {
	// hist holds the code of every earlier step that had code, in order;
	// live the indexes in hist of those whose functions have not returned,
	// innermost last. A step that continues in the code of hist[k] ends
	// the code points after k: they returned to it.
	hist    []codePoint
	live    []int
	pending *pendingMsg
	replied bool
	// last is the index (in Flow.Steps) of the actor's latest step, -1
	// when it has none.
	last int
}

// push records a code point the actor's later steps may continue in.
func (a *actorState) push(cp codePoint) {
	a.hist = append(a.hist, cp)
	a.live = append(a.live, len(a.hist)-1)
}

// resume makes hist[k] the innermost live code point: the code points
// after it returned to it (or, for one that had returned, its function
// runs again, as a goroutine it started does).
func (a *actorState) resume(k int) {
	for j, x := range a.live {
		if x == k {
			a.live = a.live[:j+1]
			return
		}
	}
	var keep []int
	for _, x := range a.live {
		if x < k {
			keep = append(keep, x)
		}
	}
	a.live = append(keep, k)
}

// rank orders the actor's code points for choosing where a step continues:
// the live ones from the innermost out, then the others from the most
// recent back.
func (a *actorState) rank() []int {
	out := make([]int, 0, len(a.hist))
	isLive := map[int]bool{}
	for j := len(a.live) - 1; j >= 0; j-- {
		out = append(out, a.live[j])
		isLive[a.live[j]] = true
	}
	for k := len(a.hist) - 1; k >= 0; k-- {
		if !isLive[k] {
			out = append(out, k)
		}
	}
	return out
}

// pathFrom searches the connection from a code point to a step's code. A
// code point of a command's run literal reaches the function that holds
// the literal in one step: they are the same code.
func (c *checker) pathFrom(cp codePoint, target string) (pathResult, bool, bool) {
	if cp.cmd != nil && cp.cmd.RunLiteral && cp.cmd.Run == target {
		return pathResult{path: []string{target}}, true, true
	}
	r, ok := c.w.findPathDepth(cp.refs, target, flowDepth)
	return r, ok, false
}

// literalNote explains a connection to the function that holds a
// command's run literal.
func literalNote(ci *commandInfo) string {
	return fmt.Sprintf("the run function of %s is a function literal in %s: the same code", "`"+ci.Path+"`", ci.Run)
}

// connectFlow fills in the connections of a flow's steps and reports the
// ones the tool cannot find. Each actor keeps the code of its earlier
// steps. A step connects to the earlier step with the shortest path in the
// call graph to its code; among equally short paths, to the innermost step
// whose function has not returned, else to the most recent. So a step may
// continue in the code of an earlier step after the functions of the steps
// in between returned, or in a goroutine an earlier step started.
func (c *checker) connectFlow(fl *Flow, rpc map[string]*RPCMethod, routes map[string]*Route, events map[string]*EventType) {
	actors := map[string]*actorState{}
	state := func(id string) *actorState {
		a := actors[id]
		if a == nil {
			a = &actorState{last: -1}
			actors[id] = a
		}
		return a
	}
	// connect connects step i of actor a and returns the code point the
	// actor's later steps may continue in.
	connect := func(actor string, i int, s *Step) codePoint {
		a := state(actor)
		self := codePoint{step: i, refs: []string{s.Ref}}
		if s.Unchecked != "" {
			from := -1
			if a.pending != nil {
				from = a.pending.step
			} else if len(a.hist) > 0 {
				from = a.hist[len(a.hist)-1].step
			}
			if from >= 0 {
				s.Connections = append(s.Connections, Connection{Actor: actor, From: from, Status: "unchecked", Note: s.Unchecked})
			}
			a.pending = nil
			return self
		}
		if p := a.pending; p != nil {
			a.pending = nil
			conn := Connection{Actor: actor, From: p.step, Kind: p.kind, Via: p.via}
			switch {
			case p.broken != "":
				conn.Status, conn.Note = "missing", p.broken
				c.warnf(fl.File, stepPlace(fl, i), "%s", p.broken)
			case p.cmd != nil && p.cmd.RunLiteral && p.cmd.Run == s.Ref:
				conn.Status, conn.Path = "found", []string{s.Ref}
				conn.Note = fmt.Sprintf("%s arrives at %s; %s", p.via, s.Ref, literalNote(p.cmd))
				a.push(codePoint{step: p.step, refs: p.entries, cmd: p.cmd})
				self = codePoint{step: i, refs: p.cmd.seeds, cmd: p.cmd}
			default:
				if r, ok := c.w.findPathDepth(p.entries, s.Ref, flowDepth); ok {
					conn.Status, conn.Path, conn.DynamicEdges, conn.ValueEdges = "found", r.path, r.dyn, r.val
					conn.Note = fmt.Sprintf("%s arrives at %s", p.via, r.path[0])
					// The receiving code is where later steps may continue.
					a.push(codePoint{step: p.step, refs: p.entries, cmd: p.cmd})
				} else {
					conn.Status = "missing"
					conn.Note = fmt.Sprintf("no path in the call graph from the code that receives %s (%s) to %s", p.via, strings.Join(clipList(p.entries, 4), ", "), s.Ref)
					c.warnf(fl.File, stepPlace(fl, i), "%s", conn.Note)
				}
			}
			s.Connections = append(s.Connections, conn)
			return self
		}
		if len(a.hist) == 0 {
			return self
		}
		latest := a.hist[len(a.hist)-1]
		best, alias := -1, false
		var r pathResult
		paths := map[int]pathResult{}
		for _, k := range a.rank() {
			p, ok, al := c.pathFrom(a.hist[k], s.Ref)
			if !ok {
				continue
			}
			paths[k] = p
			if best < 0 || len(p.path) < len(r.path) {
				best, r, alias = k, p, al
			}
		}
		// The same path from a more recent step names that step: it is the
		// one whose code the path starts in.
		for k, p := range paths {
			if k > best && !alias && slices.Equal(p.path, r.path) {
				best = k
			}
		}
		if best < 0 {
			conn := Connection{Actor: actor, From: latest.step, Kind: "path", Status: "missing"}
			conn.Note = fmt.Sprintf("no path in the call graph from %s (step %d) or the code of the actor's earlier steps to %s", strings.Join(clipList(latest.refs, 4), ", "), stepNo(fl, latest.step), s.Ref)
			c.warnf(fl.File, stepPlace(fl, i), "%s", conn.Note)
			s.Connections = append(s.Connections, conn)
			return self
		}
		from := a.hist[best]
		conn := Connection{Actor: actor, From: from.step, Kind: "path", Status: "found", Path: r.path, DynamicEdges: r.dyn, ValueEdges: r.val}
		switch {
		case len(r.path) == 1:
			conn.Kind = "same"
		case a.replied:
			conn.Kind = "reply"
		}
		var notes []string
		if alias {
			notes = append(notes, literalNote(from.cmd))
			self = codePoint{step: i, refs: from.cmd.seeds, cmd: from.cmd}
		}
		if best < len(a.hist)-1 {
			notes = append(notes, fmt.Sprintf("continues in the code of step %d, not of the latest step %d (a function returned to its caller, or another goroutine of the same process)", stepNo(fl, from.step), stepNo(fl, latest.step)))
		}
		conn.Note = strings.Join(notes, "; ")
		a.resume(best)
		s.Connections = append(s.Connections, conn)
		return self
	}
	// noCode handles a step of an actor that has no code the tool knows (a
	// file: reference, an external step): it breaks the actor's chain, and
	// an unchecked reason is still shown, from the actor's previous step.
	noCode := func(actor string, i int, s *Step) {
		a := state(actor)
		if s.Unchecked != "" {
			from := a.last
			if a.pending != nil {
				from = a.pending.step
			}
			if from >= 0 {
				s.Connections = append(s.Connections, Connection{Actor: actor, From: from, Status: "unchecked", Note: s.Unchecked})
			}
		}
		a.hist, a.live, a.pending = nil, nil, nil
	}
	for i := range fl.Steps {
		s := &fl.Steps[i]
		if s.Section != "" || fl.Kind == "tour" {
			continue
		}
		hasCode := !s.External && s.Ref != "" && c.w.syms[s.Ref] != nil
		if s.At != "" {
			a := state(s.At)
			if hasCode {
				cp := connect(s.At, i, s)
				a.push(cp)
				a.replied = false
			} else {
				noCode(s.At, i, s)
			}
			a.last = i
			continue
		}
		if s.From == "" {
			continue
		}
		if hasCode {
			a := state(s.From)
			cp := connect(s.From, i, s)
			a.push(cp)
			a.replied = false
		} else if s.Unchecked != "" {
			noCode(s.From, i, s)
		}
		state(s.From).last = i
		to := state(s.To)
		kind, arg, _ := strings.Cut(s.Via, ":")
		switch kind {
		case "rpc":
			m := rpc[arg]
			if m == nil {
				break
			}
			if hasCode && s.Unchecked == "" {
				s.Sender = c.senderCheck(fl, i, s, "rpc", m.Callers)
			}
			var entries []string
			for _, h := range m.Handlers {
				if h.Handler != "" {
					entries = appendUnique(entries, h.Handler)
				}
			}
			p := &pendingMsg{step: i, entries: entries, kind: "rpc", via: s.Via}
			if len(entries) == 0 {
				p.broken = fmt.Sprintf("%s has no handler the tool can see", s.Via)
			}
			to.pending = p
		case "http":
			r := routes[arg]
			if r == nil {
				break
			}
			entries := append(append([]string{}, c.through[r.Surface]...), r.Handler)
			p := &pendingMsg{step: i, entries: entries, kind: "route", via: s.Via}
			if r.Handler == "" {
				p.broken = fmt.Sprintf("%s has no handler the tool can see", s.Via)
			}
			to.pending = p
		case "event":
			e := events[arg]
			if e == nil {
				break
			}
			if hasCode && s.Unchecked == "" {
				s.Sender = c.senderCheck(fl, i, s, "event", e.Writers)
			}
			var entries []string
			for _, r := range e.Readers {
				if r.In != "" {
					entries = appendUnique(entries, r.In)
				}
			}
			p := &pendingMsg{step: i, entries: entries, kind: "event", via: s.Via}
			if len(entries) == 0 {
				p.broken = fmt.Sprintf("%s has no reader the tool can see", s.Via)
			}
			to.pending = p
		case "cmd":
			ci := c.cmds[arg]
			if ci == nil {
				break
			}
			p := &pendingMsg{step: i, entries: ci.seeds, kind: "command", via: s.Via, cmd: ci}
			if len(ci.seeds) == 0 {
				p.broken = fmt.Sprintf("%s has no run function", s.Via)
			}
			to.pending = p
		case "reply":
			to.pending = nil
			to.replied = true
		default:
			to.pending = &pendingMsg{step: i, kind: kind, via: s.Via,
				broken: fmt.Sprintf("the tool cannot follow a message by %q from step %d to the next step of %s; say why with unchecked", s.Via, stepNo(fl, i), s.To)}
		}
	}
	// A broken message whose receiver never acts again is not a problem.
}

// senderCheck links a message step's code to the sites that send its
// method or write its event type.
func (c *checker) senderCheck(fl *Flow, i int, s *Step, kind string, sites []Site) *Connection {
	conn := &Connection{Actor: s.From, From: i, Kind: kind, Via: s.Via}
	var ins []string
	for _, site := range sites {
		if site.In != "" {
			ins = appendUnique(ins, site.In)
		}
	}
	for _, in := range ins {
		if in == s.Ref {
			conn.Status, conn.Path = "found", []string{s.Ref}
			conn.Note = fmt.Sprintf("%s sends %s", s.Ref, s.Via)
			return conn
		}
	}
	if r, ok := c.w.findPathAny([]string{s.Ref}, ins); ok {
		conn.Status, conn.Path, conn.DynamicEdges, conn.ValueEdges = "found", r.path, r.dyn, r.val
		conn.Note = fmt.Sprintf("%s calls %s, which sends %s", s.Ref, r.path[len(r.path)-1], s.Via)
		return conn
	}
	if r, ok := c.w.findPathDepth(ins, s.Ref, flowDepth); ok {
		conn.Status, conn.Path, conn.DynamicEdges, conn.ValueEdges = "found", r.path, r.dyn, r.val
		conn.Note = fmt.Sprintf("%s names %s and calls %s", r.path[0], s.Via, s.Ref)
		return conn
	}
	conn.Status = "missing"
	what := "sends"
	if kind == "event" {
		what = "writes"
	}
	conn.Note = fmt.Sprintf("%s is not connected to any code that %s %s (%s)", s.Ref, what, s.Via, strings.Join(clipList(ins, 4), ", "))
	c.warnf(fl.File, stepPlace(fl, i), "%s", conn.Note)
	return conn
}

// stepNo is the number the app shows for the step at index i of a flow:
// sections are not counted.
func stepNo(fl *Flow, i int) int {
	n := 0
	for k := 0; k <= i && k < len(fl.Steps); k++ {
		if fl.Steps[k].Section == "" {
			n++
		}
	}
	return n
}

// stepPlace names the step at index i in a message: its number in the app,
// and its position among the file's [[step]] tables where that differs.
func stepPlace(fl *Flow, i int) string {
	if fl.Steps[i].Section != "" {
		return fmt.Sprintf("[[step]] %d (a section)", i+1)
	}
	if n := stepNo(fl, i); n != i+1 {
		return fmt.Sprintf("step %d ([[step]] %d)", n, i+1)
	}
	return fmt.Sprintf("step %d", i+1)
}

// findPathAny searches a path from sources to any of targets.
func (w *world) findPathAny(sources, targets []string) (pathResult, bool) {
	var best pathResult
	found := false
	for _, t := range targets {
		if r, ok := w.findPathDepth(sources, t, flowDepth); ok && (!found || len(r.path) < len(best.path)) {
			best, found = r, true
		}
	}
	return best, found
}

func clipList(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(append([]string{}, xs[:n]...), fmt.Sprintf("and %d more", len(xs)-n))
}

// sortDiagnostics puts errors first, then by file and place.
func sortDiagnostics(ds []Diagnostic) {
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Severity != ds[j].Severity {
			return ds[i].Severity == "error"
		}
		if ds[i].File != ds[j].File {
			return ds[i].File < ds[j].File
		}
		return false
	})
}
