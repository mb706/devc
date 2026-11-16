package boxside

// Harness extensions (work order C16): a harness that can load code (pi's
// extensions) gets a small extension from devc's baseline that connects to
// the agent's in-box socket, registers for the session of the process it
// runs in (box.input.register) and then submits the user messages the
// daemon sends (box.input.submit -> input.submit) through the harness's
// own API instead of typing them into the terminal.
//
// Any process of the box can reach the socket, and the box is one trust
// domain; still, a registration is accepted only from a process whose
// initial environment names the session it registers for (the launcher
// exports DEVC_SESSION), so a stray client cannot take another session's
// input by mistake. The newest registration of a session wins.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// submitTimeout bounds one input.submit to an extension.
const submitTimeout = 15 * time.Second

// extensions are the registered extension connections by session.
type extensions struct {
	mu sync.Mutex
	m  map[string]*extConn
}

type extConn struct {
	conn    *proto.Conn
	harness string
}

func (e *extensions) set(session string, c *extConn) {
	e.mu.Lock()
	if e.m == nil {
		e.m = map[string]*extConn{}
	}
	old := e.m[session]
	e.m[session] = c
	e.mu.Unlock()
	if old != nil {
		old.conn.Close()
	}
}

func (e *extensions) remove(session string, c *extConn) {
	e.mu.Lock()
	if e.m[session] == c {
		delete(e.m, session)
	}
	e.mu.Unlock()
}

func (e *extensions) get(session string) *extConn {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.m[session]
}

// readWriter joins the buffered reader that already holds the client's
// first line with the connection it came from.
type readWriter struct {
	io.Reader
	c net.Conn
}

func (rw readWriter) Write(p []byte) (int, error)        { return rw.c.Write(p) }
func (rw readWriter) Close() error                       { return rw.c.Close() }
func (rw readWriter) SetWriteDeadline(t time.Time) error { return rw.c.SetWriteDeadline(t) }
func (rw readWriter) SetReadDeadline(t time.Time) error  { return rw.c.SetReadDeadline(t) }

// serveExtension handles a client whose first request is
// box.input.register; it returns when the extension goes away.
func (a *Agent) serveExtension(c *net.UnixConn, r *bufio.Reader, m *proto.RPCMessage) {
	reply := func(res any, perr *proto.Error) {
		msg := &proto.RPCMessage{JSONRPC: "2.0", ID: m.ID, Error: perr}
		if perr == nil {
			b, _ := json.Marshal(res)
			msg.Result = b
		}
		b, _ := json.Marshal(msg)
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write(append(b, '\n'))
		_ = c.SetWriteDeadline(time.Time{})
	}
	var p proto.InputRegisterParams
	if err := proto.DecodeParams(m.Params, &p); err != nil {
		var pe *proto.Error
		errors.As(err, &pe)
		reply(nil, pe)
		return
	}
	if !names.ValidSessionID(p.Session) || !names.Valid(p.Harness) {
		reply(nil, proto.Errorf(proto.CodeInvalidParams, "invalid session or harness"))
		return
	}
	cred, err := peerCred(c)
	if err == nil {
		err = a.sessionOf(cred.Pid, p.Session)
	}
	if err != nil {
		a.logf("refused an extension for %s: %v", p.Session, err)
		reply(nil, proto.Errorf(proto.CodeRefused, "%s", proto.Printable(err.Error())))
		return
	}
	reply(struct{}{}, nil)
	ec := &extConn{harness: p.Harness, conn: proto.NewConn(readWriter{Reader: r, c: c}, proto.Options{CallTimeout: submitTimeout})}
	a.ext.set(p.Session, ec)
	a.logf("the %s extension of %s connected", p.Harness, p.Session)
	<-ec.conn.Done()
	a.ext.remove(p.Session, ec)
}

// sessionOf checks that process pid of this box was started for session.
func (a *Agent) sessionOf(pid int32, session string) error {
	if pid <= 0 {
		return errors.New("the peer is not a process of this box")
	}
	proc := a.o.ProcDir
	if proc == "" {
		proc = "/proc"
	}
	b, err := readCapped(filepath.Join(proc, strconv.Itoa(int(pid)), "environ"), 1<<20)
	if err != nil {
		return fmt.Errorf("reading the environment of process %d: %w", pid, err)
	}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("DEVC_SESSION=")); ok {
			if string(v) == session {
				return nil
			}
			return fmt.Errorf("process %d belongs to session %s", pid, v)
		}
	}
	return fmt.Errorf("process %d was not started for a session", pid)
}

// handleSubmit serves box.input.submit: the message goes to the session's
// extension. Without one the answer is CodeNotFound, and the daemon types
// it instead.
func (a *Agent) handleSubmit(ctx context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.BoxInputParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if !names.ValidSessionID(p.Session) || (p.Mode != proto.InputQueue && p.Mode != proto.InputSteer) {
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid session or mode")
	}
	ec := a.ext.get(p.Session)
	if ec == nil {
		return nil, proto.Errorf(proto.CodeNotFound, "no extension is connected for this session")
	}
	ctx, cancel := context.WithTimeout(ctx, submitTimeout)
	defer cancel()
	var r proto.InputSubmitReply
	if err := ec.conn.Call(ctx, proto.MethodInputSubmit, proto.InputSubmitParams{Text: p.Text, Mode: p.Mode}, &r); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return nil, proto.Errorf(proto.CodeRefused, "the extension refused the message: %s", proto.Printable(proto.Truncate(pe.Message, 300)))
		}
		return nil, proto.Errorf(proto.CodeInternal, "the extension did not answer: %s", proto.Printable(proto.Truncate(err.Error(), 200)))
	}
	r.Detail = proto.Printable(proto.Truncate(r.Detail, 300))
	if len(r.Delivered) > 32 {
		r.Delivered = ""
	}
	return r, nil
}

// handleExtension serves box.input.extension.
func (a *Agent) handleExtension(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p proto.BoxInputParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if ec := a.ext.get(p.Session); ec != nil {
		return proto.BoxExtension{Connected: true, Harness: ec.harness}, nil
	}
	return proto.BoxExtension{}, nil
}
