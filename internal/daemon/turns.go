// This file implements banking after turns.

package daemon

// Banking after turns (work order C10a section 1): when a session goes
// from working to done or idle, the daemon banks its box, at most once per
// interval per box, so snapshots and commits the agent did not push reach
// the relay. The bank is `devc bank <ref> --quiet` as a child process of
// the same binary, which takes the box lock and refuses on configuration
// problems like every bank.

import (
	"sync"
	"time"

	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// DefaultTurnBank is the least time between two banks after turns of one
// box.
const DefaultTurnBank = 30 * time.Second

// turnBanker schedules the banks after turns, per box.
type turnBanker struct {
	d     *Daemon
	every time.Duration
	mu    sync.Mutex
	boxes map[names.ProjectRef]*turnState
}

// turnState is one box's bank schedule: when the last bank started, the
// timer of the next, whether one runs, and whether a turn ended meanwhile.
type turnState struct {
	last    time.Time
	timer   *time.Timer
	running bool
	pending bool
}

func newTurnBanker(d *Daemon, every time.Duration) *turnBanker {
	return &turnBanker{d: d, every: every, boxes: map[names.ProjectRef]*turnState{}}
}

// turnEnded schedules a bank of ref's box: now, or when the interval since
// the last one has passed. It never blocks (the hub calls it under its
// lock).
func (b *turnBanker) turnEnded(ref names.ProjectRef) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.boxes[ref]
	if st == nil {
		st = &turnState{}
		b.boxes[ref] = st
	}
	switch {
	case st.running:
		st.pending = true
	case st.timer == nil:
		b.scheduleLocked(ref, st)
	}
}

// scheduleLocked starts the timer of the next bank, an interval after the
// last one (at once if that has passed). The caller holds b.mu.
func (b *turnBanker) scheduleLocked(ref names.ProjectRef, st *turnState) {
	wait := time.Until(st.last.Add(b.every))
	if wait < 0 {
		wait = 0
	}
	st.timer = time.AfterFunc(wait, func() { b.run(ref) })
}

// run banks ref's box with `devc bank <ref> --quiet` when it runs and has
// a relay, and schedules one more bank if a turn ended meanwhile.
func (b *turnBanker) run(ref names.ProjectRef) {
	b.mu.Lock()
	st := b.boxes[ref]
	st.timer, st.running, st.last = nil, true, time.Now()
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		st.running = false
		if st.pending && b.d.ctx.Err() == nil {
			st.pending = false
			b.scheduleLocked(ref, st)
		}
		b.mu.Unlock()
	}()
	if b.d.ctx == nil || b.d.ctx.Err() != nil {
		return
	}
	e := b.d.current()
	if !gitx.RelayExists(e.cfg.Paths.RelayRepo(ref)) || b.d.boxState(ref) != "running" {
		return
	}
	res, err := b.d.runChild(b.d.ctx, []string{"bank", ref.String(), "--quiet"}, 30*time.Minute)
	out := proto.PrintableText(string(res.Stdout) + string(res.Stderr))
	if err != nil {
		b.d.log.Warn("bank after a turn", "ref", ref.String(), "err", err, "output", out)
		return
	}
	b.d.log.Info("bank after a turn", "ref", ref.String(), "output", out)
}

// stop cancels the scheduled banks.
func (b *turnBanker) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, st := range b.boxes {
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
	}
}
