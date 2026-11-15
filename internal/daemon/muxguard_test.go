package daemon

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mb706/devc/internal/proto"
)

func muxFrame(typ uint8, flags uint16, id, length uint32) []byte {
	h := make([]byte, muxHeaderSize)
	h[1] = typ
	binary.BigEndian.PutUint16(h[2:4], flags)
	binary.BigEndian.PutUint32(h[4:8], id)
	binary.BigEndian.PutUint32(h[8:12], length)
	return h
}

type nopConn struct{ closed bool }

func (c *nopConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *nopConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *nopConn) Close() error                { c.closed = true; return nil }

// feed hands b to the guard as inbound bytes in pieces of n.
func feed(g *muxGuard, b []byte, n int) error {
	for len(b) > 0 {
		k := min(n, len(b))
		if err := g.inspect(&g.in, b[:k], true); err != nil {
			return err
		}
		b = b[k:]
	}
	return nil
}

func TestMuxGuard(t *testing.T) {
	clock := time.Unix(1000, 0)
	newGuard := func() *muxGuard {
		g := newMuxGuard(&nopConn{})
		g.now = func() time.Time { return clock }
		return g
	}

	// Data is never budgeted: 64 MiB on one stream, in odd pieces.
	g := newGuard()
	if err := feed(g, muxFrame(muxTypeWindow, muxFlagSYN, 2, 0), 5); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 256<<10)
	for i := 0; i < 256; i++ {
		if err := feed(g, append(muxFrame(muxTypeData, 0, 2, uint32(len(payload))), payload...), 7919); err != nil {
			t.Fatalf("data frame %d: %v", i, err)
		}
	}
	// Both sides close: the stream is gone.
	feed(g, muxFrame(muxTypeWindow, muxFlagFIN, 2, 0), 12)
	g.inspect(&g.out, muxFrame(muxTypeWindow, muxFlagFIN, 2, 0), false)
	if g.tracked() != 0 || g.violation() != nil {
		t.Errorf("after a closed stream: %d tracked, %v", g.tracked(), g.violation())
	}

	// Streams opened and closed in turn stay within the budget, also
	// beyond the window; a reset frees a stream at once.
	g = newGuard()
	for i := uint32(0); i < 3*muxMaxOpens; i++ {
		id := 2 * (i + 1)
		if err := feed(g, muxFrame(muxTypeWindow, muxFlagSYN, id, 0), 12); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		if i%2 == 0 {
			g.inspect(&g.out, muxFrame(muxTypeWindow, muxFlagRST, id, 0), false)
		} else {
			feed(g, muxFrame(muxTypeData, muxFlagFIN, id, 0), 12)
			g.inspect(&g.out, muxFrame(muxTypeWindow, muxFlagFIN, id, 0), false)
		}
		if i%100 == 99 {
			clock = clock.Add(muxWindow)
		}
	}
	if g.tracked() != 0 || g.violation() != nil {
		t.Errorf("churn: %d tracked, %v", g.tracked(), g.violation())
	}

	// Streams the daemon closed but the box never does count until the
	// budget closes the channel (C1-03).
	g = newGuard()
	var err error
	for i := uint32(1); i <= muxMaxStreams+1 && err == nil; i++ {
		if err = feed(g, muxFrame(muxTypeWindow, muxFlagSYN, 2*i, 0), 12); err == nil {
			err = g.inspect(&g.out, muxFrame(muxTypeWindow, muxFlagFIN, 2*i, 0), false)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "half-closed") {
		t.Errorf("half-closed streams: %v", err)
	}
	if !g.conn.(*nopConn).closed && g.violation() == nil {
		t.Error("no violation recorded")
	}
	if _, werr := g.Write(muxFrame(muxTypePing, muxFlagSYN, 0, 1)); werr == nil {
		t.Error("the guard writes after a violation")
	}

	// Too many new streams in one window.
	g = newGuard()
	err = nil
	for i := uint32(1); i <= muxMaxOpens+1 && err == nil; i++ {
		if err = feed(g, muxFrame(muxTypeWindow, muxFlagSYN, 2*i, 0), 12); err == nil {
			g.inspect(&g.out, muxFrame(muxTypeWindow, muxFlagRST, 2*i, 0), false)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "new streams") {
		t.Errorf("stream flood: %v", err)
	}

	// A ping flood (C1-04); pings spread over windows are fine.
	g = newGuard()
	for i := 0; i < 5*muxMaxControl; i++ {
		if err := feed(g, muxFrame(muxTypePing, muxFlagSYN, 0, uint32(i)), 3); err != nil {
			t.Fatalf("spread pings: %v", err)
		}
		if i%muxMaxControl == muxMaxControl-1 {
			clock = clock.Add(muxWindow)
		}
	}
	err = nil
	for i := 0; i <= muxMaxControl && err == nil; i++ {
		err = feed(g, muxFrame(muxTypePing, muxFlagSYN, 0, uint32(i)), 12)
	}
	if err == nil || !strings.Contains(err.Error(), "ping") {
		t.Errorf("ping flood: %v", err)
	}
	// The daemon's own pings and their answers are not the box's budget.
	g = newGuard()
	for i := 0; i < 3*muxMaxControl; i++ {
		if err := g.inspect(&g.out, muxFrame(muxTypePing, muxFlagSYN, 0, uint32(i)), false); err != nil {
			t.Fatal(err)
		}
	}
	// Garbage instead of frames.
	g = newGuard()
	if err := feed(g, []byte("\xff garbage that is not yamux"), 64); err == nil {
		t.Error("garbage passed the guard")
	}
}

// TestHostileTransport: a box that floods pings, or keeps streams open, has
// its channel closed; the daemon reconnects and keeps answering (findings
// C1-03, C1-04).
func TestHostileTransport(t *testing.T) {
	r := newRig(t)
	r.boxUp(true)
	r.start()
	c := r.client(nil)
	hello := func(s *yamux.Session) bool {
		st, err := s.AcceptStream()
		if err != nil {
			return false
		}
		bufio.NewReader(st).ReadString('\n')
		st.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocol":1,"devc_version":"x","realm":"demo","project":"hello","box":"b"}}` + "\n"))
		return true
	}
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	for _, flood := range []func(net.Conn, *yamux.Session){
		// Raw ping frames, never reading the answers.
		func(c net.Conn, _ *yamux.Session) {
			for i := 0; i < 10*muxMaxControl; i++ {
				if _, err := c.Write(muxFrame(muxTypePing, muxFlagSYN, 0, uint32(i))); err != nil {
					return
				}
			}
		},
		// Streams opened and never closed.
		func(_ net.Conn, s *yamux.Session) {
			for i := 0; i < 2*muxMaxStreams; i++ {
				if _, err := s.OpenStream(); err != nil {
					return
				}
			}
		},
	} {
		r.hostileAgent(t, func(c net.Conn) {
			s, err := yamux.Server(c, cfg)
			if err != nil {
				return
			}
			defer s.Close()
			if !hello(s) {
				return
			}
			time.Sleep(time.Second)
			flood(c, s)
			time.Sleep(3 * time.Second)
		})
		r.waitChannel(c, proto.ChannelConnected)
		r.waitDetail(c, proto.ChannelRefused, "exceeded the channel's limits")
		if err := c.Call(context.Background(), proto.MethodDaemonInfo, nil, nil); err != nil {
			t.Errorf("the daemon after a hostile transport: %v", err)
		}
	}
}
