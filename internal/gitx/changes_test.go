package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/proto"
)

func TestParseNumstat(t *testing.T) {
	in := "3\t1\tsrc/a.go\x00-\t-\timg.png\x00" + "2\t2\t\x00old/name.txt\x00new/name.txt\x00" + "1\t0\tbad\x1b[31m.txt\x00"
	files, more := ParseNumstat([]byte(in), 10)
	want := []proto.ChangedFile{{Path: "src/a.go", Added: 3, Deleted: 1}, {Path: "img.png", Binary: true},
		{Path: "new/name.txt", OldPath: "old/name.txt", Added: 2, Deleted: 2}, {Path: "bad�[31m.txt", Added: 1}}
	if more != 0 || !slices.Equal(files, want) {
		t.Errorf("ParseNumstat: %+v %d", files, more)
	}
	files, more = ParseNumstat([]byte(in), 2)
	if len(files) != 2 || more != 2 {
		t.Errorf("capped: %+v %d", files, more)
	}
	for _, bad := range []string{"x\t1\tf\x00", "1\tf\x00", "1\t1\t\x00only-old\x00"} {
		if files, _ := ParseNumstat([]byte(bad), 10); len(files) != 0 {
			t.Errorf("ParseNumstat(%q) = %+v", bad, files)
		}
	}
}

// TestChanges computes both scopes on a relay filled by banks, with host
// git run only against the relay.
func TestChanges(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	def, err := w.svc.InitRelay(ctx, w.p)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.InitBox(ctx, w.p, def); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(w.box, "main")
	res, err := w.svc.Changes(ctx, w.p, proto.ScopeSnapshot, "main", "main")
	if err != nil || res.Base != "" || res.Note == "" || len(res.Files) != 0 {
		t.Errorf("nothing banked yet: %+v %v", res, err)
	}

	// A commit on main and uncommitted work, banked.
	os.WriteFile(filepath.Join(main, "g"), []byte("a\nb\n"), 0o644)
	w.git(main, "add", "g")
	w.git(main, "commit", "-qm", "C1")
	os.WriteFile(filepath.Join(main, "f"), []byte("1\n2\n"), 0o644)
	os.WriteFile(filepath.Join(main, "new.txt"), []byte("x\n"), 0o644)
	if _, err := w.svc.Bank(ctx, w.p); err != nil {
		t.Fatal(err)
	}
	tip := w.git(w.p.Relay.Dir, "rev-parse", "refs/heads/main")
	res, err = w.svc.Changes(ctx, w.p, proto.ScopeSnapshot, "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	names := func(r proto.ChangesResult) []string {
		var out []string
		for _, f := range r.Files {
			out = append(out, f.Path)
		}
		return out
	}
	if res.Base != tip || res.Stale || res.At == nil || !slices.Equal(names(res), []string{"f", "new.txt"}) || res.Added != 2 || res.Deleted != 0 ||
		!strings.Contains(res.Diff, "+2") || !strings.Contains(res.BaseLabel, "main") || res.Truncated {
		t.Errorf("snapshot: %+v", res)
	}
	res, err = w.svc.Changes(ctx, w.p, proto.ScopeBranch, "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	if res.Head != tip || !strings.HasPrefix(res.BaseLabel, "github/main") || !slices.Equal(names(res), []string{"g"}) || !strings.Contains(res.Diff, "+b") {
		t.Errorf("branch: %+v", res)
	}

	// Everything committed: the old snapshot no longer describes the
	// worktree.
	w.git(main, "add", "-A")
	w.git(main, "commit", "-qm", "C2")
	if _, err := w.svc.Bank(ctx, w.p); err != nil {
		t.Fatal(err)
	}
	if res, err = w.svc.Changes(ctx, w.p, proto.ScopeSnapshot, "main", "main"); err != nil || !res.Stale || res.Note == "" {
		t.Errorf("a stale snapshot: %+v %v", res, err)
	}
	if res, _ = w.svc.Changes(ctx, w.p, proto.ScopeBranch, "main", "nobranch"); res.Note == "" || res.Base != "" {
		t.Errorf("an unknown branch: %+v", res)
	}
	if _, err := w.svc.Changes(ctx, w.p, "all", "main", "main"); err == nil {
		t.Error("an unknown scope")
	}
	// Every diff ran without external diff drivers or text conversion.
	diffs := 0
	for _, c := range w.rec.calls {
		if i := slices.Index(c.Args, "diff"); i > 0 && c.Name == "git" {
			diffs++
			if !slices.Contains(c.Args, "--no-ext-diff") || !slices.Contains(c.Args, "--no-textconv") {
				t.Errorf("diff without the safety options: %q", c.Args)
			}
		}
	}
	if diffs < 4 {
		t.Errorf("%d diffs recorded", diffs)
	}
	w.checkCalls()
}
