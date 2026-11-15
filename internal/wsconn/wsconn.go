// Package wsconn carries the JSON-RPC codec of internal/proto over a
// WebSocket (work order C11a section 3): one JSON object per text frame.
// The codec writes one line per message; Conn sends each line as one text
// frame without its newline, and hands every received frame to the codec
// as one line.
//
// Security rules: the peer may be hostile (a machine for the hub, a hub for
// a machine). Frames above proto.MaxMessage, binary frames and frames that
// contain a newline (more than one object) end the connection.
package wsconn

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/proto"
)

// ErrFrame is returned by Read for a frame that is not one JSON text line.
var ErrFrame = errors.New("websocket: not a single text message")

// Conn adapts a WebSocket connection to the io.ReadWriteCloser the codec
// expects, with read and write deadlines (the codec's idle and write
// timeouts). An expired deadline closes the connection, as the WebSocket
// library does for every cancelled operation.
type Conn struct {
	ws  *websocket.Conn
	ctx context.Context

	mu       sync.Mutex
	rdl, wdl time.Time
	once     sync.Once

	buf []byte // the rest of the current frame, newline included
}

// New wraps ws; ctx bounds every operation. The read limit is set to the
// codec's message limit.
func New(ctx context.Context, ws *websocket.Conn) *Conn {
	ws.SetReadLimit(proto.MaxMessage)
	return &Conn{ws: ws, ctx: ctx}
}

// WS returns the underlying connection (for pings).
func (c *Conn) WS() *websocket.Conn { return c.ws }

func (c *Conn) opCtx(deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(c.ctx)
	}
	return context.WithDeadline(c.ctx, deadline)
}

// Read returns the bytes of the current frame followed by a newline,
// reading the next frame when the current one is used up.
func (c *Conn) Read(p []byte) (int, error) {
	if len(c.buf) == 0 {
		c.mu.Lock()
		dl := c.rdl
		c.mu.Unlock()
		ctx, cancel := c.opCtx(dl)
		typ, b, err := c.ws.Read(ctx)
		cancel()
		if err != nil {
			return 0, err
		}
		if typ != websocket.MessageText || bytes.IndexByte(b, '\n') >= 0 {
			c.closeWith(websocket.StatusUnsupportedData, "one JSON object per text frame")
			return 0, ErrFrame
		}
		c.buf = append(b, '\n')
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

// Write sends one line (the codec writes whole lines) as one text frame.
func (c *Conn) Write(p []byte) (int, error) {
	line := bytes.TrimSuffix(p, []byte{'\n'})
	if bytes.IndexByte(line, '\n') >= 0 {
		return 0, ErrFrame
	}
	c.mu.Lock()
	dl := c.wdl
	c.mu.Unlock()
	ctx, cancel := c.opCtx(dl)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, line); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close closes the connection with a normal closure. It does not wait for
// the peer's half of the closing handshake (the library waits up to five
// seconds for it, in the background here).
func (c *Conn) Close() error {
	c.closeWith(websocket.StatusNormalClosure, "")
	return nil
}

func (c *Conn) closeWith(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		go func() {
			if err := c.ws.Close(code, reason); err != nil {
				_ = c.ws.CloseNow()
			}
		}()
	})
}

// SetReadDeadline and SetWriteDeadline set the deadline of the following
// reads and writes (zero: none).
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.rdl = t
	c.mu.Unlock()
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wdl = t
	c.mu.Unlock()
	return nil
}
