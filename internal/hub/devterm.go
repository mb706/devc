// The development mode's invented terminal: a screen in the style of a
// harness's TUI (24-bit and 256 colours, box drawing, a diff with coloured
// backgrounds), with a spinner that moves every second and, in control
// mode, a prompt line that shows what is typed.

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mb706/devc/internal/proto"
)

type devTerm struct {
	id      string
	conn    *proto.Conn
	cols    int
	rows    int
	control bool
	cancel  context.CancelFunc
	title   string
	cwd     string

	mu    sync.Mutex
	typed string
}

var devTerms sync.Map // stream ID → *devTerm

const (
	ansiReset  = "\x1b[0m"
	ansiOrange = "\x1b[38;2;215;119;87m"
	ansiDim    = "\x1b[38;5;245m"
	ansiGreen  = "\x1b[38;2;78;186;101m"
	ansiBold   = "\x1b[1m"
)

func (m *devMachine) termOpen(_ context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
	p, err := devDecode[proto.TerminalOpenParams](params)
	if err != nil {
		return nil, err
	}
	v, ok := m.viewOf(p.Session)
	switch {
	case !ok:
		return nil, proto.Errorf(proto.CodeNotFound, "no session %s", p.Session)
	case !v.Live:
		return nil, proto.Errorf(proto.CodeRefused, "the session has ended")
	case !proto.ValidStreamID(p.Stream):
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid stream id")
	}
	ctx, cancel := context.WithCancel(c.Context())
	t := &devTerm{id: p.Stream, conn: c, cols: 92, rows: 28, control: p.Mode == proto.TerminalControl, cancel: cancel,
		title: v.Harness, cwd: "/home/dev/" + v.Project + "/" + v.Worktree}
	devTerms.Store(p.Stream, t)
	go t.run(ctx)
	return proto.TerminalOpenResult{Stream: p.Stream, Mode: p.Mode, Cols: t.cols, Rows: t.rows}, nil
}

func (m *devMachine) termInput(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	p, err := devDecode[proto.TerminalInputParams](params)
	if err != nil {
		return nil, err
	}
	v, ok := devTerms.Load(p.Stream)
	if !ok {
		return nil, proto.Errorf(proto.CodeNotFound, "no such terminal")
	}
	t := v.(*devTerm)
	if !t.control {
		return nil, proto.Errorf(proto.CodeRefused, "this terminal only observes")
	}
	t.mu.Lock()
	for _, r := range p.Data {
		switch {
		case r == 0x7f || r == 0x08:
			if n := len(t.typed); n > 0 {
				_, size := utf8.DecodeLastRuneInString(t.typed)
				t.typed = t.typed[:n-size]
			}
		case r == '\r' || r == 0x1b || r == 3:
			t.typed = ""
		case r >= 0x20 && utf8.RuneCountInString(t.typed) < t.cols-10:
			t.typed += string(r)
		}
	}
	t.mu.Unlock()
	t.send(t.promptLine())
	return struct{}{}, nil
}

// termConfirm accepts the hub's confirmation of an open stream; the
// invented terminal does not end unconfirmed ones.
func (m *devMachine) termConfirm(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	p, err := devDecode[proto.TerminalStreamParams](params)
	if err != nil {
		return nil, err
	}
	if _, ok := devTerms.Load(p.Stream); !ok {
		return nil, proto.Errorf(proto.CodeNotFound, "no such terminal")
	}
	return struct{}{}, nil
}

func (m *devMachine) termClose(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	p, err := devDecode[proto.TerminalStreamParams](params)
	if err != nil {
		return nil, err
	}
	if v, ok := devTerms.LoadAndDelete(p.Stream); ok {
		v.(*devTerm).cancel()
	}
	return struct{}{}, nil
}

func (t *devTerm) send(s string) {
	_ = t.conn.Notify(proto.NotifyTerminalData, proto.TerminalData{Stream: t.id, Data: []byte(s)})
}

func (t *devTerm) run(ctx context.Context) {
	defer devTerms.Delete(t.id)
	t.send(t.screen())
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	start := time.Now()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		t.send(t.spinner(i, time.Since(start)))
	}
}

// pad cuts or fills s (plain text) to n columns.
func devPad(s string, n int) string {
	if c := utf8.RuneCountInString(s); c < n {
		return s + strings.Repeat(" ", n-c)
	}
	return string([]rune(s)[:n])
}

func devAt(row int) string { return fmt.Sprintf("\x1b[%d;1H", row) }

func (t *devTerm) screen() string {
	w := t.cols
	var b strings.Builder
	b.WriteString("\x1b[?25l\x1b[H\x1b[2J")
	boxLine := func(row int, text, style string) {
		b.WriteString(devAt(row) + ansiOrange + "│" + ansiReset + " " + style + devPad(text, w-4) + ansiReset + " " + ansiOrange + "│" + ansiReset)
	}
	b.WriteString(devAt(1) + ansiOrange + "╭" + strings.Repeat("─", w-2) + "╮" + ansiReset)
	boxLine(2, "✻ Welcome to "+t.title+" (invented by the development mode)", ansiBold)
	boxLine(3, "  /help for help, /status for your current setup", ansiDim)
	boxLine(4, "  cwd: "+t.cwd, ansiDim)
	b.WriteString(devAt(5) + ansiOrange + "╰" + strings.Repeat("─", w-2) + "╯" + ansiReset)
	b.WriteString(devAt(7) + ansiDim + "> Add a migration that moves sessions into their own table, and update the store." + ansiReset)
	b.WriteString(devAt(9) + ansiGreen + "●" + ansiReset + " " + ansiBold + "Read" + ansiReset + "(internal/store/schema.sql)")
	b.WriteString(devAt(10) + ansiDim + "  ⎿  Read 64 lines" + ansiReset)
	b.WriteString(devAt(11) + ansiGreen + "●" + ansiReset + " " + ansiBold + "Update" + ansiReset + "(internal/store/schema.sql)")
	b.WriteString(devAt(12) + ansiDim + "  ⎿  Updated internal/store/schema.sql with 16 additions and 2 removals" + ansiReset)
	del := "\x1b[48;2;92;30;36m\x1b[38;2;255;220;220m"
	add := "\x1b[48;2;28;74;40m\x1b[38;2;220;255;220m"
	b.WriteString(devAt(13) + ansiDim + "      14 " + ansiReset + del + devPad("-CREATE INDEX events_session ON events (machine, session);", w-12) + ansiReset)
	b.WriteString(devAt(14) + ansiDim + "      14 " + ansiReset + add + devPad("+CREATE TABLE sessions (", w-12) + ansiReset)
	b.WriteString(devAt(15) + ansiDim + "      15 " + ansiReset + add + devPad("+  machine   TEXT NOT NULL,", w-12) + ansiReset)
	b.WriteString(devAt(16) + ansiDim + "      16 " + ansiReset + add + devPad("+  session   TEXT NOT NULL,", w-12) + ansiReset)
	b.WriteString(devAt(17) + ansiGreen + "●" + ansiReset + " " + ansiBold + "Bash" + ansiReset + "(go test ./internal/store/)")
	b.WriteString(devAt(18) + ansiDim + "  ⎿  ok   example.org/api/internal/store   0.412s" + ansiReset)
	b.WriteString(t.spinner(0, 0))
	b.WriteString(devAt(22) + ansiDim + "╭" + strings.Repeat("─", w-2) + "╮" + ansiReset)
	b.WriteString(t.promptLine())
	b.WriteString(devAt(24) + ansiDim + "╰" + strings.Repeat("─", w-2) + "╯" + ansiReset)
	b.WriteString(devAt(25) + ansiDim + "  ? for shortcuts" + ansiReset)
	return b.String()
}

var spinnerFrames = []string{"✶", "✸", "✹", "✺", "✹", "✷"}

func (t *devTerm) spinner(i int, d time.Duration) string {
	f := spinnerFrames[i%len(spinnerFrames)]
	return devAt(20) + "\x1b[2K" + ansiOrange + f + " Migrating… " + ansiReset + ansiDim +
		fmt.Sprintf("(%ds · ↑ %.1fk tokens · esc to interrupt)", 37+int(d.Seconds()), 1.2+d.Seconds()/50) + ansiReset
}

func (t *devTerm) promptLine() string {
	t.mu.Lock()
	typed := t.typed
	t.mu.Unlock()
	hint := ""
	if typed == "" && !t.control {
		hint = ansiDim + "(observing; take control to type)" + ansiReset
	}
	return devAt(23) + ansiDim + "│" + ansiReset + " > " + typed + "\x1b[7m \x1b[27m" + hint +
		fmt.Sprintf("\x1b[23;%dH", t.cols) + ansiDim + "│" + ansiReset
}
