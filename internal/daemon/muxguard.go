// This file implements the frame inspector between a box's socket and yamux.

package daemon

// The transport guard of the box channel (findings C1-03 and C1-04). yamux
// itself keeps a stream it closed until the peer closes it too (or a
// minute passes), and answers every ping in a goroutine of its own; the
// application's limits on handlers and message sizes do not reach either.
// The guard sits between the box's socket and yamux on the daemon side and
// reads every frame header as it passes, in both directions: it counts the
// streams the box opens and the control frames it sends per time window,
// and tracks every stream that is open or half-closed. When a budget is
// exceeded it closes the whole connection; the daemon reconnects with
// backoff. Data is never budgeted here: a push or a terminal moves a lot of
// it over few streams.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Budgets of one box channel, all in one place.
//
// Since C13 a box of level allowlist reaches the network only through
// host.proxy streams, one per TCP connection, and host exceptions add
// host.dial streams. A package installation opens dozens of connections
// (pip and uv keep about ten to fifty open; npm up to fifteen per host;
// apt one, kept alive), and a script fetching URLs one by one opens a few
// per second. The box agent holds at most boxside.DefaultMaxRelays relays
// at once and opens at most half of muxMaxOpens relay streams per window,
// so ordinary use stays inside these budgets with room for in-box clients,
// hints and the host's own streams; only an agent that ignores its limits
// gets the channel closed.
const (
	// muxWindow is the time window of the rate budgets.
	muxWindow = 10 * time.Second
	// muxMaxOpens bounds the streams the box opens per window (every
	// in-box client, every hint and every relayed connection opens one).
	muxMaxOpens = 1024
	// muxMaxControl bounds the ping and go-away frames the box sends per
	// window (its keepalive sends one ping every 30 s).
	muxMaxControl = 32
	// muxMaxStreams bounds the streams open or half-closed at once, opened
	// by either side: each holds up to a 256 KiB receive window, so a box
	// can make the daemon buffer at most 48 MiB. It covers maxBoxStreams
	// requests and maxRelayStreams relays with room for the host's streams
	// and for streams that are closing.
	muxMaxStreams = 192
)

// Frame layout of yamux (spec.md of hashicorp/yamux): version, type,
// flags, stream ID, length; a data frame carries length bytes after it.
const (
	muxHeaderSize = 12
	muxTypeData   = 0
	muxTypeWindow = 1
	muxTypePing   = 2
	muxTypeGoAway = 3
	muxFlagSYN    = 1
	muxFlagFIN    = 4
	muxFlagRST    = 8
)

// errMuxBudget wraps the guard's reasons for closing a channel.
var errMuxBudget = errors.New("the box exceeded the channel's limits")

// muxStream is what the guard knows of one stream: which sides closed it.
type muxStream struct{ localFIN, remoteFIN bool }

// frameParser follows the frame boundaries of one direction.
type frameParser struct {
	hdr  [muxHeaderSize]byte
	have int    // header bytes collected
	skip uint32 // payload bytes still to pass
}

// muxGuard wraps the connection yamux runs on.
type muxGuard struct {
	conn io.ReadWriteCloser
	now  func() time.Time

	mu      sync.Mutex
	in, out frameParser
	streams map[uint32]*muxStream
	since   time.Time // start of the current window
	opens   int
	control int
	err     error
}

// newMuxGuard wraps the connection to a box's agent.
func newMuxGuard(conn io.ReadWriteCloser) *muxGuard {
	return &muxGuard{conn: conn, now: time.Now, streams: map[uint32]*muxStream{}}
}

// violation returns why the guard closed the connection, if it did.
func (g *muxGuard) violation() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

// Read passes what the box sent to yamux after inspecting it; a violation
// closes the connection and fails the read.
func (g *muxGuard) Read(b []byte) (int, error) {
	n, err := g.conn.Read(b)
	if n > 0 {
		if verr := g.inspect(&g.in, b[:n], true); verr != nil {
			_ = g.conn.Close()
			return 0, verr
		}
	}
	return n, err
}

// Write inspects what the daemon sends (to follow streams it opens and
// closes) before passing it on.
func (g *muxGuard) Write(b []byte) (int, error) {
	if verr := g.inspect(&g.out, b, false); verr != nil {
		_ = g.conn.Close()
		return 0, verr
	}
	return g.conn.Write(b)
}

func (g *muxGuard) Close() error { return g.conn.Close() }

// inspect follows the frames in b (inbound from the box, or outbound) and
// returns an error when a budget is exceeded or the box breaks the framing.
func (g *muxGuard) inspect(p *frameParser, b []byte, inbound bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	for len(b) > 0 {
		if p.skip > 0 {
			n := uint32(min(len(b), int(min(p.skip, 1<<30))))
			p.skip -= n
			b = b[n:]
			continue
		}
		n := copy(p.hdr[p.have:], b)
		p.have += n
		b = b[n:]
		if p.have < muxHeaderSize {
			break
		}
		p.have = 0
		if err := g.frame(p, inbound); err != nil {
			g.err = fmt.Errorf("%w: %v", errMuxBudget, err)
			return g.err
		}
	}
	return nil
}

// frame accounts for one complete header in p.
func (g *muxGuard) frame(p *frameParser, inbound bool) error {
	h := p.hdr[:]
	version, typ := h[0], h[1]
	flags := binary.BigEndian.Uint16(h[2:4])
	id := binary.BigEndian.Uint32(h[4:8])
	length := binary.BigEndian.Uint32(h[8:12])
	if version != 0 || typ > muxTypeGoAway {
		return fmt.Errorf("an invalid frame (version %d, type %d)", version, typ)
	}
	if typ == muxTypeData {
		p.skip = length
	}
	now := g.now()
	if now.Sub(g.since) >= muxWindow || now.Before(g.since) {
		g.since, g.opens, g.control = now, 0, 0
	}
	switch typ {
	case muxTypePing, muxTypeGoAway:
		if inbound {
			g.control++
			if g.control > muxMaxControl {
				return fmt.Errorf("more than %d ping or go-away frames in %s", muxMaxControl, muxWindow)
			}
		}
		return nil
	}
	// Data and window updates carry the stream's flags.
	if flags&muxFlagSYN != 0 {
		if inbound {
			g.opens++
			if g.opens > muxMaxOpens {
				return fmt.Errorf("more than %d new streams in %s", muxMaxOpens, muxWindow)
			}
		}
		if _, ok := g.streams[id]; !ok {
			g.streams[id] = &muxStream{}
			if len(g.streams) > muxMaxStreams {
				return fmt.Errorf("more than %d streams open or half-closed at once", muxMaxStreams)
			}
		}
	}
	st := g.streams[id]
	if st == nil {
		return nil
	}
	if flags&muxFlagRST != 0 {
		delete(g.streams, id)
		return nil
	}
	if flags&muxFlagFIN != 0 {
		if inbound {
			st.remoteFIN = true
		} else {
			st.localFIN = true
		}
		if st.localFIN && st.remoteFIN {
			delete(g.streams, id)
		}
	}
	return nil
}

// tracked is the number of streams the guard holds (tests).
func (g *muxGuard) tracked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.streams)
}
