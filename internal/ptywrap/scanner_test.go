package ptywrap

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

// feedAll feeds data in pieces of the given sizes (cycled) and returns the
// output and the items.
func feedAll(data []byte, sizes []int) ([]byte, []Item) {
	var sc Scanner
	var out []byte
	var items []Item
	for i, k := 0, 0; i < len(data); k++ {
		n := sizes[k%len(sizes)]
		if n <= 0 {
			n = 1
		}
		end := min(i+n, len(data))
		var its []Item
		out, its = sc.Feed(out, data[i:end])
		items = append(items, its...)
		i = end
	}
	return sc.Flush(out), items
}

func TestScannerBasics(t *testing.T) {
	for _, c := range []struct {
		name, in, out string
		bodies        []string
	}{
		{"plain", "hello\x1b[1mworld\x1b[0m", "hello\x1b[1mworld\x1b[0m", nil},
		{"bel", "a\x1b]7501;state=working\x07b", "ab", []string{"state=working"}},
		{"st", "a\x1b]7501;state=done:msg=aGk=\x1b\\b", "ab", []string{"state=done:msg=aGk="}},
		{"query", "\x1b]7501;?\x07\x1b[c", "\x1b[c", []string{"?"}},
		{"other osc", "\x1b]0;title\x07\x1b]75010;x\x07\x1b]7501\x07", "\x1b]0;title\x07\x1b]75010;x\x07\x1b]7501\x07", nil},
		{"aborted by escape", "\x1b]7501;state=wor\x1b[1mX", "\x1b]7501;state=wor\x1b[1mX", nil},
		{"aborted by control", "\x1b]7501;state\nX\x07", "\x1b]7501;state\nX\x07", nil},
		{"two", "\x1b]7501;state=idle\x07\x1b]7501;state=working\x1b\\", "", []string{"state=idle", "state=working"}},
		{"esc esc", "\x1b\x1b]7501;state=idle\x07z", "\x1bz", []string{"state=idle"}},
		{"unterminated at end", "x\x1b]7501;state=idle", "x\x1b]7501;state=idle", nil},
		{"lone esc at end", "x\x1b", "x\x1b", nil},
	} {
		// Whole, byte by byte, and in pieces of three.
		for _, sizes := range [][]int{{len(c.in) + 1}, {1}, {3}, {2, 5}} {
			out, items := feedAll([]byte(c.in), sizes)
			if string(out) != c.out {
				t.Errorf("%s %v: out %q, want %q", c.name, sizes, out, c.out)
			}
			var bodies []string
			for _, it := range items {
				bodies = append(bodies, it.Body)
			}
			if strings.Join(bodies, "|") != strings.Join(c.bodies, "|") {
				t.Errorf("%s %v: items %q, want %q", c.name, sizes, bodies, c.bodies)
			}
		}
	}
	// The terminator is kept for the answer.
	_, items := feedAll([]byte("\x1b]7501;?\x1b\\"), []int{100})
	if len(items) != 1 || items[0].Term != "\x1b\\" || !items[0].Query() {
		t.Errorf("query item: %+v", items)
	}
}

func TestScannerOversized(t *testing.T) {
	body := "state=working:msg=" + strings.Repeat("A", MaxSequence)
	in := "a\x1b]7501;" + body + "\x07b"
	for _, sizes := range [][]int{{len(in)}, {1}, {1000}, {4095}} {
		out, items := feedAll([]byte(in), sizes)
		if string(out) != in || len(items) != 0 {
			t.Errorf("%v: an oversized sequence was not passed through untouched (%d items)", sizes, len(items))
		}
	}
	// Exactly at the limit is still a sequence.
	pad := MaxSequence - len("\x1b]7501;") - 1
	exact := "\x1b]7501;" + "state=idle:x=" + strings.Repeat("A", pad-len("state=idle:x=")) + "\x07"
	if len(exact) != MaxSequence {
		t.Fatalf("test sequence has %d bytes", len(exact))
	}
	out, items := feedAll([]byte(exact), []int{7})
	if len(out) != 0 || len(items) != 1 {
		t.Errorf("a sequence of exactly %d bytes: out %d bytes, %d items", MaxSequence, len(out), len(items))
	}
	exact2 := strings.Replace(exact, "\x07", "\x1b\\", 1) // one byte longer
	if out, items := feedAll([]byte(exact2), []int{7}); string(out) != exact2 || len(items) != 0 {
		t.Errorf("a sequence of %d bytes was taken out", len(exact2))
	}
}

// TestScannerRandom mixes random bytes (escape characters included) with
// sequences, feeds them in random pieces, and checks that exactly the
// sequences are removed.
func TestScannerRandom(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	seqs := []string{"\x1b]7501;state=working:app=x\x07", "\x1b]7501;state=done:msg=ZG9uZQ\x1b\\", "\x1b]7501;?\x07"}
	for round := 0; round < 300; round++ {
		var in, want bytes.Buffer
		var wantItems int
		for part := 0; part < 20; part++ {
			n := rnd.Intn(200)
			chunk := make([]byte, n)
			rnd.Read(chunk)
			// Plenty of escape characters, never forming the prefix by chance:
			// "]7501;" after an escape is replaced.
			for i := range chunk {
				if rnd.Intn(10) == 0 {
					chunk[i] = 0x1b
				}
			}
			chunk = bytes.ReplaceAll(chunk, []byte("\x1b]7501;"), []byte("\x1b]7502;"))
			// A random chunk must not end in a prefix of the sequence start
			// that the next part completes; a trailing escape is fine
			// because the next part never starts with "]7501;".
			in.Write(chunk)
			want.Write(chunk)
			if rnd.Intn(3) == 0 {
				s := seqs[rnd.Intn(len(seqs))]
				in.WriteString(s)
				wantItems++
			}
		}
		var sizes []int
		for i := 0; i < 10; i++ {
			sizes = append(sizes, 1+rnd.Intn(64))
		}
		out, items := feedAll(in.Bytes(), sizes)
		if !bytes.Equal(out, want.Bytes()) || len(items) != wantItems {
			t.Fatalf("round %d: output differs (%d vs %d bytes) or %d items, want %d", round, len(out), want.Len(), len(items), wantItems)
		}
	}
}

func TestParseReport(t *testing.T) {
	ok := func(body string) Report {
		t.Helper()
		r, err := ParseReport(body)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		return r
	}
	if r := ok("state=working:app=claude-code"); r.State != "working" || r.App != "claude-code" || r.Progress != -1 {
		t.Errorf("%+v", r)
	}
	if r := ok(" state = blocked : kind=question : msg=V2hpY2ggb25lPw "); r.State != "blocked" || r.Kind != "question" || r.Msg != "Which one?" {
		t.Errorf("%+v", r)
	}
	if r := ok("state=blocked:kind=weird"); r.Kind != "" {
		t.Errorf("unknown kind kept: %+v", r)
	}
	if r := ok("state=working:kind=question:progress=40"); r.Kind != "" || r.Progress != 40 {
		t.Errorf("kind outside blocked or progress: %+v", r)
	}
	if r := ok("state=idle:state=done:Bad=1:bogus:x=y z:unknown=1"); r.State != "done" {
		t.Errorf("last wins, malformed pairs skipped: %+v", r)
	}
	if r := ok("state=done:id=sub/task.1"); r.ID != "sub/task.1" {
		t.Errorf("id: %+v", r)
	}
	if r := ok("state=done:msg=!!!"); r.Msg != "" {
		t.Errorf("a malformed pair is skipped: %+v", r)
	}
	if r := ok("state=clear"); r.State != StateClear {
		t.Errorf("clear: %+v", r)
	}
	for _, bad := range []string{
		"", "?", "app=x", "state=sleeping", "state=WORKING",
		"state=done:id=" + strings.Repeat("a", 33),
		"state=done:id=a//b",
		"state=done:app=" + strings.Repeat("a", 33),
		"state=done:msg=" + "aGkKdGhlcmU", // "hi\nthere": a newline
		"state=done:msg=" + strings.Repeat("QUFB", 700),
		"state=done:title=" + strings.Repeat("QUFB", 70),
		"state=done:msg=/w==", // invalid UTF-8
	} {
		if r, err := ParseReport(bad); err == nil {
			t.Errorf("%q accepted: %+v", bad, r)
		}
	}
}

// BenchmarkScanner measures the scanner on output without sequences.
func BenchmarkScanner(b *testing.B) {
	data := bytes.Repeat([]byte("some output \x1b[1mbold\x1b[0m and text\r\n"), 1000)
	var sc Scanner
	out := make([]byte, 0, len(data))
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		out, _ = sc.Feed(out[:0], data)
	}
}
