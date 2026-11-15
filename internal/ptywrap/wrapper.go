package ptywrap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Options configure Run.
type Options struct {
	// Path, Args and Env are the harness's executable, argument vector
	// (Args[0] is its name) and environment.
	Path string
	Args []string
	Env  []string
	Dir  string
	// Stdin and Stdout are the wrapper's own terminal (or pipes: then no
	// raw mode and no window size).
	Stdin  *os.File
	Stdout *os.File
	// OnReport receives every valid report, in order, on a goroutine of its
	// own; a slow receiver never slows the output (of reports that pile up
	// beyond 16, the oldest are dropped).
	OnReport func(Report)
	// Log receives problems (nil: discarded).
	Log io.Writer
	// Drain bounds how long output is still copied after the harness
	// exited while something else keeps its terminal open (default 2 s).
	Drain time.Duration
}

// Result is how the harness ended.
type Result struct {
	// Status is the exit status; Signal is set (and Status -1) when a
	// signal ended it.
	Status int
	Signal syscall.Signal
}

// forwarded are the signals passed on to the harness.
var forwarded = []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2}

// Run runs the harness on a new pseudo-terminal and copies bytes until it
// has exited. The wrapper's terminal is in raw mode meanwhile and is
// restored before Run returns, whatever happened.
func Run(o Options) (res Result, err error) {
	if o.Drain <= 0 {
		o.Drain = 2 * time.Second
	}
	logf := func(format string, args ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, "devc box pty: "+format+"\n", args...)
		}
	}
	inFd := int(o.Stdin.Fd())
	saved, termErr := unix.IoctlGetTermios(inFd, unix.TCGETS)
	isTerm := termErr == nil

	ptmx, tty, err := openPTY()
	if err != nil {
		return Result{}, fmt.Errorf("allocating a pseudo-terminal: %w", err)
	}
	defer ptmx.Close()
	if isTerm {
		// The harness's terminal starts with the settings of ours (UTF-8,
		// erase character, …), as it would without the wrapper.
		_ = unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, saved)
		if ws, err := unix.IoctlGetWinsize(inFd, unix.TIOCGWINSZ); err == nil {
			_ = setWinsize(ptmx, ws)
		}
	}
	cmd := &exec.Cmd{Path: o.Path, Args: o.Args, Env: o.Env, Dir: o.Dir, Stdin: tty, Stdout: tty, Stderr: tty,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true, Setctty: true}}
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, append([]os.Signal{syscall.SIGWINCH}, forwarded...)...)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		tty.Close()
		return Result{}, fmt.Errorf("starting %s: %w", o.Path, err)
	}
	tty.Close()

	restore := func() {}
	if isTerm {
		raw := *saved
		makeRaw(&raw)
		if err := unix.IoctlSetTermios(inFd, unix.TCSETS, &raw); err != nil {
			logf("cannot put the terminal into raw mode: %v", err)
		}
		var once sync.Once
		restore = func() { once.Do(func() { _ = unix.IoctlSetTermios(inFd, unix.TCSETS, saved) }) }
	}
	defer restore()
	defer func() {
		if r := recover(); r != nil {
			restore()
			panic(r)
		}
	}()

	// Writes to the harness's input come from two places (the terminal and
	// the answer to a query); a lock keeps each write whole.
	var wmu sync.Mutex
	writeIn := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_, err := ptmx.Write(b)
		return err
	}

	reports := newReporter(o.OnReport)
	defer reports.close()

	// Terminal to harness: unchanged.
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := o.Stdin.Read(buf)
			if n > 0 {
				if werr := writeIn(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Harness to terminal: everything but OSC 7501. Once the harness has
	// exited, reading continues while output keeps coming (something it
	// started may still hold the terminal), at most until drainUntil.
	var exited atomic.Bool
	var drainUntil atomic.Int64
	quiet := 100 * time.Millisecond
	// prefixWait is how long a possible start of a status report ("ESC",
	// "ESC ]", … up to "ESC ] 7501 ;") is held back when no byte follows.
	prefixWait := 30 * time.Millisecond
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		var sc Scanner
		buf := make([]byte, 32<<10)
		out := make([]byte, 0, 40<<10)
		stdoutOK := true
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				var items []Item
				out, items = sc.Feed(out[:0], buf[:n])
				for _, it := range items {
					if it.Query() {
						// Answered before the rest of the output reaches the
						// terminal, so the answer precedes any reply to a
						// query that follows it (device attributes).
						_ = writeIn([]byte(Reply + it.Term))
						continue
					}
					r, perr := ParseReport(it.Body)
					if perr != nil {
						logf("ignored a malformed status report")
						continue
					}
					reports.put(r)
				}
				if len(out) > 0 && stdoutOK {
					if _, werr := o.Stdout.Write(out); werr != nil {
						// The terminal went away; keep reading so the harness
						// never blocks on a full terminal.
						stdoutOK = false
					}
				}
			}
			if errors.Is(err, os.ErrDeadlineExceeded) && !exited.Load() {
				if sc.HeldPrefix() {
					// Nothing followed what might have begun a status report:
					// it was something else, pass it on.
					if rest := sc.Flush(nil); len(rest) > 0 && stdoutOK {
						if _, werr := o.Stdout.Write(rest); werr != nil {
							stdoutOK = false
						}
					}
				}
				err = nil
			}
			if err == nil {
				// Deadlines: quiet after the harness exited (bounded by
				// drainUntil); a short one while a possible start of a status
				// report waits for its next byte; none otherwise. The exit is
				// checked again after setting, so a deadline set by the
				// harness's exit is never lost.
				switch {
				case exited.Load():
					if time.Now().UnixNano() < drainUntil.Load() {
						_ = ptmx.SetReadDeadline(time.Now().Add(quiet))
					}
				case sc.HeldPrefix():
					_ = ptmx.SetReadDeadline(time.Now().Add(prefixWait))
				default:
					_ = ptmx.SetReadDeadline(time.Time{})
				}
				if exited.Load() && time.Now().UnixNano() < drainUntil.Load() {
					_ = ptmx.SetReadDeadline(time.Now().Add(quiet))
				}
				continue
			}
			// EIO: nothing holds the harness's terminal any more; a
			// deadline: quiet (or too long) after the harness exited.
			if stdoutOK {
				if rest := sc.Flush(nil); len(rest) > 0 {
					_, _ = o.Stdout.Write(rest)
				}
			}
			return
		}
	}()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var werr error
loop:
	for {
		select {
		case s := <-sigs:
			if s == syscall.SIGWINCH {
				if isTerm {
					if ws, err := unix.IoctlGetWinsize(inFd, unix.TIOCGWINSZ); err == nil {
						_ = setWinsize(ptmx, ws)
					}
				}
				continue
			}
			if sig, ok := s.(syscall.Signal); ok && cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		case werr = <-waited:
			break loop
		}
	}
	drainUntil.Store(time.Now().Add(o.Drain).UnixNano())
	exited.Store(true)
	_ = ptmx.SetReadDeadline(time.Now().Add(quiet))
	select {
	case <-outDone:
	case <-time.After(o.Drain + time.Second):
	}
	restore()
	st, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	switch {
	case ok && st.Signaled():
		return Result{Status: -1, Signal: st.Signal()}, nil
	case ok && st.Exited():
		return Result{Status: st.ExitStatus()}, nil
	case werr != nil:
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			return Result{Status: ee.ExitCode()}, nil
		}
		return Result{}, werr
	}
	return Result{Status: cmd.ProcessState.ExitCode()}, nil
}

// makeRaw is cfmakeraw(3).
func makeRaw(t *unix.Termios) {
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
}

// control runs fn with the file's descriptor without switching it to
// blocking mode (which os.File.Fd would do, disabling read deadlines).
func control(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

func setWinsize(ptmx *os.File, ws *unix.Winsize) error {
	return control(ptmx, func(fd int) error { return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, ws) })
}

// openPTY opens a new pseudo-terminal pair. The master stays non-blocking
// (read deadlines work); the slave is for the harness.
func openPTY() (*os.File, *os.File, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	var n uint32
	err = control(ptmx, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return err
		}
		var err error
		n, err = unix.IoctlGetUint32(fd, unix.TIOCGPTN)
		return err
	})
	if err != nil {
		ptmx.Close()
		return nil, nil, err
	}
	tty, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		ptmx.Close()
		return nil, nil, err
	}
	return ptmx, tty, nil
}

// reporter hands reports to a callback on its own goroutine. Reports that
// arrive while the callback is busy are coalesced: only the latest waits.
type reporter struct {
	fn      func(Report)
	mu      sync.Mutex
	pending []Report
	wake    chan struct{}
	done    chan struct{}
	closed  bool
}

// maxPending bounds reports waiting for the callback; beyond it the oldest
// are dropped (a harness that floods reports loses intermediate states,
// never the latest).
const maxPending = 16

func newReporter(fn func(Report)) *reporter {
	r := &reporter{fn: fn, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if fn == nil {
		close(r.done)
		return r
	}
	go r.run()
	return r
}

func (r *reporter) put(rep Report) {
	if r.fn == nil {
		return
	}
	r.mu.Lock()
	r.pending = append(r.pending, rep)
	if len(r.pending) > maxPending {
		r.pending = r.pending[len(r.pending)-maxPending:]
	}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *reporter) run() {
	defer close(r.done)
	for {
		r.mu.Lock()
		batch := r.pending
		r.pending = nil
		closed := r.closed
		r.mu.Unlock()
		for _, rep := range batch {
			func() {
				defer func() { _ = recover() }()
				r.fn(rep)
			}()
		}
		if closed && len(batch) == 0 {
			return
		}
		if len(batch) == 0 {
			<-r.wake
		}
	}
}

// close delivers what is pending and stops (bounded wait).
func (r *reporter) close() {
	if r.fn == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
	}
}
