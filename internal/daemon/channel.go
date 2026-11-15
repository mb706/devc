package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/session"
)

// Channel limits.
const (
	// maxBoxStreams bounds the streams one box may have open towards the
	// host at once; more are closed at once.
	maxBoxStreams = 32
	// boxStreamIdle closes a box stream on which nothing arrived for that
	// long.
	boxStreamIdle = 10 * time.Minute
	// helloTimeout bounds the box's answer to box.hello.
	helloTimeout = 10 * time.Second
	// maxBackoff is the longest pause between connection attempts;
	// noAgentBackoff the longest while the box has no agent socket.
	maxBackoff     = 30 * time.Second
	noAgentBackoff = 4 * time.Second
)

// boxWorker follows one running box: its channel and its spool. It ends
// when its context is cancelled (the box stopped or was replaced).
type boxWorker struct {
	d           *Daemon
	ref         names.ProjectRef
	containerID string
	cancel      context.CancelFunc
	done        chan struct{}
	// hint wakes the ingester (host.hint).
	hint chan struct{}

	mu      sync.Mutex
	state   string
	detail  string
	since   time.Time
	helper  string
	sess    *yamux.Session
	ctrl    *proto.Conn
	streams chan struct{}
}

func (d *Daemon) startWorker(ref names.ProjectRef, containerID string) *boxWorker {
	ctx, cancel := context.WithCancel(d.ctx)
	w := &boxWorker{d: d, ref: ref, containerID: containerID, cancel: cancel, done: make(chan struct{}),
		hint: make(chan struct{}, 1), state: proto.ChannelConnecting, since: time.Now(), streams: make(chan struct{}, maxBoxStreams)}
	var wg sync.WaitGroup
	wg.Add(2)
	d.goSafe("channel "+ref.String(), func() { defer wg.Done(); w.channelLoop(ctx) })
	d.goSafe("ingest "+ref.String(), func() { defer wg.Done(); w.ingestLoop(ctx) })
	go func() { wg.Wait(); close(w.done) }()
	w.wake() // ingest what the box spooled while nobody watched
	return w
}

func (w *boxWorker) stop() {
	w.cancel()
	w.mu.Lock()
	if w.sess != nil {
		_ = w.sess.Close()
	}
	w.mu.Unlock()
}

func (w *boxWorker) setState(state, detail string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != state || w.detail != detail {
		w.since = time.Now()
		if state == proto.ChannelConnected {
			w.d.log.Info("channel connected", "box", names.BoxName(w.ref), "helper", proto.Printable(w.helper))
		} else if state != proto.ChannelConnecting {
			w.d.log.Info("channel not connected", "box", names.BoxName(w.ref), "state", state, "detail", detail)
		}
	}
	w.state, w.detail = state, detail
}

// view fills the channel fields of a box view.
func (w *boxWorker) view(v *proto.BoxView) {
	w.mu.Lock()
	defer w.mu.Unlock()
	since := w.since
	v.Channel, v.ChannelDetail, v.ChannelSince, v.HelperVersion = w.state, w.detail, &since, w.helper
}

func (w *boxWorker) connected() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state == proto.ChannelConnected && w.ctrl != nil
}

// wake asks the ingester for a run now.
func (w *boxWorker) wake() {
	select {
	case w.hint <- struct{}{}:
	default:
	}
}

func (w *boxWorker) channelLoop(ctx context.Context) {
	first := min(time.Second, w.d.o.MaxBackoff)
	backoff := first
	for ctx.Err() == nil {
		err := w.connect(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			backoff = first // connected and served until the box went away
		case errors.Is(err, ErrNoSocket):
			w.setState(proto.ChannelNoAgent, "the box has no agent socket (restart it to get one)")
		case errors.Is(err, ErrNotSocket), errors.Is(err, errRefused):
			w.setState(proto.ChannelRefused, proto.Printable(err.Error()))
		default:
			w.setState(proto.ChannelError, proto.Printable(err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		switch {
		case err == nil:
		case errors.Is(err, ErrNoSocket):
			// A box still starting has no socket yet, and looking costs
			// one fstatat: keep looking often.
			backoff = min(2*backoff, noAgentBackoff, w.d.o.MaxBackoff)
		default:
			backoff = min(2*backoff, w.d.o.MaxBackoff)
		}
	}
}

var errRefused = errors.New("refused")

// connect opens the box's agent socket, checks the peer, says hello and
// serves the channel until it closes. It returns nil after a channel that
// was established has ended.
func (w *boxWorker) connect(ctx context.Context) error {
	env := w.d.current()
	rt, err := env.cfg.Paths.BoxRuntimeDir(names.BoxID(w.ref))
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := DialBoxSocket(dctx, rt, boxside.AgentSocketName)
	cancel()
	if err != nil {
		return err
	}
	// The main PID (for the PID namespace check) comes from the engine,
	// which must still report this container running.
	ci, err := env.mgr.Find(ctx, w.ref)
	if err != nil || ci == nil || !ci.State.Running || ci.ID != w.containerID {
		conn.Close()
		if err == nil {
			err = errors.New("the box is not running")
		}
		return err
	}
	cred, err := PeerCred(conn)
	if err == nil {
		err = CheckPeer(w.d.procDir, cred.Pid, w.containerID, ci.State.Pid)
	}
	if err != nil {
		conn.Close()
		return fmt.Errorf("%w: %v", errRefused, err)
	}
	cfg := yamux.DefaultConfig()
	cfg.AcceptBacklog = maxBoxStreams
	cfg.StreamOpenTimeout = 15 * time.Second
	cfg.StreamCloseTimeout = time.Minute
	cfg.LogOutput = io.Discard
	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		conn.Close()
		return err
	}
	defer sess.Close()
	st, err := sess.OpenStream()
	if err != nil {
		// The peer passed the checks and then broke the protocol.
		return fmt.Errorf("%w: %v", errRefused, err)
	}
	ctrl := proto.NewConn(st, proto.Options{Context: ctx, Log: w.d.log})
	defer ctrl.Close()
	hctx, cancel := context.WithTimeout(ctx, w.d.o.HelloTimeout)
	var raw json.RawMessage
	err = ctrl.Call(hctx, proto.MethodBoxHello, proto.Hello{Protocol: proto.ChannelVersion, DevcVersion: w.d.version}, &raw)
	cancel()
	if err != nil {
		return fmt.Errorf("%w: box.hello failed: %v", errRefused, err)
	}
	var hello proto.HelloReply
	if err := proto.DecodeResult(raw, &hello); err != nil {
		return fmt.Errorf("%w: invalid box.hello reply: %v", errRefused, err)
	}
	if err := hello.Validate(); err != nil {
		return fmt.Errorf("%w: %v", errRefused, err)
	}
	if hello.Protocol != proto.ChannelVersion {
		return fmt.Errorf("%w: the helper speaks channel protocol %d, this daemon knows %d (restart the box after updating devc)", errRefused, hello.Protocol, proto.ChannelVersion)
	}
	if hello.Realm != w.ref.Realm || hello.Project != w.ref.Project {
		return fmt.Errorf("%w: the helper says it is the box of %s/%s", errRefused, proto.Printable(hello.Realm), proto.Printable(hello.Project))
	}
	w.mu.Lock()
	w.sess, w.ctrl, w.helper = sess, ctrl, proto.Printable(hello.DevcVersion)
	w.mu.Unlock()
	w.setState(proto.ChannelConnected, "")
	defer func() {
		w.mu.Lock()
		w.sess, w.ctrl = nil, nil
		w.mu.Unlock()
		w.setState(proto.ChannelConnecting, "the channel closed")
	}()
	w.wake()
	w.d.goSafe("streams "+w.ref.String(), func() { w.acceptStreams(ctx, sess) })
	select {
	case <-ctx.Done():
	case <-sess.CloseChan():
	case <-ctrl.Done():
	}
	return nil
}

// acceptStreams serves the streams the box opens (in-box clients relayed
// by the agent). Requests are attributed to this box by the channel they
// arrive on.
func (w *boxWorker) acceptStreams(ctx context.Context, sess *yamux.Session) {
	handlers := map[string]proto.Handler{
		proto.MethodHostPing: func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
			return proto.PingReply{Version: w.d.version, Instance: w.d.instance}, nil
		},
	}
	notify := map[string]proto.NotifyHandler{
		proto.MethodHostHint: func(_ context.Context, _ *proto.Conn, params json.RawMessage) {
			var h proto.Hint
			if proto.DecodeParams(params, &h) == nil {
				w.wake()
			}
		},
	}
	for {
		st, err := sess.AcceptStreamWithContext(ctx)
		if err != nil {
			return
		}
		select {
		case w.streams <- struct{}{}:
		default:
			_ = st.Close() // too many at once
			continue
		}
		c := proto.NewConn(st, proto.Options{Handlers: handlers, Notify: notify, Context: ctx, IdleTimeout: boxStreamIdle, Log: w.d.log})
		go func() {
			<-c.Done()
			<-w.streams
		}()
	}
}

// live asks the box for its live sessions over the channel; the answer is
// validated as strictly as `devc box session ps --json` output.
func (w *boxWorker) live(ctx context.Context) ([]session.Live, error) {
	w.mu.Lock()
	ctrl := w.ctrl
	w.mu.Unlock()
	if ctrl == nil {
		return nil, errors.New("the box channel is not connected")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var raw json.RawMessage
	if err := ctrl.Call(cctx, proto.MethodBoxSessions, nil, &raw); err != nil {
		return nil, fmt.Errorf("asking %s for its sessions: %w", names.BoxName(w.ref), err)
	}
	var reply proto.LiveReply
	if err := proto.DecodeResult(raw, &reply); err != nil {
		return nil, fmt.Errorf("parsing the live-session report: %w", err)
	}
	return session.ParseLive(reply.Sessions)
}
