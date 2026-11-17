package main

// This file is the query subcommand: answers about the code from the
// extracted facts, for people writing the model.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const queryHelp = `usage: explore query <question> [arguments] [<question> [arguments]]...
       explore query -          (questions from standard input, one per line)

  outline <package|file>       declarations with signatures and first doc sentences
  undocumented <package|file>  declarations without a doc comment (fields and test files left out)
  symbol <ref>                 one symbol: signature, position, doc, where it runs
  callers <ref>                who calls or mentions a function (uses, and calls through interfaces)
  calls <ref>                  what a function calls or mentions
  uses <ref>                   every use of a symbol (a field: where the key takes effect)
  path <from> <to>             a path in the call graph from one function to another
  find <text>                  symbols whose reference contains text (case-insensitive)
  flow <id>                    a flow's steps with the connections the check found, and their paths
  help                         this text

Several questions in one run load the code once: "query outline internal/box
callers internal/box.Up", or one question per line on standard input (blank
lines and lines starting with # are skipped). Each answer then starts with a
line "== question".

References are as in explorer/README.md: internal/box.Up, internal/box.Manager.Up.
Paths are an over-approximation (every mention counts, interfaces of this module
fan out to every implementation); test code is left out unless you start in it.
In paths, → is a call, ⇢ a call through an interface, ~> a mention that is no
call (a function passed as a value or held in a table).
`

// queryArgs is the number of arguments of each question.
var queryArgs = map[string]int{"outline": 1, "undocumented": 1, "symbol": 1, "callers": 1, "calls": 1, "uses": 1,
	"path": 2, "find": 1, "flow": 1}

// parseQuestions splits a command line or the lines of standard input into
// questions, each a word and its arguments.
func parseQuestions(args []string, stdin io.Reader) ([][]string, error) {
	var words [][]string
	if len(args) == 1 && args[0] == "-" {
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			words = append(words, strings.Fields(l))
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	} else {
		words = [][]string{args}
	}
	var out [][]string
	for _, ws := range words {
		for len(ws) > 0 {
			n, ok := queryArgs[ws[0]]
			if !ok {
				return nil, fmt.Errorf("unknown question %q", ws[0])
			}
			if len(ws) < n+1 {
				return nil, fmt.Errorf("%s needs %d argument(s)", ws[0], n)
			}
			out = append(out, ws[:n+1])
			ws = ws[n+1:]
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no question")
	}
	return out, nil
}

func runQuery(args []string, stdout, stderr io.Writer) int {
	return runQueryIn(args, os.Stdin, stdout, stderr)
}

func runQueryIn(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, queryHelp)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	qs, err := parseQuestions(args, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "explore: %v\n\n", err)
		fmt.Fprint(stderr, queryHelp)
		return 2
	}
	res, err := analyze("", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "explore: %v\n", err)
		return 1
	}
	q := &querier{res: res, w: res.w, out: stdout, errOut: stderr}
	status := 0
	for i, qa := range qs {
		if len(qs) > 1 {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			fmt.Fprintf(stdout, "== %s\n", strings.Join(qa, " "))
		}
		if st := q.answer(qa); st > status {
			status = st
		}
	}
	return status
}

// answer answers one question: 0, or 1 when it has no answer.
func (q *querier) answer(qa []string) int {
	switch qa[0] {
	case "outline":
		return q.outline(qa[1])
	case "undocumented":
		return q.undocumented(qa[1])
	case "find":
		return q.find(qa[1])
	case "flow":
		return q.flow(qa[1])
	}
	for _, r := range qa[1:] {
		if q.w.syms[r] == nil {
			fmt.Fprintf(q.errOut, "explore: unknown symbol %q%s\n", r, suggest(r, keysOf(q.w.syms)))
			return 1
		}
	}
	switch qa[0] {
	case "symbol":
		return q.symbol(qa[1])
	case "callers":
		return q.callers(qa[1])
	case "calls":
		return q.calls(qa[1])
	case "uses":
		return q.uses(qa[1])
	case "path":
		return q.path(qa[1], qa[2])
	}
	return 1
}

type querier struct {
	res    *result
	w      *world
	out    io.Writer
	errOut io.Writer
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

// filesOf returns the Go files of a package or the file itself, and
// whether target is a package.
func (q *querier) filesOf(target string) ([]string, bool) {
	if p := q.w.prog.byPath[strings.TrimPrefix(target, "pkg:")]; p != nil {
		var files []string
		for _, f := range p.Files {
			files = append(files, f.Path)
		}
		sort.SliceStable(files, func(i, j int) bool {
			return !strings.HasSuffix(files[i], "_test.go") && strings.HasSuffix(files[j], "_test.go")
		})
		return files, true
	}
	return []string{strings.TrimPrefix(target, "file:")}, false
}

func (q *querier) outline(target string) int {
	files, isPkg := q.filesOf(target)
	if isPkg {
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
	}
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
	for i, p := range r.path {
		arrow := "  "
		if i > 0 {
			arrow = hop(i, r.dyn, r.val) + " "
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
	if len(r.val) > 0 {
		fmt.Fprintln(q.out, "(~> is a mention that is no call: a function passed as a value or held in a table)")
	}
}

// hop is the arrow into path element i: a call, a call through an
// interface, or a mention that is no call.
func hop(i int, dyn, val []int) string {
	in := func(xs []int) bool {
		for _, x := range xs {
			if x == i {
				return true
			}
		}
		return false
	}
	switch {
	case in(val):
		return "~>"
	case in(dyn):
		return "⇢"
	}
	return "→"
}

// undocumented lists the declarations of a package's non-test files, or of
// one file, that have no doc comment: functions, methods, types, variables
// and constants (fields and interface methods are left out).
func (q *querier) undocumented(target string) int {
	files, isPkg := q.filesOf(target)
	known := false
	exported, total := 0, 0
	for _, file := range files {
		if isPkg && strings.HasSuffix(file, "_test.go") {
			continue
		}
		var list []*symInfo
		for _, s := range q.w.symList {
			if s.File != file {
				continue
			}
			known = true
			switch s.Kind {
			case "func", "method", "type", "var", "const":
			default:
				continue
			}
			if strings.TrimSpace(s.Doc) == "" {
				list = append(list, s)
			}
		}
		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Line < list[j].Line })
		fmt.Fprintf(q.out, "\n%s\n", file)
		for _, s := range list {
			mark := "         "
			if s.Exported {
				mark = "exported "
				exported++
			}
			total++
			fmt.Fprintf(q.out, "%5d  %s%s\n", s.Line, mark, s.Signature)
		}
	}
	if !known {
		fmt.Fprintf(q.out, "no package or Go file %q\n", target)
		return 1
	}
	fmt.Fprintf(q.out, "\n%d declarations without a doc comment, %d of them exported\n", total, exported)
	return 0
}

// flow prints a flow's steps with the connections the check found.
func (q *querier) flow(id string) int {
	var fl *Flow
	var ids []string
	for i := range q.res.core.Model.Flows {
		f := &q.res.core.Model.Flows[i]
		ids = append(ids, f.ID)
		if f.ID == id {
			fl = f
		}
	}
	if fl == nil {
		fmt.Fprintf(q.errOut, "explore: no flow %q%s\n", id, suggest(id, ids))
		return 1
	}
	no := map[int]int{} // index in Steps -> step number
	n := 0
	for i, s := range fl.Steps {
		if s.Section == "" {
			n++
			no[i] = n
		}
	}
	fmt.Fprintf(q.out, "%s: %s (%s, %d steps)\n", fl.ID, fl.Title, fl.Kind, n)
	for i, s := range fl.Steps {
		if s.Section != "" {
			fmt.Fprintf(q.out, "\n  -- %s\n", s.Section)
			continue
		}
		who := s.At
		if s.From != "" {
			who = s.From + " → " + s.To
		}
		what := s.Ref
		if s.Via != "" {
			what = strings.TrimSpace(s.Via + "  " + s.Ref)
		}
		if s.External {
			what = strings.TrimSpace(what + "  (external)")
		}
		fmt.Fprintf(q.out, "%3d  %s  %s\n", no[i], who, what)
		for _, c := range s.Connections {
			q.printConn(c, no, "from")
		}
		if s.Sender != nil {
			q.printConn(*s.Sender, no, "sends")
		}
	}
	return 0
}

// printConn prints one connection of a step under it.
func (q *querier) printConn(c Connection, no map[int]int, what string) {
	head := fmt.Sprintf("       %s step %d", what, no[c.From])
	if what == "sends" {
		head = "       sends"
	}
	if c.Actor != "" && what == "from" {
		head += " (" + c.Actor + ")"
	}
	switch c.Status {
	case "found":
		kind := c.Kind
		if c.Via != "" {
			kind += " " + c.Via
		}
		var hops []string
		for i, p := range c.Path {
			if i > 0 {
				hops = append(hops, hop(i, c.DynamicEdges, c.ValueEdges))
			}
			hops = append(hops, p)
		}
		fmt.Fprintf(q.out, "%s: %s: %s\n", head, kind, strings.Join(hops, " "))
	case "unchecked":
		fmt.Fprintf(q.out, "%s: not checked: %s\n", head, c.Note)
		return
	default:
		fmt.Fprintf(q.out, "%s: MISSING\n", head)
	}
	if c.Note != "" {
		fmt.Fprintf(q.out, "         %s\n", c.Note)
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
