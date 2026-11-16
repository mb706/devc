package main

import (
	"strings"
	"testing"
)

func TestSplitRef(t *testing.T) {
	cases := []struct{ ref, kind, rest string }{
		{"internal/box.Up", "symbol", "internal/box.Up"},
		{"pkg:internal/box", "pkg", "internal/box"},
		{"http:POST /api/commands", "http", "POST /api/commands"},
		{"cmd:devc box init", "cmd", "devc box init"},
		{"doc:specs-v2.md#13", "doc", "specs-v2.md#13"},
	}
	for _, c := range cases {
		k, r, ok := splitRef(c.ref)
		if !ok || k != c.kind || r != c.rest {
			t.Errorf("splitRef(%q) = %q, %q, %v", c.ref, k, r, ok)
		}
	}
	if _, _, ok := splitRef("bogus:x"); ok {
		t.Error("an unknown kind was accepted")
	}
	for ref, want := range map[string][2]string{
		"internal/box.Manager.Up": {"internal/box", "Manager.Up"},
		"cmd/devc.main":           {"cmd/devc", "main"},
		"examples.FS":             {"examples", "FS"},
		"internal/box":            {"internal/box", ""},
	} {
		if p, m := splitSymbol(ref); p != want[0] || m != want[1] {
			t.Errorf("splitSymbol(%q) = %q, %q", ref, p, m)
		}
	}
}

func TestResolve(t *testing.T) {
	w := loadTestWorld(t)
	ix := testIndex(t, w)
	dispatchLine := 0
	for i, l := range strings.Split(ix.files["server/server.go"], "\n") {
		if strings.Contains(l, "dispatch") {
			dispatchLine = i + 1
			break
		}
	}
	good := []struct {
		ref, concern string
		want         ResolvedRef
	}{
		{"proto.MethodPing", "", ResolvedRef{Kind: "symbol", Key: "proto.MethodPing", File: "proto/proto.go", Line: w.syms["proto.MethodPing"].Line}},
		{"server.Server.ping", "", ResolvedRef{Kind: "symbol", Key: "server.Server.ping", File: "server/server.go", Line: w.syms["server.Server.ping"].Line}},
		{"pkg:server", "", ResolvedRef{Kind: "package", Key: "server"}},
		{"file:server/server.go", "", ResolvedRef{Kind: "file", Key: "server/server.go", File: "server/server.go"}},
		{"file:server/server.go~dispatch", "", ResolvedRef{Kind: "file", Key: "server/server.go", File: "server/server.go", Line: dispatchLine}},
		{"cmd:devc up", "", ResolvedRef{Kind: "command", Key: "devc up"}},
		{"doc:docs/plan.md#3.6", "", ResolvedRef{Kind: "doc", Key: "docs/plan.md", Anchor: "36-events-and-journal", Line: 5}},
		{"doc:docs/plan.md#3-decisions", "", ResolvedRef{Kind: "doc", Key: "docs/plan.md", Anchor: "3-decisions", Line: 3}},
		{"finding:C1-05", "", ResolvedRef{Kind: "finding", Key: "C1-05"}},
		{"chunk:C10a", "", ResolvedRef{Kind: "chunk", Key: "C10a"}},
		{"concern:network", "", ResolvedRef{Kind: "concern", Key: "network"}},
		{"diagram:states", "network", ResolvedRef{Kind: "diagram", Key: "network/states"}},
	}
	for _, c := range good {
		r, err := ix.resolve(c.ref, c.concern)
		if err != nil {
			t.Errorf("%s: %v", c.ref, err)
			continue
		}
		if c.ref == "rpc:x.send" {
			continue
		}
		if r != c.want {
			t.Errorf("%s: %+v, want %+v", c.ref, r, c.want)
		}
	}
	if r, err := ix.resolve("rpc:x.send", ""); err != nil || r.Kind != "rpc" || r.File != "proto/proto.go" {
		t.Errorf("rpc:x.send: %+v %v", r, err)
	}
	bad := map[string][]string{
		// A near miss names the nearest names that exist.
		"server.Server.png":         {"unknown symbol", "did you mean server.Server.ping"},
		"servr.Server":              {"unknown package", "did you mean server"},
		"pkg:sever":                 {"did you mean server"},
		"concern:netwrk":            {"unknown concern", "did you mean network"},
		"bogus:x":                   {"unknown kind of reference"},
		"doc:docs/plan.md#9.9":      {"has no heading"},
		"file:server/server.go~zzz": {"no line of server/server.go contains"},
		"server":                    {"names a package; write pkg:server"},
		"rpc:x.sned":                {"did you mean x.send"},
	}
	for ref, wants := range bad {
		_, err := ix.resolve(ref, "")
		if err == nil {
			t.Errorf("%s resolved", ref)
			continue
		}
		for _, want := range wants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %q does not contain %q", ref, err.Error(), want)
			}
		}
	}
}

func TestMarkdownRefs(t *testing.T) {
	text := "See [[internal/box.Up]] and [[term:relay|the relay]].\n" +
		"```\n[[not.a.ref]]\n```\n" +
		"Code `[[nor.this]]` and [[pkg:internal/box]] again."
	var got []string
	for _, r := range markdownRefs(text) {
		got = append(got, r.ref)
		if r.ref == "pkg:internal/box" && r.line != 5 {
			t.Errorf("line %d", r.line)
		}
	}
	want := "internal/box.Up term:relay pkg:internal/box"
	if strings.Join(got, " ") != want {
		t.Errorf("got %v, want %s", got, want)
	}
}
