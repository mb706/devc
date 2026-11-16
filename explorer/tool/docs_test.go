package main

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestPatterns(t *testing.T) {
	findings := map[string]bool{"C1-05": true, "B1": true, "B4": true, "D1-03": true, "D1-04": true, "A1": true}
	chunks := map[string]bool{"C10a": true, "C12r1": true}
	docs := map[string]bool{"doc:docs/dev/plan.md#3.6": true, "doc:specs-v2.md#13": true, "doc:specs-v2.md#7.2": true,
		"doc:docs/dev/README.md#security-rules-for-code": true}
	lookup := func(kind, ref string) bool {
		k, rest, _ := strings.Cut(ref, ":")
		switch kind {
		case "finding":
			return k == "finding" && findings[rest]
		case "chunk":
			return k == "chunk" && chunks[rest]
		case "doc":
			return docs[ref]
		}
		return false
	}
	text := "Fixed (finding C1-05); see findings B1 and B4, work order C10a and chunk C12r1; plan 3.6, " +
		"README rule 2, spec section 13 (spec 7.2). A1 alone, C1-99 unknown, findings D1-03, D1-04. plan 9.9."
	var refs, links []string
	for _, m := range applyPatterns(text, docPatterns(), lookup) {
		refs = append(refs, m.Ref)
		links = append(links, text[m.Start:m.End])
	}
	wantRefs := []string{"finding:C1-05", "finding:B1", "finding:B4", "chunk:C10a", "chunk:C12r1", "doc:docs/dev/plan.md#3.6",
		"doc:docs/dev/README.md#security-rules-for-code", "doc:specs-v2.md#13", "doc:specs-v2.md#7.2", "finding:D1-03", "finding:D1-04"}
	if !reflect.DeepEqual(refs, wantRefs) {
		t.Errorf("refs\n%v\nwant\n%v", refs, wantRefs)
	}
	wantLinks := []string{"C1-05", "B1", "B4", "C10a", "C12r1", "plan 3.6", "README rule 2", "spec section 13", "spec 7.2", "D1-03", "D1-04"}
	if !reflect.DeepEqual(links, wantLinks) {
		t.Errorf("links\n%v\nwant\n%v", links, wantLinks)
	}
	// The app applies the same expressions in JavaScript: nothing beyond
	// the syntax both share.
	for _, p := range docPatterns() {
		for _, re := range []string{p.Regex, p.Split} {
			if re == "" {
				continue
			}
			if _, err := regexp.Compile(re); err != nil {
				t.Errorf("%s: %v", p.Name, err)
			}
			for _, bad := range []string{"(?P<", "(?<", "(?i", `\z`, `\A`, "[[:"} {
				if strings.Contains(re, bad) {
					t.Errorf("%s: %q is not portable to JavaScript", p.Name, bad)
				}
			}
		}
		if !strings.Contains(p.Target, ":") {
			t.Errorf("%s: target %q is not a reference", p.Name, p.Target)
		}
	}
}

func TestParseHeadings(t *testing.T) {
	text := "# devc v2 — specification\n\n## 1. Purpose\n\n```\n# not a heading\n```\n### 6.2 Creation flags\n## Notes\n## Notes\n"
	hs := parseHeadings(text)
	var got []string
	for _, h := range hs {
		got = append(got, strings.Join([]string{itoa(h.Level), h.Number, h.Anchor, itoa(h.Line)}, "|"))
	}
	want := []string{"1||devc-v2--specification|1", "2|1|1-purpose|3", "3|6.2|62-creation-flags|8", "2||notes|9", "2||notes-1|10"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFindings(t *testing.T) {
	review := func(name, text string) *docFile {
		return &docFile{Document: Document{Path: "docs/dev/reviews/" + name, Group: "review"}, text: text}
	}
	a := review("A-review.md", "1. **A1 — class A, high — Privileged commands use a box-writable PATH.**  \n"+
		"   Files: x.go.\n\n   **Scenario:** bad.\n\n"+
		"2. **B1 — class B, medium — Failed startup leaves resources.**\n   Text of A's B1.\n\nA closing paragraph.\n")
	aResp := review("A-response.md", "# Response to review A\n\n**A1: fixed.** Root exec has a fixed PATH.\n- detail\n\n**B1: fixed.** Rolled back.\n")
	b := review("B-review.md", "1. **B1 — earlier-finding — high: The sentinel does not prove firewall enforcement (A5).**\n   Text.\n\n"+
		"### Unconfirmed\n\n13. **B13 — security — high if confirmed: Resolver exceptions may survive.**\n    Maybe.\n")
	f1 := review("F1-review.md", "1. **F1-01 — (d), medium — Failed group termination is discarded.**\n   Text.\n\n## Unconfirmed\n\n"+
		"- **F1-U1 — (e), medium — Group signaling continues.**\n  Text.\n")
	fResp := review("F-response.md", "# Response\n\n**F1-01 and F1-U1 (process groups) — fixed together.** Details.\n")
	got := indexFindings([]*docFile{a, aResp, b, f1, fResp})
	type row struct {
		id, review, meta, title string
		unconfirmed             bool
		resp                    string
	}
	var rows []row
	for _, f := range got {
		resp := f.Response
		if i := strings.Index(resp, "\n"); i >= 0 {
			resp = resp[:i]
		}
		rows = append(rows, row{f.ID, f.Review, f.Meta, f.Title, f.Unconfirmed, resp})
	}
	want := []row{
		{"A1", "A", "class A, high", "Privileged commands use a box-writable PATH.", false, "**A1: fixed.** Root exec has a fixed PATH."},
		{"A/B1", "A", "class B, medium", "Failed startup leaves resources.", false, "**B1: fixed.** Rolled back."},
		{"B1", "B", "earlier-finding — high", "The sentinel does not prove firewall enforcement (A5).", false, ""},
		{"B13", "B", "security — high if confirmed", "Resolver exceptions may survive.", true, ""},
		{"F1-01", "F1", "(d), medium", "Failed group termination is discarded.", false, "**F1-01 and F1-U1 (process groups) — fixed together.** Details."},
		{"F1-U1", "F1", "(e), medium", "Group signaling continues.", true, "**F1-01 and F1-U1 (process groups) — fixed together.** Details."},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("got\n%+v\nwant\n%+v", rows, want)
	}
	if got[0].Text != "Files: x.go.\n\n**Scenario:** bad." {
		t.Errorf("text of A1: %q", got[0].Text)
	}
	if got[1].Text != "Text of A's B1." {
		t.Errorf("text of A's B1 (the closing paragraph must not belong to it): %q", got[1].Text)
	}
	if !strings.Contains(got[0].Response, "- detail") {
		t.Errorf("the response of A1 lost its list: %q", got[0].Response)
	}
}
