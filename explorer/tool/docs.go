package main

// This file indexes the documents: headings, review findings with their
// responses, work orders, and the patterns that turn mentions of them in
// comments and documents into references.

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// docRoots are the documents: files and directories (searched for *.md).
var docRoots = []string{"README.md", "specs-v2.md", "docs", "explorer/README.md", "examples"}

// docFile is a document with its text.
type docFile struct {
	Document
	text string
}

// collectDocs reads and indexes the documents below root.
func collectDocs(root string) ([]*docFile, error) {
	var out []*docFile
	add := func(rel string) error {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		d := &docFile{text: string(b)}
		d.Path, d.Bytes = rel, len(b)
		d.Headings = parseHeadings(d.text)
		d.Title = path.Base(rel)
		if len(d.Headings) > 0 {
			d.Title = d.Headings[0].Text
		}
		d.Group = docGroup(rel)
		d.Chunk = "data/docs/" + strings.ReplaceAll(rel, "/", "~") + ".js"
		out = append(out, d)
		return nil
	}
	for _, r := range docRoots {
		p := filepath.Join(root, filepath.FromSlash(r))
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if err := add(r); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(q, ".md") {
				return err
			}
			return add(filepath.ToSlash(strings.TrimPrefix(q, root+string(filepath.Separator))))
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// docGroup sorts a document into the app's groups.
func docGroup(rel string) string {
	switch {
	case strings.HasPrefix(rel, "docs/dev/chunks/"):
		return "workorder"
	case strings.HasPrefix(rel, "docs/dev/reviews/"):
		return "review"
	case strings.HasPrefix(rel, "docs/dev/"):
		return "development"
	case strings.HasPrefix(rel, "explorer/"):
		return "explorer"
	case strings.HasPrefix(rel, "examples/"):
		return "example"
	}
	return "product"
}

var (
	headingRE    = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	headingNumRE = regexp.MustCompile(`^(\d+(?:\.\d+)*)\.?(?:\s|$)`)
	fenceRE      = regexp.MustCompile("^\\s*(```|~~~)")
)

// parseHeadings finds the ATX headings outside fenced code blocks.
func parseHeadings(text string) []Heading {
	var out []Heading
	used := map[string]int{}
	inFence := false
	for i, l := range strings.Split(text, "\n") {
		if fenceRE.MatchString(l) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := headingRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		h := Heading{Level: len(m[1]), Text: m[2], Line: i + 1}
		if n := headingNumRE.FindStringSubmatch(h.Text); n != nil {
			h.Number = n[1]
		}
		a := slug(h.Text)
		if k := used[a]; k > 0 {
			used[a] = k + 1
			a = a + "-" + itoa(k)
		} else {
			used[a] = 1
		}
		h.Anchor = a
		out = append(out, h)
	}
	return out
}

// slug makes a GitHub-style anchor: lower case, punctuation removed,
// spaces to hyphens.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// findingIDRE is the form of a finding's label: a review letter and digits,
// optionally a dash and a number ("A3", "C1-05", "F1-U1").
const findingIDRE = `[A-F][0-9]{1,2}(?:-U?[0-9]{1,2})?`

var (
	findingItemRE = regexp.MustCompile(`^\s*(?:\d+\.|[-*])\s+\*\*(` + findingIDRE + `)\s+—\s+(.*?)\*\*`)
	findingIDOnly = regexp.MustCompile(`\b` + findingIDRE + `\b`)
	responseLead  = regexp.MustCompile(`^\*\*(.+?)\*\*`)
	severityRE    = regexp.MustCompile(`^((?:low|medium|high|critical)[^:]*):\s+(.*)$`)
)

// indexFindings reads the review findings and their responses.
func indexFindings(docs []*docFile) []Finding {
	byPath := map[string]*docFile{}
	for _, d := range docs {
		byPath[d.Path] = d
	}
	var all []Finding
	var reviews []string
	for _, d := range docs {
		if d.Group != "review" || !strings.HasSuffix(d.Path, "-review.md") {
			continue
		}
		review := strings.TrimSuffix(path.Base(d.Path), "-review.md")
		reviews = append(reviews, review)
		fs := parseFindings(review, d)
		resp := byPath["docs/dev/reviews/"+review+"-response.md"]
		if resp == nil && review != "" {
			resp = byPath["docs/dev/reviews/"+review[:1]+"-response.md"]
		}
		if resp != nil {
			attachResponses(fs, resp)
		}
		all = append(all, fs...)
	}
	// A label found in two reviews keeps the plain ID in the review whose
	// name starts with its letter.
	count := map[string]int{}
	for _, f := range all {
		count[f.Label]++
	}
	for i := range all {
		f := &all[i]
		f.ID = f.Label
		if count[f.Label] > 1 && !strings.HasPrefix(f.Review, f.Label[:1]) {
			f.ID = f.Review + "/" + f.Label
		}
	}
	_ = reviews
	return all
}

// parseFindings reads the findings of one review file.
func parseFindings(review string, d *docFile) []Finding {
	lines := strings.Split(d.text, "\n")
	var out []Finding
	unconfirmed := 0 // level of the "Unconfirmed" heading, 0 outside
	cur := -1
	var body []string
	flush := func() {
		if cur >= 0 {
			out[cur].Text = dedent(body)
		}
		body = nil
	}
	for i, l := range lines {
		if m := headingRE.FindStringSubmatch(l); m != nil {
			flush()
			cur = -1
			level := len(m[1])
			if strings.Contains(strings.ToLower(m[2]), "unconfirmed") {
				unconfirmed = level
			} else if unconfirmed > 0 && level <= unconfirmed {
				unconfirmed = 0
			}
			continue
		}
		if m := findingItemRE.FindStringSubmatch(l); m != nil {
			flush()
			f := Finding{Label: m[1], Review: review, File: d.Path, Line: i + 1, Unconfirmed: unconfirmed > 0}
			f.Meta, f.Title = splitTitle(m[2])
			out = append(out, f)
			cur = len(out) - 1
			continue
		}
		if cur < 0 {
			continue
		}
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			flush()
			cur = -1
			continue
		}
		body = append(body, l)
	}
	flush()
	return out
}

// splitTitle separates the class and severity from a finding's title.
func splitTitle(s string) (string, string) {
	parts := strings.Split(s, " — ")
	title := parts[len(parts)-1]
	meta := strings.Join(parts[:len(parts)-1], " — ")
	if m := severityRE.FindStringSubmatch(title); m != nil {
		if meta != "" {
			meta += " — "
		}
		meta += m[1]
		title = m[2]
	}
	return meta, strings.TrimSpace(title)
}

// dedent removes the common indentation and surrounding blank lines.
func dedent(lines []string) string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) >= indent && indent > 0 {
			l = l[indent:]
		}
		out[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(out, "\n")
}

// attachResponses finds, for each finding, the paragraph of the response
// file whose bold lead names it.
func attachResponses(fs []Finding, resp *docFile) {
	idx := map[string][]int{}
	for i, f := range fs {
		idx[f.Label] = append(idx[f.Label], i)
	}
	lines := strings.Split(resp.text, "\n")
	type para struct {
		labels []string
		start  int
		end    int
	}
	var paras []para
	for i, l := range lines {
		isHeading := headingRE.MatchString(l)
		var labels []string
		if m := responseLead.FindStringSubmatch(l); m != nil {
			for _, id := range findingIDOnly.FindAllString(m[1], -1) {
				if _, ok := idx[id]; ok {
					labels = append(labels, id)
				}
			}
		}
		if len(labels) > 0 || isHeading {
			if n := len(paras); n > 0 && paras[n-1].end < 0 {
				paras[n-1].end = i
			}
		}
		if len(labels) > 0 {
			paras = append(paras, para{labels: labels, start: i, end: -1})
		}
	}
	if n := len(paras); n > 0 && paras[n-1].end < 0 {
		paras[n-1].end = len(lines)
	}
	for _, p := range paras {
		text := strings.TrimSpace(strings.Join(lines[p.start:p.end], "\n"))
		for _, l := range p.labels {
			for _, i := range idx[l] {
				if fs[i].Response == "" {
					fs[i].Response, fs[i].ResponseFile, fs[i].ResponseLine = text, resp.Path, p.start+1
				}
			}
		}
	}
}

// indexWorkOrders lists the work orders.
func indexWorkOrders(docs []*docFile) []WorkOrder {
	var out []WorkOrder
	for _, d := range docs {
		if d.Group != "workorder" {
			continue
		}
		base := strings.TrimSuffix(path.Base(d.Path), ".md")
		id, _, _ := strings.Cut(base, "-")
		out = append(out, WorkOrder{ID: id, Title: d.Title, File: d.Path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// workOrderIDRE is the form of a work order's ID ("C01", "C10a", "C12r1",
// "X1").
const workOrderIDRE = `[CX][0-9]{1,2}(?:[a-z][0-9]?)?`

// listOf makes a regular expression for a list of items: "A", "A, B",
// "A and B", "A–B".
func listOf(item string) string {
	return `(?:` + item + `)(?:(?:,\s*|,?\s+and\s+|\s*–\s*)(?:` + item + `))*`
}

// docPatterns are the patterns the app turns into references.
func docPatterns() []Pattern {
	return []Pattern{
		{Name: "findings", Regex: `\b[Ff]indings?\s+(` + listOf(findingIDRE) + `)\b`, Group: 1,
			Split: findingIDRE, Target: "finding:{}", Lookup: "finding"},
		{Name: "finding", Regex: `\b([A-F][0-9]-U?[0-9]{1,2})\b`, Group: 1, Target: "finding:{}", Lookup: "finding"},
		{Name: "work order", Regex: `\b(?:[Ww]ork orders?|[Cc]hunks?)\s+(` + listOf(workOrderIDRE) + `)\b`, Group: 1,
			Split: workOrderIDRE, Target: "chunk:{}", Lookup: "chunk"},
		{Name: "plan", Regex: `\b[Pp]lan\s+(?:section\s+)?([0-9]+(?:\.[0-9]+)*)`, Group: 1,
			Target: "doc:docs/dev/plan.md#{}", Lookup: "doc"},
		{Name: "README rule", Regex: `\bREADME rules?\s+([1-9])`, Group: 1,
			Target: "doc:docs/dev/README.md#security-rules-for-code", Lookup: "doc"},
		{Name: "spec", Regex: `\b[Ss]pec(?:ification)?(?:\s+section)?\s+([0-9]+(?:\.[0-9]+)*)`, Group: 1,
			Target: "doc:specs-v2.md#{}", Lookup: "doc"},
	}
}

// patternMatch is one reference found by applying the patterns.
type patternMatch struct {
	Start, End int // byte offsets of the link in the text
	Ref        string
}

// applyPatterns applies patterns as the app does (see Pattern), with
// lookup deciding whether a key exists. It exists for tests and for the
// tool's own use.
func applyPatterns(text string, pats []Pattern, lookup func(kind, ref string) bool) []patternMatch {
	var out []patternMatch
	var taken [][2]int
	overlaps := func(a, b int) bool {
		for _, t := range taken {
			if a < t[1] && t[0] < b {
				return true
			}
		}
		return false
	}
	for _, p := range pats {
		re := regexp.MustCompile(p.Regex)
		var split *regexp.Regexp
		if p.Split != "" {
			split = regexp.MustCompile(p.Split)
		}
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			if overlaps(m[0], m[1]) {
				continue
			}
			gs, ge := m[2*p.Group], m[2*p.Group+1]
			if gs < 0 {
				continue
			}
			var keys [][2]int
			if split != nil {
				for _, k := range split.FindAllStringIndex(text[gs:ge], -1) {
					keys = append(keys, [2]int{gs + k[0], gs + k[1]})
				}
			} else {
				keys = [][2]int{{gs, ge}}
			}
			found := false
			for _, k := range keys {
				ref := strings.ReplaceAll(p.Target, "{}", text[k[0]:k[1]])
				if p.Lookup != "" && lookup != nil && !lookup(p.Lookup, ref) {
					continue
				}
				pm := patternMatch{Start: k[0], End: k[1], Ref: ref}
				if split == nil {
					pm.Start, pm.End = m[0], m[1]
				}
				out = append(out, pm)
				found = true
			}
			if found {
				taken = append(taken, [2]int{m[0], m[1]})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// citeInComments records, on every finding and work order, the places in
// the module's Go comments that mention it (as the app would link them),
// so that a reader can go from a finding to the hardening it explains.
// A site's In is the declaration the comment documents or sits in.
func citeInComments(w *world, findings []Finding, orders []WorkOrder) {
	fi := map[string]int{}
	for i, f := range findings {
		fi[f.ID] = i
	}
	oi := map[string]int{}
	for i, o := range orders {
		oi[o.ID] = i
	}
	lookup := func(kind, ref string) bool {
		switch kind {
		case "finding":
			_, ok := fi[strings.TrimPrefix(ref, "finding:")]
			return ok
		case "chunk":
			_, ok := oi[strings.TrimPrefix(ref, "chunk:")]
			return ok
		}
		// Documents are not recorded; patterns that come later cannot take
		// a span from the findings and work orders, which come first.
		return false
	}
	var pats []Pattern
	for _, p := range docPatterns() {
		if p.Lookup == "finding" || p.Lookup == "chunk" {
			pats = append(pats, p)
		}
	}
	byFile := map[string][]*symInfo{}
	for _, s := range w.symList {
		byFile[s.File] = append(byFile[s.File], s)
	}
	seen := map[string]bool{}
	for _, f := range w.prog.files {
		if f.AST == nil {
			continue
		}
		syms := byFile[f.Path]
		for _, cg := range f.AST.Comments {
			for _, c := range cg.List {
				line := f.line(c.Pos())
				for i, l := range strings.Split(c.Text, "\n") {
					for _, m := range applyPatterns(l, pats, lookup) {
						site := Site{File: f.Path, Line: line + i, In: commentOwner(syms, line+i, f.line(cg.End()))}
						key := m.Ref + "\x00" + site.File + "\x00" + itoa(site.Line)
						if seen[key] {
							continue
						}
						seen[key] = true
						if id, ok := strings.CutPrefix(m.Ref, "finding:"); ok {
							findings[fi[id]].Cited = append(findings[fi[id]].Cited, site)
						} else if id, ok := strings.CutPrefix(m.Ref, "chunk:"); ok {
							orders[oi[id]].Cited = append(orders[oi[id]].Cited, site)
						}
					}
				}
			}
		}
	}
}

// commentOwner is the innermost declaration around a comment line, else
// the declaration that a comment group ending on groupEnd documents.
func commentOwner(syms []*symInfo, line, groupEnd int) string {
	best, bestLen := "", 1<<30
	for _, s := range syms {
		if s.Line <= line && line <= s.EndLine && s.EndLine-s.Line < bestLen {
			best, bestLen = s.Ref, s.EndLine-s.Line
		}
	}
	if best != "" {
		return best
	}
	for _, s := range syms {
		if s.Line == groupEnd+1 && s.Parent == "" {
			return s.Ref
		}
	}
	return ""
}
