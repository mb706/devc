package proto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadLine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("one\ntwo\r\n\nlast"), 16)
	for _, want := range []string{"one", "two", ""} {
		got, err := ReadLine(r, 100)
		if err != nil || string(got) != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	if _, err := ReadLine(r, 100); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("unterminated last line: %v", err)
	}
	if _, err := ReadLine(r, 100); !errors.Is(err, io.EOF) {
		t.Errorf("at the end: %v", err)
	}
	// Exactly at the limit (across buffer refills), and one byte over.
	ok := strings.Repeat("x", 64)
	r = bufio.NewReaderSize(strings.NewReader(ok+"\n"+ok+"y\n"), 16)
	if got, err := ReadLine(r, 64); err != nil || string(got) != ok {
		t.Errorf("line at the limit: %q %v", got, err)
	}
	if _, err := ReadLine(r, 64); !errors.Is(err, ErrTooLarge) {
		t.Errorf("line above the limit: %v", err)
	}
}

func TestParseMessage(t *testing.T) {
	for _, c := range []struct {
		line string
		kind int
		ok   bool
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"m"}`, KindRequest, true},
		{`{"jsonrpc":"2.0","id":"a","method":"m","params":{"x":1}}`, KindRequest, true},
		{`{"jsonrpc":"2.0","method":"m"}`, KindNotification, true},
		{`{"jsonrpc":"2.0","id":1,"result":{}}`, KindResponse, true},
		{`{"jsonrpc":"2.0","id":1,"result":null}`, KindResponse, true},
		{`{"jsonrpc":"2.0","id":null,"error":{"code":1,"message":"x"}}`, KindResponse, true},
		{`{"jsonrpc":"1.0","id":1,"method":"m"}`, 0, false},
		{`{"id":1,"method":"m"}`, 0, false},
		{`{"jsonrpc":"2.0","id":null,"method":"m"}`, 0, false},
		{`{"jsonrpc":"2.0","id":{},"method":"m"}`, 0, false},
		{`{"jsonrpc":"2.0","id":[1],"method":"m"}`, 0, false},
		{`{"jsonrpc":"2.0","id":true,"method":"m"}`, 0, false},
		{`{"jsonrpc":"2.0","id":1,"method":"m","result":1}`, 0, false},
		{`{"jsonrpc":"2.0","id":1,"result":1,"error":{"code":1,"message":"x"}}`, 0, false},
		{`{"jsonrpc":"2.0","id":1}`, 0, false},
		{`{"jsonrpc":"2.0","method":"m","extra":1}`, 0, false},
		{`{"jsonrpc":"2.0","method":"m"} {}`, 0, false},
		{`[{"jsonrpc":"2.0","method":"m"}]`, 0, false},
		{`not json`, 0, false},
		{`{"jsonrpc":"2.0","id":"` + strings.Repeat("x", 200) + `","method":"m"}`, 0, false},
	} {
		_, kind, err := ParseMessage([]byte(c.line))
		if (err == nil) != c.ok || kind != c.kind {
			t.Errorf("%s: kind %d, err %v", c.line, kind, err)
		}
	}
}

// pair connects two Conns through a pipe.
func pair(t *testing.T, server, client Options) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	s := NewConn(a, server)
	c := NewConn(b, client)
	t.Cleanup(func() { s.Close(); c.Close() })
	return s, c
}

func TestCalls(t *testing.T) {
	type addArgs struct{ A, B int }
	var notified sync.WaitGroup
	notified.Add(2)
	var got []string
	var mu sync.Mutex
	server := Options{Handlers: map[string]Handler{
		"add": func(_ context.Context, _ *Conn, p json.RawMessage) (any, error) {
			var a addArgs
			if err := DecodeParams(p, &a); err != nil {
				return nil, err
			}
			return a.A + a.B, nil
		},
		"fail": func(context.Context, *Conn, json.RawMessage) (any, error) {
			return nil, errors.New("secret host path /home/x")
		},
		"refuse": func(context.Context, *Conn, json.RawMessage) (any, error) { return nil, Errorf(CodeRefused, "no") },
		"panic":  func(context.Context, *Conn, json.RawMessage) (any, error) { panic("boom") },
		"slow": func(ctx context.Context, _ *Conn, _ json.RawMessage) (any, error) {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			return 1, nil
		},
		"huge": func(context.Context, *Conn, json.RawMessage) (any, error) {
			return strings.Repeat("x", MaxMessage), nil
		},
		"back": func(ctx context.Context, c *Conn, _ json.RawMessage) (any, error) {
			return nil, c.Notify("note", map[string]string{"from": "server"})
		},
	}, Notify: map[string]NotifyHandler{
		"note": func(_ context.Context, _ *Conn, p json.RawMessage) {
			mu.Lock()
			got = append(got, string(p))
			mu.Unlock()
			notified.Done()
		},
	}}
	client := Options{Notify: map[string]NotifyHandler{
		"note": func(_ context.Context, _ *Conn, p json.RawMessage) {
			mu.Lock()
			got = append(got, "client:"+string(p))
			mu.Unlock()
			notified.Done()
		},
	}}
	_, c := pair(t, server, client)
	ctx := context.Background()
	var sum int
	if err := c.Call(ctx, "add", addArgs{2, 3}, &sum); err != nil || sum != 5 {
		t.Errorf("add: %d %v", sum, err)
	}
	if err := c.Call(ctx, "add", map[string]int{"A": 1, "C": 2}, &sum); !IsCode(err, CodeInvalidParams) {
		t.Errorf("unknown parameter field: %v", err)
	}
	if err := c.Call(ctx, "nope", nil, nil); !IsCode(err, CodeMethodNotFound) {
		t.Errorf("unknown method: %v", err)
	}
	err := c.Call(ctx, "fail", nil, nil)
	if !IsCode(err, CodeInternal) || strings.Contains(err.Error(), "secret") {
		t.Errorf("hidden internal error: %v", err)
	}
	if err := c.Call(ctx, "refuse", nil, nil); !IsCode(err, CodeRefused) {
		t.Errorf("refuse: %v", err)
	}
	if err := c.Call(ctx, "panic", nil, nil); !IsCode(err, CodeInternal) {
		t.Errorf("panic: %v", err)
	}
	if err := c.Call(ctx, "huge", nil, nil); !IsCode(err, CodeTooLarge) {
		t.Errorf("huge result: %v", err)
	}
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if err := c.Call(tctx, "slow", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
	cancel()
	// The connection survives a timed-out call; the late answer is dropped.
	if err := c.Call(ctx, "add", addArgs{1, 1}, &sum); err != nil || sum != 2 {
		t.Errorf("after a timeout: %d %v", sum, err)
	}
	if err := c.Notify("note", map[string]string{"from": "client"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "back", nil, nil); err != nil {
		t.Fatal(err)
	}
	notified.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != `{"from":"client"}` || got[1] != `client:{"from":"server"}` {
		t.Errorf("notifications: %q", got)
	}
}

func TestExposeErrors(t *testing.T) {
	_, c := pair(t, Options{ExposeErrors: true, Handlers: map[string]Handler{
		"fail": func(context.Context, *Conn, json.RawMessage) (any, error) { return nil, errors.New("disk full") },
	}}, Options{})
	if err := c.Call(context.Background(), "fail", nil, nil); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("%v", err)
	}
}

// rawPeer talks to a Conn with raw lines.
func rawPeer(t *testing.T, o Options) (net.Conn, *bufio.Reader, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	c := NewConn(a, o)
	t.Cleanup(func() { c.Close(); b.Close() })
	return b, bufio.NewReader(b), c
}

func TestMalformedInput(t *testing.T) {
	echo := Options{Handlers: map[string]Handler{
		"echo": func(_ context.Context, _ *Conn, p json.RawMessage) (any, error) { return p, nil },
	}}
	w, r, c := rawPeer(t, echo)
	send := func(s string) {
		t.Helper()
		_ = w.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := w.Write([]byte(s + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() RPCMessage {
		t.Helper()
		_ = w.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := ReadLine(r, MaxMessage)
		if err != nil {
			t.Fatal(err)
		}
		var m RPCMessage
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	send("garbage")
	if m := recv(); m.Error == nil || m.Error.Code != CodeParseError || string(m.ID) != "null" {
		t.Errorf("garbage: %+v", m)
	}
	send(`{"jsonrpc":"2.0","id":7,"method":"echo","bogus":1}`)
	if m := recv(); m.Error == nil || m.Error.Code != CodeParseError {
		t.Errorf("unknown field: %+v", m)
	}
	send(`{"jsonrpc":"1.0","id":8,"method":"echo"}`)
	if m := recv(); m.Error == nil || m.Error.Code != CodeInvalidRequest || string(m.ID) != "8" {
		t.Errorf("wrong version: %+v", m)
	}
	// A response nobody asked for and a notification nobody handles are
	// dropped silently; the connection still works.
	send(`{"jsonrpc":"2.0","id":99,"result":1}`)
	send(`{"jsonrpc":"2.0","method":"unheard"}`)
	send(`{"jsonrpc":"2.0","id":9,"method":"echo","params":{"a":1}}`)
	if m := recv(); m.Error != nil || string(m.ID) != "9" || string(m.Result) != `{"a":1}` {
		t.Errorf("after junk: %+v", m)
	}
	// A line above the limit ends the connection.
	go func() { _, _ = w.Write([]byte(strings.Repeat("x", MaxMessage+10) + "\n")) }()
	select {
	case <-c.Done():
		if !errors.Is(c.Err(), ErrTooLarge) {
			t.Errorf("closed with %v", c.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an oversized line did not close the connection")
	}
}

func TestHalfClose(t *testing.T) {
	// A peer that sends a request and closes its side still gets the answer.
	ua, ub := unixPair(t)
	c := NewConn(ua, Options{Handlers: map[string]Handler{
		"ping": func(context.Context, *Conn, json.RawMessage) (any, error) { return "pong", nil },
	}})
	defer c.Close()
	if _, err := ub.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	ub.(*net.UnixConn).CloseWrite()
	_ = ub.SetReadDeadline(time.Now().Add(5 * time.Second))
	out, err := io.ReadAll(ub)
	if err != nil || !strings.Contains(string(out), `"result":"pong"`) {
		t.Errorf("%q %v", out, err)
	}
}

func unixPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	dir := t.TempDir()
	ln, err := net.Listen("unix", dir+"/s")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	b, err := net.Dial("unix", dir+"/s")
	if err != nil {
		t.Fatal(err)
	}
	a := <-ch
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestClosePending(t *testing.T) {
	block := make(chan struct{})
	s, c := pair(t, Options{Handlers: map[string]Handler{
		"wait": func(context.Context, *Conn, json.RawMessage) (any, error) { <-block; return 1, nil },
	}}, Options{})
	defer close(block)
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "wait", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	s.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("pending call: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a pending call did not fail when the peer closed")
	}
	if err := c.Call(context.Background(), "wait", nil, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("call on a closed connection: %v", err)
	}
}

func TestSessionViewApply(t *testing.T) {
	mk := func(seq int64, typ string, data any) Event {
		e, _ := New("s0000000001aaaaaaaaaa", typ, data, time.Unix(1000+seq, 0))
		e.Seq, e.At = seq, time.Unix(2000+seq, 0).UTC().Format(time.RFC3339Nano)
		return e
	}
	v := NewSessionView("r", "p", "s0000000001aaaaaaaaaa")
	for _, e := range []Event{
		mk(1, TypeSessionStart, SessionStart{Harness: "fake", Worktree: "main"}),
		mk(2, TypeQuestion, Question{ID: "q", Text: "which?"}),
		mk(3, TypeState, State{State: StateBlocked, Kind: KindQuestion}),
	} {
		v.Apply(&e)
	}
	if !v.NeedsYou() || v.Question == nil || v.Harness != "fake" || v.LastSeq != 3 || v.Events != 3 {
		t.Errorf("%+v", v)
	}
	end := mk(4, TypeSessionEnd, SessionEnd{Reason: "exit"})
	v.Apply(&end)
	after := mk(5, TypeState, State{State: StateWorking})
	v.Apply(&after)
	if v.Live || v.State != "ended" || v.EndReason != "exit" || v.LastSeq != 5 || v.Question != nil {
		t.Errorf("after the end: %+v", v)
	}
}
