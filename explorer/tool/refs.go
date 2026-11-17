package main

// This file parses and resolves references (explorer/README.md,
// "References") and suggests the nearest names for one that does not
// resolve.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// refKinds are the prefixes of references; a reference without one is a
// symbol.
var refKinds = map[string]bool{"pkg": true, "file": true, "cmd": true, "rpc": true, "http": true, "event": true,
	"config": true, "env": true, "concern": true, "flow": true, "term": true, "actor": true, "zone": true,
	"channel": true, "doc": true, "finding": true, "chunk": true, "diagram": true, "surface": true, "layer": true,
	"group": true}

// modelKinds are the reference kinds that name model objects.
var modelKinds = []string{"concern", "flow", "term", "actor", "zone", "channel", "surface", "layer", "group"}

// splitRef returns a reference's kind ("symbol" without a prefix) and the
// rest. ok is false for an unknown prefix.
func splitRef(ref string) (kind, rest string, ok bool) {
	k, r, found := strings.Cut(ref, ":")
	if !found {
		return "symbol", ref, true
	}
	if !refKinds[k] {
		return k, r, false
	}
	return k, r, true
}

// splitSymbol splits a symbol reference into its package and the dotted
// rest ("internal/box", "Manager.Up").
func splitSymbol(ref string) (string, string) {
	slash := strings.LastIndex(ref, "/")
	dot := strings.Index(ref[slash+1:], ".")
	if dot < 0 {
		return ref, ""
	}
	return ref[:slash+1+dot], ref[slash+1+dot+1:]
}

// index is everything references can point at.
type index struct {
	syms     map[string]*symInfo
	pkgs     map[string]bool
	files    map[string]string // path -> text ("" for binary files)
	cmds     map[string]*commandInfo
	rpc      map[string]*RPCMethod
	routes   map[string]*Route
	events   map[string]*EventType
	config   map[string]*ConfigKey
	env      map[string]*EnvVar
	docs     map[string]*docFile
	findings map[string]bool
	orders   map[string]bool
	model    map[string]map[string]bool // kind -> ids
	// diagrams maps "<concern>/<id>" to true and a bare id to the concerns
	// that have it.
	diagrams map[string][]string
}

// resolve resolves one reference. concern is the concern whose text holds
// the reference, for bare diagram ids.
func (ix *index) resolve(ref, concern string) (ResolvedRef, error) {
	kind, rest, ok := splitRef(ref)
	if !ok {
		return ResolvedRef{}, fmt.Errorf("unknown kind of reference %q (known: %s)", kind+":", strings.Join(sortedKeys(refKinds), ", "))
	}
	if rest == "" {
		return ResolvedRef{}, fmt.Errorf("empty reference %q", ref)
	}
	switch kind {
	case "symbol":
		if s := ix.syms[ref]; s != nil {
			return ResolvedRef{Kind: "symbol", Key: ref, File: s.File, Line: s.Line}, nil
		}
		pkg, member := splitSymbol(ref)
		if !ix.pkgs[pkg] {
			return ResolvedRef{}, fmt.Errorf("unknown package %q in %q%s", pkg, ref, suggest(pkg, keysOf(ix.pkgs)))
		}
		if member == "" {
			return ResolvedRef{}, fmt.Errorf("%q names a package; write pkg:%s", ref, ref)
		}
		var cands []string
		for r, s := range ix.syms {
			if s.Package == pkg {
				cands = append(cands, r)
			}
		}
		return ResolvedRef{}, fmt.Errorf("unknown symbol %q%s", ref, suggest(ref, cands))
	case "pkg":
		if ix.pkgs[rest] {
			return ResolvedRef{Kind: "package", Key: rest}, nil
		}
		return ResolvedRef{}, fmt.Errorf("unknown package %q%s", rest, suggest(rest, keysOf(ix.pkgs)))
	case "file":
		p, text, hasText := strings.Cut(rest, "~")
		content, ok := ix.files[p]
		if !ok {
			return ResolvedRef{}, fmt.Errorf("unknown file %q%s", p, suggest(p, keysOf(ix.files)))
		}
		r := ResolvedRef{Kind: "file", Key: p, File: p}
		if hasText {
			for i, l := range strings.Split(content, "\n") {
				if strings.Contains(l, text) {
					r.Line = i + 1
					return r, nil
				}
			}
			return ResolvedRef{}, fmt.Errorf("no line of %s contains %q", p, text)
		}
		return r, nil
	case "cmd":
		if c := ix.cmds[rest]; c != nil {
			return ResolvedRef{Kind: "command", Key: rest, File: c.RunFile, Line: c.RunLine}, nil
		}
		return ResolvedRef{}, fmt.Errorf("unknown command %q%s", rest, suggest(rest, keysOf(ix.cmds)))
	case "rpc":
		if m := ix.rpc[rest]; m != nil {
			s := ix.syms[m.Const]
			return ResolvedRef{Kind: "rpc", Key: rest, File: s.File, Line: s.Line}, nil
		}
		return ResolvedRef{}, fmt.Errorf("unknown RPC method %q%s", rest, suggest(rest, keysOf(ix.rpc)))
	case "http":
		if r := ix.routes[rest]; r != nil {
			return ResolvedRef{Kind: "route", Key: rest, File: r.File, Line: r.Line}, nil
		}
		return ResolvedRef{}, fmt.Errorf("unknown route %q%s", rest, suggest(rest, keysOf(ix.routes)))
	case "event":
		if e := ix.events[rest]; e != nil {
			s := ix.syms[e.Const]
			return ResolvedRef{Kind: "event", Key: rest, File: s.File, Line: s.Line}, nil
		}
		return ResolvedRef{}, fmt.Errorf("unknown event type %q%s", rest, suggest(rest, keysOf(ix.events)))
	case "config":
		if k := ix.config[rest]; k != nil {
			r := ResolvedRef{Kind: "config", Key: rest}
			if s := ix.syms[k.Field]; s != nil {
				r.File, r.Line = s.File, s.Line
			}
			return r, nil
		}
		return ResolvedRef{}, fmt.Errorf("unknown configuration key %q%s", rest, suggest(rest, keysOf(ix.config)))
	case "env":
		if ix.env[rest] != nil {
			return ResolvedRef{Kind: "env", Key: rest}, nil
		}
		return ResolvedRef{}, fmt.Errorf("unknown environment variable %q (not named in the code)%s", rest, suggest(rest, keysOf(ix.env)))
	case "doc":
		p, anchor, hasAnchor := strings.Cut(rest, "#")
		d := ix.docs[p]
		if d == nil {
			return ResolvedRef{}, fmt.Errorf("unknown document %q%s", p, suggest(p, keysOf(ix.docs)))
		}
		r := ResolvedRef{Kind: "doc", Key: p}
		if !hasAnchor {
			return r, nil
		}
		var names []string
		var prefixed []Heading
		for _, h := range d.Headings {
			if h.Number == anchor || h.Anchor == anchor {
				r.Anchor, r.Line = h.Anchor, h.Line
				return r, nil
			}
			if anchor != "" && strings.HasPrefix(h.Anchor, anchor+"-") {
				prefixed = append(prefixed, h)
			}
			names = append(names, h.Anchor)
			if h.Number != "" {
				names = append(names, h.Number)
			}
		}
		// The leading words of one heading's anchor name it too
		// ("#x7" for "X7 Explorer content: git").
		if len(prefixed) == 1 {
			r.Anchor, r.Line = prefixed[0].Anchor, prefixed[0].Line
			return r, nil
		}
		if len(prefixed) > 1 {
			return ResolvedRef{}, fmt.Errorf("document %s has several headings starting with %q (%s)", p, anchor, strings.Join(clipList(headingAnchors(prefixed), 4), ", "))
		}
		return ResolvedRef{}, fmt.Errorf("document %s has no heading %q%s", p, anchor, suggest(anchor, names))
	case "finding":
		if ix.findings[rest] {
			return ResolvedRef{Kind: "finding", Key: rest}, nil
		}
		return ResolvedRef{}, fmt.Errorf("unknown finding %q%s", rest, suggest(rest, keysOf(ix.findings)))
	case "chunk":
		if ix.orders[rest] {
			return ResolvedRef{Kind: "chunk", Key: rest}, nil
		}
		return ResolvedRef{}, fmt.Errorf("unknown work order %q%s", rest, suggest(rest, keysOf(ix.orders)))
	case "diagram":
		if strings.Contains(rest, "/") {
			if _, ok := ix.diagrams[rest]; ok {
				return ResolvedRef{Kind: "diagram", Key: rest}, nil
			}
		} else {
			owners := ix.diagrams[rest]
			for _, c := range owners {
				if c == concern {
					return ResolvedRef{Kind: "diagram", Key: c + "/" + rest}, nil
				}
			}
			if len(owners) == 1 {
				return ResolvedRef{Kind: "diagram", Key: owners[0] + "/" + rest}, nil
			}
			if len(owners) > 1 {
				return ResolvedRef{}, fmt.Errorf("diagram %q is ambiguous; write diagram:<concern>/%s", rest, rest)
			}
		}
		var all []string
		for k := range ix.diagrams {
			if strings.Contains(k, "/") {
				all = append(all, k)
			}
		}
		return ResolvedRef{}, fmt.Errorf("unknown diagram %q%s", rest, suggest(rest, all))
	}
	if ids := ix.model[kind]; ids[rest] {
		return ResolvedRef{Kind: kind, Key: rest}, nil
	}
	return ResolvedRef{}, fmt.Errorf("unknown %s %q%s", kind, rest, suggest(rest, keysOf(ix.model[kind])))
}

// suggest names up to three candidates near s, as "; did you mean …?".
func suggest(s string, cands []string) string {
	type scored struct {
		c string
		d int
	}
	var best []scored
	limit := len([]rune(s))/2 + 1
	if limit < 3 {
		limit = 3
	}
	last := s[strings.LastIndexAny(s, "./ ")+1:]
	for _, c := range cands {
		d := levenshtein(s, c)
		// The same last name elsewhere is a good guess too.
		if d > limit && last != "" && strings.HasSuffix(c, "."+last) {
			d = limit
		}
		if d <= limit {
			best = append(best, scored{c, d})
		}
	}
	if len(best) == 0 {
		return ""
	}
	sort.Slice(best, func(i, j int) bool {
		if best[i].d != best[j].d {
			return best[i].d < best[j].d
		}
		return best[i].c < best[j].c
	})
	if len(best) > 3 {
		best = best[:3]
	}
	var names []string
	for _, b := range best {
		names = append(names, b.c)
	}
	return "; did you mean " + strings.Join(names, ", ") + "?"
}

// levenshtein is the edit distance between two strings, in runes.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string { return keysOf(m) }

// headingAnchors lists the anchors of headings.
func headingAnchors(hs []Heading) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Anchor)
	}
	return out
}

// mdRefRE finds [[reference]] and [[reference|text]] in Markdown.
var mdRefRE = regexp.MustCompile(`\[\[([^\]|]+?)(?:\|([^\]]*))?\]\]`)

// mdRef is one reference found in Markdown.
type mdRef struct {
	ref  string
	line int // 1-based line in the text
}

// markdownRefs finds the references in Markdown text outside fenced code
// blocks and code spans.
func markdownRefs(text string) []mdRef {
	var out []mdRef
	inFence := false
	for i, l := range strings.Split(text, "\n") {
		if fenceRE.MatchString(l) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		l = codeSpanRE.ReplaceAllStringFunc(l, func(s string) string { return strings.Repeat(" ", len(s)) })
		for _, m := range mdRefRE.FindAllStringSubmatch(l, -1) {
			out = append(out, mdRef{ref: strings.TrimSpace(m[1]), line: i + 1})
		}
	}
	return out
}

var codeSpanRE = regexp.MustCompile("`[^`]*`")
