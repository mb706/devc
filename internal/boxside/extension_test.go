package boxside

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// extClient is a stand-in for pi's extension: it registers, then answers
// input.submit by recording the text.
func extClient(t *testing.T, sock, session string) (*bufio.Reader, net.Conn, string) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": proto.MethodInputRegister,
		"params": proto.InputRegisterParams{Session: session, Harness: "pi", Version: 1}})
	c.Write(append(b, '\n'))
	r := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return r, c, line
}

func TestExtensionRegistration(t *testing.T) {
	dir := t.TempDir()
	proc := filepath.Join(dir, "proc")
	sid, other := names.NewSessionID(), names.NewSessionID()
	// The peer is this test process; its "environment" names sid.
	envDir := filepath.Join(proc, strconv.Itoa(os.Getpid()))
	os.MkdirAll(envDir, 0o755)
	os.WriteFile(filepath.Join(envDir, "environ"), []byte("PATH=/bin\x00DEVC_SESSION="+sid+"\x00"), 0o644)

	a := &Agent{o: AgentOptions{ProcDir: proc, Log: io.Discard}}
	sock := filepath.Join(dir, "s")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			go a.relay(c)
		}
	}()

	ctx := context.Background()
	submit := func(session, text, mode string) (proto.InputSubmitReply, error) {
		b, _ := json.Marshal(proto.BoxInputParams{Session: session, Text: text, Mode: mode})
		res, err := a.handleSubmit(ctx, nil, b)
		if err != nil {
			return proto.InputSubmitReply{}, err
		}
		return res.(proto.InputSubmitReply), nil
	}
	if _, err := submit(sid, "x", proto.InputQueue); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("without an extension: %v", err)
	}

	// Registering for another session than the process's is refused.
	_, _, line := extClient(t, sock, other)
	if !strings.Contains(line, "belongs to session") {
		t.Errorf("foreign registration: %s", line)
	}

	r, c, line := extClient(t, sock, sid)
	if !strings.Contains(line, `"result"`) {
		t.Fatalf("registration: %s", line)
	}
	got := make(chan proto.InputSubmitParams, 2)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			var m proto.RPCMessage
			json.Unmarshal([]byte(line), &m)
			var p proto.InputSubmitParams
			json.Unmarshal(m.Params, &p)
			got <- p
			res := proto.InputSubmitReply{Accepted: true, Delivered: "follow_up"}
			if p.Text == "refuse" {
				b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "busy\x1b[31m"}})
				c.Write(append(b, '\n'))
				continue
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": res})
			c.Write(append(b, '\n'))
		}
	}()
	time.Sleep(100 * time.Millisecond)
	ex, _ := a.handleExtension(ctx, nil, json.RawMessage(`{"session":"`+sid+`"}`))
	if e := ex.(proto.BoxExtension); !e.Connected || e.Harness != "pi" {
		t.Errorf("box.input.extension: %+v", e)
	}
	rep, err := submit(sid, "hello", proto.InputQueue)
	if err != nil || !rep.Accepted || rep.Delivered != "follow_up" {
		t.Errorf("submit: %+v %v", rep, err)
	}
	if p := <-got; p.Text != "hello" || p.Mode != proto.InputQueue {
		t.Errorf("the extension got %+v", p)
	}
	if _, err := submit(sid, "refuse", proto.InputSteer); !proto.IsCode(err, proto.CodeRefused) || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("refused submit: %v", err)
	}
	if _, err := submit(sid, "x", "sideways"); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("bad mode: %v", err)
	}
	// The extension goes away: it is no longer used.
	c.Close()
	time.Sleep(200 * time.Millisecond)
	if _, err := submit(sid, "x", proto.InputQueue); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("after the extension left: %v", err)
	}
}
