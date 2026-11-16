package main

// This file is the query subcommand: answers about the code from the
// extracted facts, for people writing the model.

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

const queryHelp = `usage: explore query <question> [arguments]

  outline <package|file>   declarations with signatures and first doc sentences
  symbol <ref>             one symbol: signature, position, doc, where it runs
  callers <ref>            who calls or mentions a function (uses, and calls through interfaces)
  calls <ref>              what a function calls or mentions
  uses <ref>               every use of a symbol (a field: where the key takes effect)
  path <from> <to>         a path in the call graph from one function to another
  find <text>              symbols whose reference contains text (case-insensitive)
  help                     this text

References are as in explorer/README.md: internal/box.Up, internal/box.Manager.Up.
Paths are an over-approximation (every mention counts, interfaces of this module
fan out to every implementation); test code is left out unless you start in it.
`

func runQuery(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, queryHelp)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	want := map[string]int{"outline": 1, "symbol": 1, "callers": 1, "calls": 1, "uses": 1, "path": 2, "find": 1}
	n, ok := want[args[0]]
	if !ok || len(args)-1 != n {
		fmt.Fprint(stderr, queryHelp)
		return 2
	}
	res, err := analyze("", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "explore: %v\n", err)
		return 1
	}
	q := &querier{res: res, w: res.w, out: stdout}
	switch args[0] {
	case "outline":
		return q.outline(args[1])
	case "find":
		return q.find(args[1])
	}
	for _, r := range args[1:] {
		if res.w.syms[r] == nil {
			fmt.Fprintf(stderr, "explore: unknown symbol %q%s\n", r, suggest(r, keysOf(res.w.syms)))
			return 1
		}
	}
	switch args[0] {
	case "symbol":
		return q.symbol(args[1])
	case "callers":
		return q.callers(args[1])
	case "calls":
		return q.calls(args[1])
	case "uses":
		return q.uses(args[1])
	case "path":
		return q.path(args[1], args[2])
	}
	return 2
}

type querier struct {
	res *result
	w   *world
	out io.Writer
}

// firstSentence returns the first sentence of a doc comment.
func firstSentence(doc string) string {
	doc = strings.Join(strings.Fields(doc), " ")
	if i := strings.Index(doc, ". "); i >= 0 {
		return doc[:i+1]
	}
	return doc
}

func (q *querier) printSym(s *symInfo, indent string) {
	extra := ""
	if d := firstSentence(s.Doc); d != "" {
		extra = "  // " + clip(d, 140)
	}
	fmt.Fprintf(q.out, "%s%5d  %s%s\n", indent, s.Line, s.Signature, extra)
}

func (q *querier) outline(target string) int {
	var files []string
	if p := q.w.prog.byPath[strings.TrimPrefix(target, "pkg:")]; p != nil {
		for _, f := range p.Files {
			files = append(files, f.Path)
		}
		pk := strings.TrimPrefix(target, "pkg:")
		for _, c := range q.res.core.Packages {
			if c.Path == pk {
				if c.Role != "" {
					fmt.Fprintf(q.out, "%s: %s\n", pk, c.Role)
				}
				if c.Doc != "" {
					fmt.Fprintf(q.out, "%s\n", strings.TrimSpace(c.Doc))
				}
			}
		}
	} else {
		files = []string{strings.TrimPrefix(target, "file:")}
	}
	sort.SliceStable(files, func(i, j int) bool {
		return !strings.HasSuffix(files[i], "_test.go") && strings.HasSuffix(files[j], "_test.go")
	})
	found := false
	for _, file := range files {
		var top []*symInfo
		children := map[string][]*symInfo{}
		for _, s := range q.w.symList {
			if s.File != file {
				continue
			}
			if s.Parent != "" && q.w.syms[s.Parent] != nil && q.w.syms[s.Parent].File == file && s.Kind != "method" {
				children[s.Parent] = append(children[s.Parent], s)
				continue
			}
			top = append(top, s)
		}
		if len(top) == 0 {
			continue
		}
		found = true
		sort.Slice(top, func(i, j int) bool { return top[i].Line < top[j].Line })
		doc := ""
		for _, f := range q.res.core.Files {
			if f.Path == file && f.Doc != "" {
				doc = "  // " + clip(firstSentence(f.Doc), 140)
			}
		}
		fmt.Fprintf(q.out, "\n%s%s\n", file, doc)
		for _, s := range top {
			q.printSym(s, "")
			kids := children[s.Ref]
			sort.Slice(kids, func(i, j int) bool { return kids[i].Line < kids[j].Line })
			for _, k := range kids {
				q.printSym(k, "    ")
			}
		}
	}
	if !found {
		fmt.Fprintf(q.out, "no package or Go file %q\n", target)
		return 1
	}
	return 0
}

func (q *querier) symbol(ref string) int {
	s := q.w.syms[ref]
	fmt.Fprintf(q.out, "%s  (%s, %s:%d-%d)\n%s\n", s.Ref, s.Kind, s.File, s.Line, s.EndLine, s.Signature)
	if s.Value != "" && !strings.Contains(s.Signature, s.Value) {
		fmt.Fprintf(q.out, "value: %s\n", s.Value)
	}
	if s.Doc != "" {
		fmt.Fprintf(q.out, "\n%s\n", strings.TrimSpace(s.Doc))
	}
	if len(s.Actors) > 0 {
		fmt.Fprintf(q.out, "\nruns in (over-approximation): %s\n", strings.Join(s.Actors, ", "))
	}
	fmt.Fprintf(q.out, "used %d times\n", len(q.usesOf(ref)))
	return 0
}

func (q *querier) usesOf(ref string) []Use {
	s := q.w.syms[ref]
	c := q.res.chunks[chunkID(true, s.Package)]
	if c == nil {
		return nil
	}
	return c.UsedBy[ref]
}

func (q *querier) printUses(uses []Use, onlyCalls bool) {
	for _, u := range uses {
		if onlyCalls && u.Kind == "type" {
			continue
		}
		mark := ""
		if u.Test {
			mark = " [test]"
		}
		fmt.Fprintf(q.out, "%s:%d  %s  (%s%s)  %s\n", u.File, u.Line, u.In, u.Kind, mark, u.Text)
	}
}

func (q *querier) callers(ref string) int {
	q.printUses(q.usesOf(ref), true)
	s := q.w.syms[ref]
	if c := q.res.chunks[chunkID(true, s.Package)]; c != nil {
		for _, d := range c.DynamicCallers[ref] {
			fmt.Fprintf(q.out, "%s  (through %s)\n", d.Ref, d.Via)
		}
	}
	return 0
}

func (q *querier) calls(ref string) int {
	var calls []Call
	for to, e := range q.w.edges[ref] {
		calls = append(calls, Call{Ref: to, Line: e.line, Dynamic: e.dynamic, Via: e.via})
	}
	sortCalls(calls)
	for _, c := range calls {
		extra := ""
		if c.Dynamic {
			extra = "  (through " + c.Via + ")"
		}
		fmt.Fprintf(q.out, "%5d  %s%s\n", c.Line, c.Ref, extra)
	}
	return 0
}

func (q *querier) uses(ref string) int {
	q.printUses(q.usesOf(ref), false)
	return 0
}

func (q *querier) path(from, to string) int {
	r, ok := q.w.findPath([]string{from}, to)
	if !ok {
		fmt.Fprintf(q.out, "no path from %s to %s within %d calls\n", from, to, maxPathDepth)
		if r2, ok := q.w.findPath([]string{to}, from); ok {
			fmt.Fprintf(q.out, "but there is one the other way:\n")
			q.printPath(r2)
		}
		return 1
	}
	q.printPath(r)
	return 0
}

func (q *querier) printPath(r pathResult) {
	dyn := map[int]bool{}
	for _, i := range r.dyn {
		dyn[i] = true
	}
	for i, p := range r.path {
		arrow := "  "
		if i > 0 {
			arrow = "→ "
			if dyn[i] {
				arrow = "⇢ "
			}
		}
		loc := ""
		if s := q.w.syms[p]; s != nil {
			loc = fmt.Sprintf("  (%s:%d)", s.File, s.Line)
		}
		fmt.Fprintf(q.out, "%s%s%s\n", arrow, p, loc)
	}
	if len(r.dyn) > 0 {
		fmt.Fprintln(q.out, "(⇢ is a call through an interface)")
	}
}

func (q *querier) find(text string) int {
	text = strings.ToLower(text)
	n := 0
	for _, s := range q.w.symList {
		if strings.Contains(strings.ToLower(s.Ref), text) {
			fmt.Fprintf(q.out, "%-60s %-8s %s:%d\n", s.Ref, s.Kind, s.File, s.Line)
			n++
			if n == 100 {
				fmt.Fprintln(q.out, "… (more)")
				break
			}
		}
	}
	if n == 0 {
		return 1
	}
	return 0
}
