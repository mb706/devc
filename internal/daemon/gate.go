// The SSH gate (work order C10b section 2): a host.gate stream from a box
// becomes `sshd -i` with the stream as standard input and output. The
// project, and so the grant and the key sshd accepts, is the one the
// channel is bound to; the box names only the host, which must be granted
// to that project. sshd runs the forced command devc gate-exec for
// everything, has every kind of forwarding switched off, and the master
// connection's control socket stays in <runtime>/gate, which no box can
// reach.

package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/gate"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// gateServer returns the gate's sshd launcher for a configuration.
func (d *Daemon) gateServer(e *env) (*gate.Server, error) {
	p := e.cfg.Paths
	sshd := d.o.SSHD
	if sshd == "" {
		found, err := execx.LookPath("sshd")
		if err != nil {
			return nil, errors.New("sshd is not installed on the host, so the SSH gate is not available; ask the user to install openssh-server (the service itself need not run)")
		}
		sshd = found
	}
	rt, err := p.RuntimeDir()
	if err != nil {
		return nil, err
	}
	return &gate.Server{Paths: p, SSHD: sshd, Exec: gate.ExecTarget{
		Executable: d.o.Executable, ConfigDir: p.ConfigDir(), StateDir: p.StateDir(), RuntimeDir: rt, SSHConfig: d.o.SSHConfig,
	}}, nil
}

// serveGate answers a host.gate request and, when the host is granted to
// the channel's project, runs the gate's sshd on the stream.
func (w *boxWorker) serveGate(ctx context.Context, st *yamux.Stream, br *bufio.Reader, m *proto.RPCMessage) {
	raw := rawStream{st: st, stall: streamStall}
	defer raw.guard(ctx, w.d.log)()
	fail := func(code int, format string, args ...any) {
		_ = respond(st, m.ID, nil, proto.Errorf(code, format, args...))
	}
	var p proto.GateParams
	if err := proto.DecodeParams(m.Params, &p); err != nil {
		var pe *proto.Error
		errors.As(err, &pe)
		_ = respond(st, m.ID, nil, pe)
		return
	}
	if !config.ValidHostAlias(p.Host) {
		fail(proto.CodeInvalidParams, "%q is not a valid SSH host alias", proto.Printable(proto.Truncate(p.Host, 80)))
		return
	}
	d := w.d
	e := d.current()
	ref := w.ref
	r, perr := resolvedOf(e, ref)
	if perr != nil {
		_ = respond(st, m.ID, nil, perr)
		return
	}
	if _, ok := r.Grants.SSHGrantFor(p.Host); !ok {
		d.log.Info("gate: host not granted", "box", names.BoxName(ref), "host", p.Host)
		fail(proto.CodeRefused, "%s is not granted to %s; the user can add it to grants.ssh in the project's configuration", p.Host, ref)
		return
	}
	// Granted now and when the box started (finding C1-02).
	caps, perr := d.startedWith(e.cfg.Paths, ref, w.containerID)
	if perr != nil {
		_ = respond(st, m.ID, nil, perr)
		return
	}
	if _, _, ok := caps.SSHGrant(r, p.Host); !ok {
		d.log.Info("gate: host granted after the box started", "box", names.BoxName(ref), "host", p.Host)
		fail(proto.CodeRefused, "%s was granted to %s after its box started; ask the user to bring the box up again ('devc down %s' and 'devc up %s')", p.Host, ref, ref, ref)
		return
	}
	srv, err := d.gateServer(e)
	if err != nil {
		fail(proto.CodeRefused, "%s", err.Error())
		return
	}
	conf, err := srv.Prepare(r.BoxID, p.Host)
	switch {
	case errors.Is(err, gate.ErrNoBoxKey), errors.Is(err, gate.ErrNoHostKey):
		fail(proto.CodeRefused, "this box was started without a key for the SSH gate; ask the user to restart it ('devc down %s' and 'devc up %s')", ref, ref)
		return
	case err != nil:
		d.log.Error("gate: preparing sshd", "box", names.BoxName(ref), "err", err)
		fail(proto.CodeInternal, "the host could not prepare the SSH gate; see the daemon's log")
		return
	}
	defer os.Remove(conf)
	if err := respond(st, m.ID, proto.GateReply{Host: p.Host}, nil); err != nil {
		return
	}
	start := time.Now()
	code, stderr, err := runGateSSHD(ctx, d.log, d.o.Runner, srv.Command(conf), raw, br)
	// sshd is gone: its configuration goes, and the stream ends for both
	// copies (finding C1-09).
	_ = os.Remove(conf)
	raw.abort()
	d.log.Info("gate", "box", names.BoxName(ref), "host", p.Host, "exit", code, "err", err,
		"duration", time.Since(start).Round(time.Millisecond), "log", proto.Printable(proto.Truncate(stderr, 600)))
}

// runGateSSHD runs sshd with the stream as its standard input and output.
// Standard input is fed through a pipe of the daemon's own, so the run
// ends when sshd exits even while the box keeps its side open.
func runGateSSHD(ctx context.Context, log *slog.Logger, runner execx.Runner, cmd execx.Cmd, st io.Writer, br io.Reader) (int, string, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return -1, "", err
	}
	defer pr.Close()
	goTask(log, &task{what: "the input of the SSH gate"}, func() {
		defer pw.Close()
		_, _ = io.Copy(pw, br)
	})
	var stderr cappedBuffer
	stderr.max = 16 << 10
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pr, st, &stderr
	code, err := runner.Interactive(ctx, cmd)
	pw.Close()
	if err != nil {
		return code, stderr.String(), fmt.Errorf("sshd: %w", err)
	}
	return code, stderr.String(), nil
}
