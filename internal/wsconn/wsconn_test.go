package wsconn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/proto"
)

// pair returns a server-side raw WebSocket and a client-side codec
// connection over it.
func pair(t *testing.T, handlers map[string]proto.Handler) (*websocket.Conn, *proto.Conn) {
	t.Helper()
	srvc := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		srvc <- ws
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := proto.NewConn(New(context.Background(), ws), proto.Options{Handlers: handlers})
	t.Cleanup(func() { c.Close() })
	return <-srvc, c
}

func TestFrames(t *testing.T) {
	raw, c := pair(t, map[string]proto.Handler{
		"echo": func(_ context.Context, _ *proto.Conn, p json.RawMessage) (any, error) { return p, nil },
	})
	ctx := context.Background()
	// One request per text frame, answered in one text frame without a
	// newline.
	if err := raw.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":1,"method":"echo","params":{"a":1}}`)); err != nil {
		t.Fatal(err)
	}
	typ, b, err := raw.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(b) != `{"jsonrpc":"2.0","id":1,"result":{"a":1}}` {
		t.Errorf("answer %v %q %v", typ, b, err)
	}
	// Two objects in one frame end the connection.
	raw.Write(ctx, websocket.MessageText, []byte("{\"jsonrpc\":\"2.0\",\"method\":\"x\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"y\"}"))
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a frame with two objects did not end the connection")
	}
}

func TestBinaryAndOversized(t *testing.T) {
	raw, c := pair(t, nil)
	raw.Write(context.Background(), websocket.MessageBinary, []byte(`{}`))
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a binary frame did not end the connection")
	}
	raw2, c2 := pair(t, nil)
	go raw2.Write(context.Background(), websocket.MessageText, []byte(`{"jsonrpc":"2.0","method":"`+strings.Repeat("a", proto.MaxMessage)+`"}`))
	select {
	case <-c2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("an oversized frame did not end the connection")
	}
}

func TestCallsBothWays(t *testing.T) {
	raw, c := pair(t, nil)
	// The raw side answers a call from the codec side.
	go func() {
		_, b, err := raw.Read(context.Background())
		if err != nil {
			return
		}
		var m proto.RPCMessage
		json.Unmarshal(b, &m)
		raw.Write(context.Background(), websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":[1,2]}`))
	}()
	var out []int
	if err := c.Call(context.Background(), "list", nil, &out); err != nil || len(out) != 2 {
		t.Errorf("call: %v %v", out, err)
	}
}
