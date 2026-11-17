// This file implements the JSON-RPC 2.0 codec of every devc connection.

package proto

// The JSON-RPC 2.0 codec (plan 3.11): one JSON object per line, used on
// the daemon's Unix socket, on every box channel stream and, through
// internal/wsconn, inside the WebSocket frames of a hub link. A Conn is
// symmetric: either side may serve requests from a method table and make
// calls of its own.
//
// Security rules: the peer may be hostile (a box on its channel streams).
// Lines above MaxMessage are refused and end the connection; every message
// is decoded strictly (no unknown fields, nothing after the object, version
// "2.0", a valid id); unknown methods get an error; responses that match no
// pending call are dropped. Handler errors that are not *Error are reported
// as "internal error" unless the connection is told to expose them, so host
// details never reach a box.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// MaxMessage is the largest line a reader accepts (1 MiB, newline excluded).
const MaxMessage = 1 << 20

// ErrTooLarge is returned by ReadLine for a line above the limit.
var ErrTooLarge = errors.New("message larger than 1 MiB")

// ErrClosed is returned by calls on a closed connection.
var ErrClosed = errors.New("connection closed")

// Error codes: the JSON-RPC 2.0 ones and devc's own (-32000 to -32099).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603

	// CodeHostUnavailable: the box agent has no host connection.
	CodeHostUnavailable = -32000
	// CodeNotFound: the named session, box or object does not exist.
	CodeNotFound = -32001
	// CodeRefused: the request is valid but not allowed.
	CodeRefused = -32002
	// CodeTooLarge: the result would exceed MaxMessage.
	CodeTooLarge = -32003
	// CodeBusy: too many requests or connections; try again.
	CodeBusy = -32004
	// CodeVersion: the peer speaks a protocol version this side does not know.
	CodeVersion = -32005
)

// Error is a JSON-RPC error object; handlers return it to choose the code.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// Errorf returns an *Error with a formatted message.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsCode reports whether err is an *Error with code.
func IsCode(err error, code int) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// RPCMessage is one JSON-RPC message: a request (method and id), a
// notification (method, no id) or a response (id and result or error).
type RPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Kinds of messages.
const (
	KindRequest = iota + 1
	KindNotification
	KindResponse
)

// maxID bounds the length of an id.
const maxID = 128

// validID accepts a JSON number or string of bounded length (not null).
func validID(id json.RawMessage) bool {
	if len(id) == 0 || len(id) > maxID {
		return false
	}
	switch id[0] {
	case '"':
		var s string
		return json.Unmarshal(id, &s) == nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		return json.Unmarshal(id, &n) == nil
	}
	return false
}

// ParseMessage decodes one line strictly and classifies it.
func ParseMessage(line []byte) (*RPCMessage, int, error) {
	if len(line) > MaxMessage {
		return nil, 0, ErrTooLarge
	}
	var m RPCMessage
	if err := decodeStrict(line, &m); err != nil {
		return nil, 0, err
	}
	if m.JSONRPC != "2.0" {
		return &m, 0, errors.New(`"jsonrpc" must be "2.0"`)
	}
	if m.ID != nil && string(m.ID) != "null" && !validID(m.ID) {
		return &m, 0, errors.New("invalid id")
	}
	if m.Method != "" {
		switch {
		case m.Result != nil || m.Error != nil:
			return &m, 0, errors.New("a request carries no result or error")
		case len(m.Method) > 128:
			return &m, 0, errors.New("method name too long")
		case m.ID == nil:
			return &m, KindNotification, nil
		case string(m.ID) == "null":
			return &m, 0, errors.New("a request id must not be null")
		}
		return &m, KindRequest, nil
	}
	if m.ID == nil || (m.Result == nil) == (m.Error == nil) || m.Params != nil {
		return &m, 0, errors.New("neither a valid request nor a valid response")
	}
	return &m, KindResponse, nil
}

// ReadLine reads one line of at most max bytes (without its newline) and
// returns it without the trailing newline or carriage return. A longer
// line fails with ErrTooLarge; the stream cannot be resynchronised then.
func ReadLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max+1 {
			return nil, ErrTooLarge
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			buf = bytes.TrimSuffix(buf[:len(buf)-1], []byte{'\r'})
			if len(buf) > max {
				return nil, ErrTooLarge
			}
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(buf) > 0:
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// Handler serves one request; the result is encoded as JSON. Returning an
// *Error chooses the error code.
type Handler func(ctx context.Context, c *Conn, params json.RawMessage) (any, error)

// NotifyHandler handles one notification. Notification handlers run in
// the order the notifications arrive, on the connection's reader.
type NotifyHandler func(ctx context.Context, c *Conn, params json.RawMessage)

// Options configure a Conn.
type Options struct {
	// Handlers serve requests; Notify handles notifications. Unknown
	// methods get CodeMethodNotFound (requests) or are dropped.
	Handlers map[string]Handler
	Notify   map[string]NotifyHandler
	// CallTimeout bounds calls whose context has no deadline (30 s).
	CallTimeout time.Duration
	// WriteTimeout bounds each write when the connection supports
	// deadlines (30 s).
	WriteTimeout time.Duration
	// IdleTimeout closes the connection when nothing was read for that
	// long (zero: never).
	IdleTimeout time.Duration
	// ExposeErrors sends the text of non-*Error handler errors to the
	// peer; otherwise they become "internal error". Only for peers that
	// may see host details (the same user on the daemon socket).
	ExposeErrors bool
	// Workers is how many requests are served at once (default 1: one at a
	// time, in arrival order). A peer that forwards independent calls (the
	// hub) needs more, so that a long command.get does not hold up the rest.
	Workers int
	// Context is the parent of the connection's context.
	Context context.Context
	// Log receives problems with the peer (nil: discarded).
	Log *slog.Logger
}

// Conn is one JSON-RPC connection.
type Conn struct {
	rwc  io.ReadWriteCloser
	opts Options
	ctx  context.Context
	stop context.CancelFunc

	wmu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan *RPCMessage
	next    int64
	closed  bool
	err     error
	values  map[string]any
	done    chan struct{}

	reqs chan *RPCMessage
}

type deadliner interface {
	SetWriteDeadline(time.Time) error
	SetReadDeadline(time.Time) error
}

// NewConn starts serving rwc. Close it when done; it closes itself when
// the peer goes away or sends something that ends the connection.
func NewConn(rwc io.ReadWriteCloser, o Options) *Conn {
	if o.CallTimeout <= 0 {
		o.CallTimeout = 30 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 30 * time.Second
	}
	if o.Workers <= 0 {
		o.Workers = 1
	}
	parent := o.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := context.WithCancel(parent)
	c := &Conn{rwc: rwc, opts: o, ctx: ctx, stop: stop, pending: map[string]chan *RPCMessage{},
		values: map[string]any{}, done: make(chan struct{}), reqs: make(chan *RPCMessage, 16)}
	go c.readLoop()
	go c.serveLoop()
	go func() {
		<-ctx.Done()
		c.closeWith(ErrClosed)
	}()
	return c
}

// Context is cancelled when the connection closes.
func (c *Conn) Context() context.Context { return c.ctx }

// Done is closed when the connection has closed.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection closed (nil while open).
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Set stores a per-connection value (the server's per-connection state).
func (c *Conn) Set(key string, v any) {
	c.mu.Lock()
	c.values[key] = v
	c.mu.Unlock()
}

// Get returns a per-connection value.
func (c *Conn) Get(key string) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key]
}

// Claim sets key to true unless it is set already, and reports whether it
// did: an atomic once-per-connection guard for handlers that may run
// concurrently (Options.Workers).
func (c *Conn) Claim(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.values[key]; ok {
		return false
	}
	c.values[key] = true
	return true
}

// Close closes the connection; pending calls fail with ErrClosed.
func (c *Conn) Close() error {
	c.closeWith(ErrClosed)
	return nil
}

// closeWith closes the connection once, recording why, and fails every
// pending call.
func (c *Conn) closeWith(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed, c.err = true, err
	pending := c.pending
	c.pending = map[string]chan *RPCMessage{}
	c.mu.Unlock()
	c.stop()
	_ = c.rwc.Close()
	for _, ch := range pending {
		close(ch)
	}
	close(c.done)
}

func (c *Conn) logf(msg string, args ...any) {
	if c.opts.Log != nil {
		c.opts.Log.Debug(msg, args...)
	}
}

// readLoop is the only sender on c.reqs and closes it when it ends. At a
// clean end of input (the peer closed its side) the requests already read
// are still answered before the connection closes.
func (c *Conn) readLoop() {
	defer close(c.reqs)
	r := bufio.NewReaderSize(c.rwc, 64<<10)
	d, canDeadline := c.rwc.(deadliner)
	for {
		if canDeadline && c.opts.IdleTimeout > 0 {
			_ = d.SetReadDeadline(time.Now().Add(c.opts.IdleTimeout))
		}
		line, err := ReadLine(r, MaxMessage)
		if err != nil {
			if errors.Is(err, ErrTooLarge) {
				c.logf("rpc: peer sent a line above the limit; closing")
			}
			if !errors.Is(err, io.EOF) {
				c.closeWith(err)
			}
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		m, kind, err := ParseMessage(line)
		if err != nil {
			c.logf("rpc: malformed message", "err", err)
			// Only a request gets an answer; anything else is dropped.
			id := json.RawMessage("null")
			code := CodeParseError
			if m != nil {
				code = CodeInvalidRequest
				if m.Method != "" && m.ID != nil && validID(m.ID) {
					id = m.ID
				}
			}
			_ = c.write(&RPCMessage{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: Printable(Truncate(err.Error(), 200))}})
			continue
		}
		switch kind {
		case KindResponse:
			c.mu.Lock()
			ch, ok := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ok {
				ch <- m
			}
		case KindNotification:
			if h := c.opts.Notify[m.Method]; h != nil {
				c.safeNotify(h, m.Params)
			}
		case KindRequest:
			select {
			case c.reqs <- m:
			case <-c.ctx.Done():
				return
			}
		}
	}
}

func (c *Conn) safeNotify(h NotifyHandler, params json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			c.logf("rpc: notification handler panicked", "panic", fmt.Sprint(r))
		}
	}()
	h(c.ctx, c, params)
}

// serveLoop runs the workers; once the reader has ended and every queued
// request is answered, the connection closes.
func (c *Conn) serveLoop() {
	var wg sync.WaitGroup
	for range c.opts.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker()
		}()
	}
	wg.Wait()
	if c.ctx.Err() == nil {
		c.closeWith(io.EOF)
	}
}

// worker serves queued requests one at a time and writes each answer; a
// failed write closes the connection.
func (c *Conn) worker() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case m, ok := <-c.reqs:
			if !ok {
				return
			}
			resp := c.serve(m)
			if err := c.write(resp); err != nil {
				c.closeWith(err)
				return
			}
		}
	}
}

// serve answers one request from the method table. A panic, a result that
// cannot be encoded or is too large, and an error that is not an *Error
// (unless ExposeErrors) become generic errors, so no unplanned detail of
// the host reaches the peer. An *Error passes unchanged: what its message
// says is the handler's choice.
func (c *Conn) serve(m *RPCMessage) (resp *RPCMessage) {
	resp = &RPCMessage{JSONRPC: "2.0", ID: m.ID}
	defer func() {
		if r := recover(); r != nil {
			c.logf("rpc: handler panicked", "method", m.Method, "panic", fmt.Sprint(r))
			resp.Result, resp.Error = nil, &Error{Code: CodeInternal, Message: "internal error"}
		}
	}()
	h := c.opts.Handlers[m.Method]
	if h == nil {
		resp.Error = &Error{Code: CodeMethodNotFound, Message: "unknown method " + strconv.Quote(Printable(Truncate(m.Method, 64)))}
		return resp
	}
	res, err := h(c.ctx, c, m.Params)
	if err != nil {
		var e *Error
		switch {
		case errors.As(err, &e):
			resp.Error = e
		case c.opts.ExposeErrors:
			resp.Error = &Error{Code: CodeInternal, Message: err.Error()}
		default:
			resp.Error = &Error{Code: CodeInternal, Message: "internal error"}
		}
		return resp
	}
	b, err := json.Marshal(res)
	if err != nil {
		resp.Error = &Error{Code: CodeInternal, Message: "internal error"}
		return resp
	}
	if len(b) > MaxMessage-256 {
		resp.Error = &Error{Code: CodeTooLarge, Message: "the result is too large"}
		return resp
	}
	resp.Result = b
	return resp
}

// write sends one message as one line.
func (c *Conn) write(m *RPCMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxMessage {
		return ErrTooLarge
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if d, ok := c.rwc.(deadliner); ok {
		_ = d.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout))
	}
	_, err = c.rwc.Write(b)
	return err
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	if c.isClosed() {
		return ErrClosed
	}
	m := &RPCMessage{JSONRPC: "2.0", Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = b
	}
	if err := c.write(m); err != nil {
		c.closeWith(err)
		return err
	}
	return nil
}

func (c *Conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Call sends a request and decodes the result into result (unless nil). An
// error response is returned as *Error. Without a deadline in ctx the call
// is bounded by Options.CallTimeout.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.CallTimeout)
		defer cancel()
	}
	m := &RPCMessage{JSONRPC: "2.0", Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = b
	}
	ch := make(chan *RPCMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.next++
	id := strconv.FormatInt(c.next, 10)
	c.pending[id] = ch
	c.mu.Unlock()
	m.ID = json.RawMessage(id)
	drop := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	if err := c.write(m); err != nil {
		drop()
		c.closeWith(err)
		return err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, result)
	case <-ctx.Done():
		drop()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.done:
		return ErrClosed
	}
}

// DecodeParams decodes request parameters strictly into v: unknown fields
// and trailing data are refused. Absent parameters leave v unchanged.
func DecodeParams(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	if err := decodeStrict(params, v); err != nil {
		return &Error{Code: CodeInvalidParams, Message: "invalid params: " + Printable(Truncate(err.Error(), 200))}
	}
	return nil
}

// DecodeResult decodes a raw result strictly (for results from a hostile
// peer): unknown fields and trailing data are refused. Unlike parameters, a
// result may be any JSON value (boxes.list returns an array).
func DecodeResult(raw json.RawMessage, v any) error {
	if t := bytes.TrimSpace(raw); len(t) == 0 || string(t) == "null" {
		return errors.New("no result")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the JSON value")
	}
	return nil
}
