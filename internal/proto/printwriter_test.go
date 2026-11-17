package proto

import (
	"bytes"
	"testing"
)

// TestPrintableWriter: a stream is escaped as PrintableText escapes text,
// whatever the writes' boundaries: a UTF-8 sequence or a CR LF split
// between writes passes whole; escape sequences, a bell, a lone CR, an
// invalid byte and a format character become U+FFFD (finding X4 2).
func TestPrintableWriter(t *testing.T) {
	in := "grüße \x1b]0;title\x07ok\r\nspin\r|\xffend‮!\r"
	want := "grüße �]0;title�ok\r\nspin�|�end�!�"
	for _, step := range []int{1, 2, 3, len(in)} {
		var out bytes.Buffer
		w := NewPrintableWriter(&out)
		for i := 0; i < len(in); i += step {
			end := min(i+step, len(in))
			if n, err := w.Write([]byte(in[i:end])); err != nil || n != end-i {
				t.Fatalf("step %d: write: %d %v", step, n, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Errorf("step %d: %q, want %q", step, out.String(), want)
		}
	}
	// A sequence cut off at the end is written as one replacement.
	var out bytes.Buffer
	w := NewPrintableWriter(&out)
	w.Write([]byte("a\xc3"))
	w.Close()
	if out.String() != "a�" {
		t.Errorf("cut off: %q", out.String())
	}
}
