// Command fakeagent stands in for a coding agent in tests. It is installed
// into the toolbox through a manifest with install.method = "local" and
// started through its shim like a real harness.
//
//	fakeagent --version              print a version
//	fakeagent [flags] -p '<command>' run one command and exit
//	fakeagent [flags]                print a prompt, read commands until "exit"
//	fakeagent --cat                  echo every input byte unchanged (raw mode)
//	fakeagent --hostile-helper <mode> <socket> <seconds>
//	                                 stand in for a misbehaving box agent
//
// Flags for tests (work order C09 section 7): --no-end (no session.end
// event), --ignore-first-submit (the first submit key does nothing),
// --never-submit (the submit key never submits), --busy <seconds> (every
// turn stays working that long first), --winsize (print the window size
// at start and whenever it changes).
//
// The hostile helper (work order C08, integration test 5) listens on the
// socket for that many seconds and answers the host's daemon with garbage
// instead of yamux (mode garbage), or with yamux and then a box.hello
// answer above the 1 MiB line limit (oversized) or malformed JSON
// (malformed).
//
// Commands: env [NAME…], say <text>, write <path> <text>, sh <command>,
// sleep <seconds>, touch-state <relative path>, ask <question> (waits for
// the next submitted line as the answer), askhook <question> [| option…]
// (asks through the hook receiver, which may return the answer; otherwise
// as ask), die [code] (exits at once, without an end event), exit.
//
// On a terminal it behaves like a TUI agent: raw mode, its own line
// editing, bracketed paste (a paste followed by the submit key, Enter, is
// one prompt), Escape or C-c ends the current turn. At start it sends the
// Program Status Protocol query (OSC 7501) followed by a device-attributes
// query; when the OSC answer comes first, it reports its states through
// OSC 7501 as well. Without a terminal it reads one command per line.
//
// Like a real harness it reports what it does through devc's hook
// receiver (`devc box hook fake <event>`, JSON {"type", "data"} on stdin):
// a harness and an idle state event at start; for each input line a prompt
// and "working", then a message and "done"; for ask a question and
// "blocked"; for an interrupted turn "idle"; session.end at exit, unless
// --no-end is given. The receiver is FAKEAGENT_HOOK, by default
// /opt/devc/bin/devc; hook failures are ignored.
//
// The agent's own "config directory" comes from FAKEAGENT_DIR, which its
// manifest sets through home.env, so tests can tell the home strategies
// apart. Values of variables that look like secrets are printed as their
// SHA-256, never in clear.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const version = "fakeagent 1.0.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// options are the test flags.
type options struct {
	noEnd       bool
	ignoreFirst bool
	neverSubmit bool
	busy        time.Duration
	winsize     bool
}

// noEnd suppresses the session.end event (--no-end).
var noEnd bool

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o options
	noEnd = false
flags:
	for len(args) > 0 {
		switch args[0] {
		case "--no-end":
			o.noEnd = true
		case "--ignore-first-submit":
			o.ignoreFirst = true
		case "--never-submit":
			o.neverSubmit = true
		case "--winsize":
			o.winsize = true
		case "--busy":
			if len(args) < 2 {
				fmt.Fprintln(stderr, "--busy needs seconds")
				return 2
			}
			s, err := strconv.ParseFloat(args[1], 64)
			if err != nil || s < 0 {
				fmt.Fprintln(stderr, "--busy needs seconds")
				return 2
			}
			o.busy = time.Duration(s * float64(time.Second))
			args = args[1:]
		default:
			break flags
		}
		args = args[1:]
	}
	noEnd = o.noEnd
	switch {
	case len(args) == 1 && args[0] == "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case len(args) == 1 && args[0] == "--cat":
		return catMode(stdin, stdout)
	case len(args) == 2 && args[0] == "-p":
		a := &agent{o: o, out: &lockedWriter{w: stdout}, errw: stderr}
		a.started()
		code, done := a.turn(context.Background(), args[1], nil)
		if !done {
			end("exit")
		}
		return code
	case len(args) == 0:
		return interactive(o, stdin, stdout, stderr)
	case len(args) == 4 && args[0] == "--hostile-helper":
		secs, err := strconv.Atoi(args[3])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if err := hostileHelper(args[1], args[2], time.Duration(secs)*time.Second); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stderr, "usage: fakeagent [flags] [--version | --cat | -p <command>]\n")
	return 2
}

// hookOut runs devc's hook receiver with one event and returns its output.
func hookOut(event string, in []byte) []byte {
	exe := os.Getenv("FAKEAGENT_HOOK")
	if exe == "" {
		exe = "/opt/devc/bin/devc"
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm()&0o111 == 0 {
		return nil
	}
	cmd := exec.Command(exe, "box", "hook", "fake", event)
	cmd.Stdin = bytes.NewReader(in)
	out, _ := cmd.Output()
	return out
}

// hook reports one event through devc's hook receiver.
func hook(typ string, data any) {
	in, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	hookOut(typ, in)
}

func end(reason string) {
	if !noEnd {
		hook("session.end", map[string]any{"reason": reason})
	}
}

func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// lockedWriter serialises writes from the input echo and the turn, and
// turns "\n" into "\r\n" on a terminal in raw mode.
type lockedWriter struct {
	mu   sync.Mutex
	w    io.Writer
	crlf bool
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := p
	if l.crlf {
		q = bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))
	}
	if _, err := l.w.Write(q); err != nil {
		return 0, err
	}
	return len(p), nil
}

// raw writes bytes unchanged.
func (l *lockedWriter) raw(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(p)
}

// agent is one interactive session.
type agent struct {
	o    options
	out  *lockedWriter
	errw io.Writer
	osc  bool
	// reported holds lines submitted during a turn whose prompt was
	// reported at once (on a terminal, like a TUI that takes input while it
	// works); their turn does not report it again.
	reported map[string]bool
}

func reportPrompt(line string) {
	sum := sha256.Sum256([]byte(line))
	hook("prompt", map[string]any{"sha256": hex.EncodeToString(sum[:]), "length": utf8.RuneCountInString(line), "text": firstRunes(line, 2000)})
}

func (a *agent) started() {
	cwd, _ := os.Getwd()
	a.status("idle", "", "")
	hook("harness", map[string]any{"id": fmt.Sprintf("fake-%d", os.Getpid()), "cwd": cwd, "source": "startup"})
	hook("state", map[string]any{"state": "idle"})
}

// status reports a state through OSC 7501 when the terminal answered the
// query.
func (a *agent) status(state, kind, msg string) {
	if !a.osc {
		return
	}
	body := "state=" + state + ":app=fakeagent"
	if kind != "" {
		body += ":kind=" + kind
	}
	if msg = oscText(msg); msg != "" {
		body += ":msg=" + base64.StdEncoding.EncodeToString([]byte(msg))
	}
	a.out.raw([]byte("\x1b]7501;" + body + "\x07"))
}

// oscText makes a message fit the protocol: one line without control
// characters, at most 200 characters.
func oscText(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return firstRunes(strings.TrimSpace(s), 200)
}

func (a *agent) state(state, kind, msg string) {
	a.status(state, kind, msg)
	d := map[string]any{"state": state}
	if kind != "" {
		d["kind"] = kind
	}
	hook("state", d)
}

// turn handles one submitted line as a harness turn: prompt and "working",
// the command, then the message and "done". ask also reports a question,
// "blocked", and takes the next submitted line from answers. A cancelled
// context (the interrupt key) ends the turn as "idle".
func (a *agent) turn(ctx context.Context, line string, ask func(context.Context) (string, bool)) (int, bool) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	switch verb {
	case "exit":
		end("exit")
		return 0, true
	case "die":
		code, _ := strconv.Atoi(strings.TrimSpace(rest))
		return code, true
	}
	if !a.reported[line] {
		reportPrompt(line)
	}
	delete(a.reported, line)
	a.state("working", "", "")
	var buf bytes.Buffer
	out, errw := io.MultiWriter(a.out, &buf), io.MultiWriter(a.errw, &buf)
	code := 0
	interrupted := func() (int, bool) {
		fmt.Fprintln(a.out, "interrupted")
		hook("message", map[string]any{"text": "interrupted"})
		a.state("idle", "", "")
		return 130, false
	}
	if a.o.busy > 0 {
		select {
		case <-time.After(a.o.busy):
		case <-ctx.Done():
			return interrupted()
		}
	}
	switch verb {
	case "ask", "askhook":
		question, opts := strings.TrimSpace(rest), []string(nil)
		if q, o, ok := strings.Cut(question, "|"); ok {
			question = strings.TrimSpace(q)
			for _, s := range strings.Split(o, "|") {
				opts = append(opts, strings.TrimSpace(s))
			}
		}
		qid := fmt.Sprintf("q%d", time.Now().UnixNano())
		answer, ok := "", false
		if verb == "askhook" {
			in, _ := json.Marshal(map[string]any{"id": qid, "text": firstRunes(question, 4000), "options": opts})
			a.status("blocked", "question", question)
			var r struct {
				Choice int    `json:"choice"`
				Text   string `json:"text"`
			}
			if b := hookOut("ask", in); json.Unmarshal(b, &r) == nil && (r.Choice > 0 || r.Text != "") {
				answer, ok = r.Text, true
				if r.Choice > 0 && r.Choice <= len(opts) {
					answer = opts[r.Choice-1]
				}
				a.status("working", "", "")
				fmt.Fprintf(out, "answer from hook: %s\n", answer)
			}
		}
		if !ok {
			fmt.Fprintf(a.out, "question: %s\n", question)
			if verb == "ask" {
				hook("question", map[string]any{"id": qid, "text": firstRunes(question, 4000), "options": opts})
				a.state("blocked", "question", question)
			}
			if ask != nil {
				fmt.Fprint(a.out, "answer> ")
				answer, ok = ask(ctx)
				if ctx.Err() != nil {
					return interrupted()
				}
			}
			if !ok {
				return 0, false
			}
			a.state("working", "", "")
			fmt.Fprintf(out, "answer: %s\n", answer)
		}
	default:
		code, _ = command(ctx, line, out, errw)
		if ctx.Err() != nil {
			return interrupted()
		}
	}
	msg := strings.TrimSpace(buf.String())
	if msg == "" {
		msg = "ok: " + verb
	}
	hook("message", map[string]any{"text": firstRunes(msg, 4000)})
	a.state("done", "", msg)
	return code, false
}

// Input events.
type evKind int

const (
	evSubmit evKind = iota
	evInterrupt
	evEOF
	evOSC
	evDA
)

type event struct {
	kind evKind
	text string
}

// lineReader reads one command per line (no terminal).
func lineReader(in io.Reader, events chan<- event) {
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 32<<10)
	for {
		n, err := in.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			events <- event{kind: evSubmit, text: strings.TrimSpace(string(buf[:i]))}
			buf = buf[i+1:]
		}
		if err != nil {
			if len(bytes.TrimSpace(buf)) > 0 {
				events <- event{kind: evSubmit, text: strings.TrimSpace(string(buf))}
			}
			events <- event{kind: evEOF}
			return
		}
	}
}

// termReader is the line editor on a raw terminal: typed characters and
// bracketed pastes collect in a line, the submit key (Enter) submits it,
// Escape and C-c interrupt, answers to the start-up queries are reported.
type termReader struct {
	a       *agent
	events  chan<- event
	line    []byte
	paste   []byte
	inPaste bool
	pending []byte // an escape sequence split across reads
	ignored bool
}

func (t *termReader) echo(b []byte) { _, _ = t.a.out.Write(b) }

func (t *termReader) submit() {
	switch {
	case t.a.o.neverSubmit:
		return
	case t.a.o.ignoreFirst && !t.ignored:
		t.ignored = true
		return
	}
	t.echo([]byte("\n"))
	t.events <- event{kind: evSubmit, text: string(t.line)}
	t.line = t.line[:0]
}

// feed handles one chunk of terminal input.
func (t *termReader) feed(b []byte) {
	b = append(t.pending, b...)
	t.pending = nil
	for i := 0; i < len(b); i++ {
		c := b[i]
		if t.inPaste {
			if j := bytes.Index(b[i:], []byte("\x1b[201~")); j >= 0 {
				t.paste = append(t.paste, b[i:i+j]...)
				t.inPaste = false
				t.line = append(t.line, t.paste...)
				t.echo(t.paste)
				t.paste = nil
				i += j + len("\x1b[201~") - 1
				continue
			}
			// Keep a possible start of the end marker for the next read.
			keep := 0
			for k := 1; k < len("\x1b[201~") && k <= len(b)-i; k++ {
				if bytes.HasPrefix([]byte("\x1b[201~"), b[len(b)-k:]) {
					keep = k
				}
			}
			t.paste = append(t.paste, b[i:len(b)-keep]...)
			t.pending = append(t.pending, b[len(b)-keep:]...)
			return
		}
		switch {
		case c == 0x1b:
			n, ok := t.escape(b[i:])
			if !ok {
				t.pending = append(t.pending, b[i:]...)
				return
			}
			i += n - 1
		case c == '\r' || c == '\n':
			t.submit()
		case c == 0x7f || c == 0x08:
			if len(t.line) > 0 {
				_, size := utf8.DecodeLastRune(t.line)
				t.line = t.line[:len(t.line)-size]
				t.echo([]byte("\b \b"))
			}
		case c == 0x03:
			t.events <- event{kind: evInterrupt}
		case c == 0x04:
			if len(t.line) == 0 {
				t.events <- event{kind: evEOF}
			}
		case c >= 0x20:
			t.line = append(t.line, c)
			t.echo([]byte{c})
		}
	}
}

// escape handles an escape sequence at the start of b; it returns how many
// bytes it took, or false when b ends inside a sequence.
func (t *termReader) escape(b []byte) (int, bool) {
	if len(b) == 1 {
		// A lone escape at the end of a read is the Escape key.
		t.events <- event{kind: evInterrupt}
		return 1, true
	}
	switch b[1] {
	case '[':
		// CSI: parameters, then a final byte in 0x40-0x7e.
		for j := 2; j < len(b); j++ {
			if b[j] >= 0x40 && b[j] <= 0x7e {
				seq := string(b[:j+1])
				switch {
				case seq == "\x1b[200~":
					t.inPaste, t.paste = true, nil
				case b[j] == 'c' && strings.HasPrefix(seq, "\x1b[?"):
					t.events <- event{kind: evDA}
				}
				return j + 1, true
			}
		}
		return 0, false
	case ']':
		// OSC, up to BEL or ESC \.
		for j := 2; j < len(b); j++ {
			if b[j] == 0x07 || (b[j] == '\\' && b[j-1] == 0x1b) {
				body := string(b[2:j])
				body = strings.TrimSuffix(body, "\x1b")
				if strings.HasPrefix(body, "7501;?") {
					t.events <- event{kind: evOSC, text: body}
				}
				return j + 1, true
			}
		}
		return 0, false
	}
	return 2, true // Alt+key and the like: ignored
}

func (t *termReader) run(in io.Reader) {
	chunk := make([]byte, 32<<10)
	for {
		n, err := in.Read(chunk)
		if n > 0 {
			t.feed(chunk[:n])
		}
		if err != nil {
			t.events <- event{kind: evEOF}
			return
		}
	}
}

// makeRaw puts fd into raw mode and returns the previous settings.
func makeRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	t := *old
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &t); err != nil {
		return nil, err
	}
	return old, nil
}

func termFd(r io.Reader) (int, bool) {
	f, ok := r.(*os.File)
	if !ok {
		return 0, false
	}
	fd := int(f.Fd())
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return fd, err == nil
}

// watchSize prints the window size at start and on every change.
func watchSize(fd int, out *lockedWriter) func() {
	print := func() {
		if ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err == nil {
			out.raw([]byte(fmt.Sprintf("\r\n<winsize %dx%d>\r\n", ws.Row, ws.Col)))
		}
	}
	print()
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				print()
			case <-done:
				return
			}
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}

// catMode echoes every input byte unchanged; the window size is printed
// when it changes.
func catMode(stdin io.Reader, stdout io.Writer) int {
	out := &lockedWriter{w: stdout}
	if fd, ok := termFd(stdin); ok {
		if old, err := makeRaw(fd); err == nil {
			defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
		}
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGWINCH)
		go func() {
			for range ch {
				if ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err == nil {
					out.raw([]byte(fmt.Sprintf("<winsize %dx%d>", ws.Row, ws.Col)))
				}
			}
		}()
	}
	out.raw([]byte("<cat ready>"))
	buf := make([]byte, 32<<10)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			out.raw(buf[:n])
		}
		if err != nil {
			return 0
		}
	}
}

func interactive(o options, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &agent{o: o, out: &lockedWriter{w: stdout}, errw: stderr, reported: map[string]bool{}}
	events := make(chan event, 64)
	fd, isTerm := termFd(stdin)
	if isTerm {
		if old, err := makeRaw(fd); err == nil {
			defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
			a.out.crlf = true
		}
		tr := &termReader{a: a, events: events}
		go tr.run(stdin)
		// Bracketed paste, then the status protocol query followed by a
		// device-attributes query that every terminal answers.
		a.out.raw([]byte("\x1b[?2004h\x1b]7501;?\x07\x1b[c"))
		a.osc = detect(events)
		if o.winsize {
			defer watchSize(fd, a.out)()
		}
		defer a.out.raw([]byte("\x1b[?2004l"))
	} else {
		go lineReader(stdin, events)
	}
	a.started()

	type result struct {
		code int
		exit bool
	}
	var queue []string
	var cancel context.CancelFunc
	var turnDone chan result
	// A turn that asks takes the next submitted line: one already queued,
	// or the next to come.
	asks := make(chan chan string)
	var pendingAsk chan string
	ask := func(ctx context.Context) (string, bool) {
		reply := make(chan string, 1)
		select {
		case asks <- reply:
		case <-ctx.Done():
			return "", false
		}
		select {
		case s, ok := <-reply:
			return s, ok
		case <-ctx.Done():
			return "", false
		}
	}
	eof := false
	code := 0
	startTurn := func(line string) {
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		turnDone = make(chan result, 1)
		done := turnDone
		go func() {
			code, exit := a.turn(ctx, line, ask)
			done <- result{code, exit}
		}()
	}
	prompt := func() { fmt.Fprint(a.out, "fake> ") }
	prompt()
	for {
		select {
		case reply := <-asks:
			switch {
			case len(queue) > 0:
				delete(a.reported, queue[0])
				reply <- queue[0]
				queue = queue[1:]
			case eof:
				close(reply)
			default:
				pendingAsk = reply
			}
		case ev := <-events:
			switch ev.kind {
			case evSubmit:
				switch {
				case pendingAsk != nil:
					pendingAsk <- ev.text
					pendingAsk = nil
				case ev.text == "":
					if turnDone == nil {
						prompt()
					}
				case turnDone != nil:
					queue = append(queue, ev.text)
					if isTerm {
						reportPrompt(ev.text)
						a.reported[ev.text] = true
					}
				default:
					startTurn(ev.text)
				}
			case evInterrupt:
				if cancel != nil {
					cancel()
				}
			case evEOF:
				eof = true
				if turnDone == nil {
					fmt.Fprintln(a.out)
					end("eof")
					return code
				}
				if pendingAsk != nil {
					close(pendingAsk)
					pendingAsk = nil
				}
			}
		case r := <-turnDone:
			cancel()
			turnDone, cancel, pendingAsk = nil, nil, nil
			code = r.code
			if r.exit {
				return code
			}
			if len(queue) > 0 {
				next := queue[0]
				queue = queue[1:]
				startTurn(next)
				continue
			}
			if eof {
				fmt.Fprintln(a.out)
				end("eof")
				return code
			}
			prompt()
		}
	}
}

// detect waits for the answers to the start-up queries: the OSC 7501
// answer before the device attributes means the protocol is supported.
func detect(events chan event) bool {
	timeout := time.After(2 * time.Second)
	var held []event
	defer func() {
		for _, ev := range held {
			events <- ev
		}
	}()
	for {
		select {
		case ev := <-events:
			switch ev.kind {
			case evOSC:
				return true
			case evDA:
				return false
			default:
				held = append(held, ev)
			}
		case <-timeout:
			return false
		}
	}
}

// command runs one command line and returns its exit code and whether the
// session should end.
func command(ctx context.Context, line string, stdout, stderr io.Writer) (int, bool) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	rest = strings.TrimSpace(rest)
	switch verb {
	case "exit":
		return 0, true
	case "say":
		fmt.Fprintln(stdout, rest)
	case "env":
		printEnv(strings.Fields(rest), stdout)
	case "write":
		p, text, ok := strings.Cut(rest, " ")
		if !ok || p == "" {
			fmt.Fprintln(stderr, "usage: write <path> <text>")
			return 2, false
		}
		if err := os.WriteFile(p, []byte(text+"\n"), 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, false
		}
	case "sh":
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", rest)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), false
			}
			fmt.Fprintln(stderr, err)
			return 1, false
		}
	case "sleep":
		s, err := strconv.ParseFloat(rest, 64)
		if err != nil || s < 0 {
			fmt.Fprintln(stderr, "usage: sleep <seconds>")
			return 2, false
		}
		select {
		case <-time.After(time.Duration(s * float64(time.Second))):
		case <-ctx.Done():
		}
	case "touch-state":
		dir := os.Getenv("FAKEAGENT_DIR")
		if dir == "" || rest == "" || filepath.IsAbs(rest) || strings.Contains(rest, "..") {
			fmt.Fprintln(stderr, "usage: touch-state <relative path> (FAKEAGENT_DIR must be set)")
			return 2, false
		}
		p := filepath.Join(dir, rest)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, false
		}
		if err := os.WriteFile(p, []byte(os.Getenv("DEVC_PROJECT")+"\n"), 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, false
		}
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", verb)
		return 2, false
	}
	return 0, false
}

// defaultEnv is what "env" prints without arguments.
var defaultEnv = []string{"DEVC_SESSION", "DEVC_HARNESS", "DEVC_WORKTREE", "DEVC_REALM", "DEVC_PROJECT", "FAKEAGENT_DIR", "HOME", "FAKE_TOKEN"}

func secretLike(name string) bool {
	for _, s := range strings.Split(os.Getenv("FAKEAGENT_SECRETS"), ",") {
		if s != "" && s == name {
			return true
		}
	}
	u := strings.ToUpper(name)
	for _, w := range []string{"TOKEN", "SECRET", "KEY", "PASSWORD", "GRANT"} {
		if strings.Contains(u, w) {
			return true
		}
	}
	return false
}

func printEnv(names []string, w io.Writer) {
	if len(names) == 0 {
		names = defaultEnv
		if exe, err := os.Executable(); err == nil {
			fmt.Fprintf(w, "FAKEAGENT_EXE=%s\n", exe)
		}
	}
	for _, n := range names {
		v, ok := os.LookupEnv(n)
		switch {
		case !ok:
			fmt.Fprintf(w, "%s=<unset>\n", n)
		case secretLike(n):
			sum := sha256.Sum256([]byte(v))
			fmt.Fprintf(w, "%s=sha256:%s\n", n, hex.EncodeToString(sum[:]))
		default:
			fmt.Fprintf(w, "%s=%s\n", n, v)
		}
	}
}
