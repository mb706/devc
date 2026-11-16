package daemon

// Terminal streams (work order C12 section 3): terminal.open runs the
// box's tmux client for one session's window on a pseudo-terminal of the
// daemon's (`podman exec -t` with it as controlling terminal) and relays
// its output as terminal.data notifications on the connection that opened
// it. The client attaches through its own grouped session with the client
// flag ignore-size (tmux.Observe): it never resizes the pane; the
// pseudo-terminal gets the window's size, and follows it when the window
// changes. An observing stream cannot type because the daemon, the only
// writer of its pseudo-terminal, refuses terminal.input for it (tmux's
// read-only flag is not used: it makes tmux refuse the daemon's own
// send-keys while the observer is attached). Control mode accepts
// terminal.input; it is refused unless [terminal] control = true in
// config.toml (the hub has its own switch). A stream ends when the session
// or its window ends, when the client exits, when the connection closes,
// or with terminal.close.
//
// Security: the stream is box-scoped (one session's window in its own
// box); input reaches only that pane, and only in control mode; the stream
// ID is the caller's but bound to its connection, so another connection
// cannot act on it. Commands are argument vectors from / (rule 3).

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/ptywrap"
	"github.com/mb706/devc/internal/tmux"
)

// Limits and intervals of terminal streams (variables for tests).
var (
	termPerConn  = 4
	termTotal    = 16
	termSizePoll = 5 * time.Second
	termWatch    = time.Second
)

const (
	// termMaxLife bounds one stream; the web app offers to reconnect.
	termMaxLife   = 12 * time.Hour
	termStopWait  = 3 * time.Second
	termExecLimit = 10 * time.Second
	termWriteWait = 5 * time.Second
)

// termEnv is the environment of the tmux client: xterm.js emulates
// xterm-256color with 24-bit colour.
var termEnv = []string{"TERM=xterm-256color", "COLORTERM=truecolor", "TERMINFO_DIRS=" + tmux.TerminfoDirs}

type termManager struct {
	d       *Daemon
	mu      sync.Mutex
	streams map[string]*termStream
}

type termStream struct {
	m       *termManager
	id      string
	session string
	ref     names.ProjectRef
	window  string
	client  string
	control bool
	conn    *proto.Conn
	eng     engine.Engine
	ptmx    *os.File
	cancel  context.CancelFunc
	// execDone is closed when the engine process has exited.
	execDone chan struct{}
	once     sync.Once
	wmu      sync.Mutex
	cols     int
	rows     int
}

func newTermManager(d *Daemon) *termManager {
	return &termManager{d: d, streams: map[string]*termStream{}}
}

func (m *termManager) get(id string, c *proto.Conn) *termStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.streams[id]
	if st == nil || st.conn != c {
		return nil
	}
	return st
}

// count returns the streams of c and of everyone.
func (m *termManager) count(c *proto.Conn) (mine, all int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.streams {
		if st.conn == c {
			mine++
		}
	}
	return mine, len(m.streams)
}

func (m *termManager) open(ctx context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
	d := m.d
	var p proto.TerminalOpenParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if !proto.ValidStreamID(p.Stream) {
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid stream id")
	}
	want, err := parseRefParam(p.Ref)
	if err != nil {
		return nil, err
	}
	if err := checkSession(p.Session); err != nil {
		return nil, err
	}
	if p.Mode == "" {
		p.Mode = proto.TerminalObserve
	}
	if p.Mode != proto.TerminalObserve && p.Mode != proto.TerminalControl {
		return nil, proto.Errorf(proto.CodeInvalidParams, "mode must be %s or %s", proto.TerminalObserve, proto.TerminalControl)
	}
	control := p.Mode == proto.TerminalControl
	d.fresh()
	e := d.current()
	if control && !e.cfg.Global.Terminal.Control {
		return nil, proto.Errorf(proto.CodeRefused, "terminal control is off on this machine ([terminal] control = true in config.toml turns it on)")
	}
	v, ref, reason := d.cmd.lookup(ctx, want, p.Session)
	switch {
	case reason != "":
		return nil, proto.Errorf(proto.CodeNotFound, "%s", reason)
	case !v.Live:
		return nil, proto.Errorf(proto.CodeRefused, "the session has ended")
	case d.boxState(ref) != "running":
		return nil, proto.Errorf(proto.CodeRefused, "the box %s is not running", names.BoxName(ref))
	}
	window := d.liveWindow(ctx, ref, p.Session)
	if !tmux.ValidWindowID(window) {
		return nil, proto.Errorf(proto.CodeRefused, "the session has no tmux window")
	}
	if mine, all := m.count(c); mine >= termPerConn || all >= termTotal {
		return nil, proto.Errorf(proto.CodeBusy, "too many terminals are open")
	}
	cols, rows, err := m.windowSize(ctx, e.eng, ref, window)
	if err != nil {
		return nil, proto.Errorf(proto.CodeRefused, "the window's size cannot be read: %s", proto.Printable(proto.Truncate(err.Error(), 200)))
	}
	ptmx, tty, err := ptywrap.OpenPTY()
	if err != nil {
		return nil, err
	}
	if err := ptywrap.SetSize(ptmx, cols, rows); err != nil {
		ptmx.Close()
		tty.Close()
		return nil, err
	}
	client := boxside.ClientName()
	argv, err := tmux.Observe(client, window, control)
	if err != nil {
		ptmx.Close()
		tty.Close()
		return nil, err
	}
	sctx, cancel := context.WithCancel(d.ctx)
	st := &termStream{m: m, id: p.Stream, session: p.Session, ref: ref, window: window, client: client, control: control,
		conn: c, eng: e.eng, ptmx: ptmx, cancel: cancel, execDone: make(chan struct{}), cols: cols, rows: rows}
	m.mu.Lock()
	if _, dup := m.streams[p.Stream]; dup {
		m.mu.Unlock()
		cancel()
		ptmx.Close()
		tty.Close()
		return nil, proto.Errorf(proto.CodeRefused, "the stream id is in use")
	}
	m.streams[p.Stream] = st
	m.mu.Unlock()
	d.log.Info("terminal opened", "session", p.Session, "box", names.BoxName(ref), "mode", p.Mode, "size", [2]int{cols, rows})

	d.goSafe("terminal exec", func() {
		defer close(st.execDone)
		_, err := st.eng.ExecInteractive(sctx, engine.ExecOptions{Container: names.BoxName(ref), Interactive: true, TTY: true,
			Env: termEnv, Workdir: "/", Cmd: tmux.Argv(argv), Stdin: tty, Stdout: tty, Stderr: tty, Ctty: true, Timeout: termMaxLife})
		tty.Close()
		if err != nil && sctx.Err() == nil {
			d.log.Warn("terminal client", "session", p.Session, "err", err)
		}
		go st.end("the terminal client exited")
	})
	d.goSafe("terminal reader", st.read)
	d.goSafe("terminal watch", func() { st.watch(sctx) })
	return proto.TerminalOpenResult{Stream: p.Stream, Mode: p.Mode, Cols: cols, Rows: rows}, nil
}

// windowSize reads a window's size in the box.
func (m *termManager) windowSize(ctx context.Context, eng engine.Engine, ref names.ProjectRef, window string) (int, int, error) {
	argv, err := tmux.WindowSize(window)
	if err != nil {
		return 0, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, termExecLimit)
	defer cancel()
	res, err := eng.Exec(ctx, engine.ExecOptions{Container: names.BoxName(ref), Workdir: "/", Cmd: tmux.Argv(argv), Timeout: termExecLimit})
	if err != nil {
		return 0, 0, err
	}
	cols, rows, err := tmux.ParseWindowSize(string(res.Stdout))
	if err != nil {
		return 0, 0, err
	}
	return min(max(cols, proto.MinTermCols), proto.MaxTermCols), min(max(rows, proto.MinTermRows), proto.MaxTermRows), nil
}

// liveWindow returns the tmux window of a live session: asked in the box
// when its channel is connected (a session started a moment ago is not in
// the last reconciliation yet), else as the last reconciliation saw it.
func (d *Daemon) liveWindow(ctx context.Context, ref names.ProjectRef, sid string) string {
	if w := d.worker(ref); w != nil && w.connected() {
		var cl proto.BoxClients
		cctx, cancel := context.WithTimeout(ctx, termExecLimit)
		err := w.call(cctx, proto.MethodBoxClients, proto.BoxInputParams{Session: sid}, &cl)
		cancel()
		if err == nil && tmux.ValidWindowID(cl.Window) {
			return cl.Window
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.windows[ref][sid]
}

// read relays the client's output until the pseudo-terminal closes.
func (st *termStream) read() {
	buf := make([]byte, proto.MaxTerminalData)
	for {
		n, err := st.ptmx.Read(buf)
		if n > 0 {
			if nerr := st.conn.Notify(proto.NotifyTerminalData, proto.TerminalData{Stream: st.id, Data: append([]byte(nil), buf[:n]...)}); nerr != nil {
				go st.end("the connection closed")
				return
			}
		}
		if err != nil {
			go st.end("the terminal closed")
			return
		}
	}
}

// watch ends the stream with its session or connection, and follows the
// window's size.
func (st *termStream) watch(ctx context.Context) {
	tick := time.NewTicker(termWatch)
	defer tick.Stop()
	lastSize := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-st.conn.Done():
			st.end("the connection closed")
			return
		case <-tick.C:
		}
		e, ok := st.m.d.hub.lookup(keyOf(st.ref, st.session))
		if !ok || !e.view.Live {
			st.end("the session ended")
			return
		}
		if time.Since(lastSize) < termSizePoll {
			continue
		}
		lastSize = time.Now()
		cols, rows, err := st.m.windowSize(ctx, st.eng, st.ref, st.window)
		if err != nil || (cols == st.cols && rows == st.rows) {
			continue
		}
		st.cols, st.rows = cols, rows
		if err := ptywrap.SetSize(st.ptmx, cols, rows); err == nil {
			_ = st.conn.Notify(proto.NotifyTerminalSize, proto.TerminalSize{Stream: st.id, Cols: cols, Rows: rows})
		}
	}
}

// end stops the stream once: the grouped session is killed (the client
// detaches and exits), the engine process is given a moment and then
// stopped, and the connection is told why.
func (st *termStream) end(reason string) {
	st.once.Do(func() {
		m := st.m
		m.mu.Lock()
		delete(m.streams, st.id)
		m.mu.Unlock()
		if argv, err := tmux.KillSession(st.client); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), termExecLimit)
			_, _ = st.eng.Exec(ctx, engine.ExecOptions{Container: names.BoxName(st.ref), Workdir: "/", Cmd: tmux.Argv(argv), Timeout: termExecLimit})
			cancel()
		}
		select {
		case <-st.execDone:
		case <-time.After(termStopWait):
		}
		st.cancel()
		st.ptmx.Close()
		_ = st.conn.Notify(proto.NotifyTerminalEnd, proto.TerminalEnd{Stream: st.id, Reason: reason})
		m.d.log.Info("terminal closed", "session", st.session, "reason", reason)
	})
}

func (m *termManager) input(_ context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.TerminalInputParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	st := m.get(p.Stream, c)
	switch {
	case st == nil:
		return nil, proto.Errorf(proto.CodeNotFound, "no such terminal")
	case !st.control:
		return nil, proto.Errorf(proto.CodeRefused, "this terminal only observes")
	case len(p.Data) == 0 || len(p.Data) > proto.MaxTerminalInput:
		return nil, proto.Errorf(proto.CodeInvalidParams, "input must be 1 to %d bytes", proto.MaxTerminalInput)
	}
	st.wmu.Lock()
	defer st.wmu.Unlock()
	_ = st.ptmx.SetWriteDeadline(time.Now().Add(termWriteWait))
	if _, err := st.ptmx.Write([]byte(p.Data)); err != nil {
		return nil, proto.Errorf(proto.CodeBusy, "the terminal does not take input")
	}
	return struct{}{}, nil
}

func (m *termManager) close(_ context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.TerminalStreamParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	st := m.get(p.Stream, c)
	if st == nil {
		return nil, proto.Errorf(proto.CodeNotFound, "no such terminal")
	}
	st.end("closed")
	return struct{}{}, nil
}

// closeAll ends every stream (the daemon stops).
func (m *termManager) closeAll() {
	m.mu.Lock()
	all := make([]*termStream, 0, len(m.streams))
	for _, st := range m.streams {
		all = append(all, st)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, st := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st.end("the machine's daemon is stopping")
		}()
	}
	wg.Wait()
}
