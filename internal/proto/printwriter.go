// This file implements PrintableWriter, the streaming form of
// PrintableText for output that goes to a terminal as it arrives.

package proto

import (
	"io"
	"unicode"
	"unicode/utf8"
)

// PrintableWriter passes a stream of text that a box can influence (the
// output of a vendor command run on box-writable state, finding X4 2) to a
// terminal with the escaping of PrintableText: control and format
// characters, escape sequences among them, become U+FFFD; newlines and
// tabs pass. A carriage return passes only directly before a newline, the
// way a terminal ends its lines, so the output of a command run with a
// terminal keeps its lines in a terminal switched to raw mode. A UTF-8
// sequence or a carriage return split between two writes is held back
// until the next one; Close writes what is still held.
type PrintableWriter struct {
	w    io.Writer
	held []byte
}

// NewPrintableWriter returns a PrintableWriter that writes to w.
func NewPrintableWriter(w io.Writer) *PrintableWriter { return &PrintableWriter{w: w} }

// Write escapes b and writes it; it reports len(b) unless w fails.
func (p *PrintableWriter) Write(b []byte) (int, error) {
	data := make([]byte, 0, len(p.held)+len(b))
	data = append(append(data, p.held...), b...)
	p.held = nil
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		c := data[i]
		switch {
		case c == '\r':
			if i+1 == len(data) {
				p.held = append(p.held, c)
				i++
				continue
			}
			if data[i+1] == '\n' {
				out = append(out, '\r', '\n')
				i += 2
				continue
			}
			out = utf8.AppendRune(out, utf8.RuneError)
			i++
			continue
		case c < utf8.RuneSelf:
			if c == '\n' || c == '\t' || (c >= 0x20 && c != 0x7f) {
				out = append(out, c)
			} else {
				out = utf8.AppendRune(out, utf8.RuneError)
			}
			i++
			continue
		case !utf8.FullRune(data[i:]):
			p.held = append(p.held, data[i:]...)
			i = len(data)
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		if (r == utf8.RuneError && size == 1) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			out = utf8.AppendRune(out, utf8.RuneError)
		} else {
			out = append(out, data[i:i+size]...)
		}
		i += size
	}
	if len(out) > 0 {
		if _, err := p.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// Close writes what is still held back (an incomplete UTF-8 sequence or a
// final carriage return, each as U+FFFD). It does not close w.
func (p *PrintableWriter) Close() error {
	if len(p.held) == 0 {
		return nil
	}
	held := p.held
	p.held = nil
	_, err := io.WriteString(p.w, PrintableText(string(held)))
	return err
}
