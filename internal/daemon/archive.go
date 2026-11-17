// The archive of durable state (work order C17 section 2): for every hub
// in the list an uploader runs beside the machine link, on its own
// goroutine and its own HTTP connections, so uploads never hold up event
// delivery or commands. It passes over the durable state of every realm
// with [sync] hub at an interval, rate-limited ([sync] upload_rate in
// config.toml), and resumes where the hub's stored lengths say. The work
// that needs containers (listing the box-writable harness state, bundling
// the context repository) runs through fleet.ContainerWorkers.

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/mb706/devc/internal/fleet"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/hubnode"
	"github.com/mb706/devc/internal/proto"
)

// archiveWorkers is the workers of the current configuration.
func (d *Daemon) archiveWorkers(e *env) fleet.Workers {
	if d.o.ArchiveWorkers != nil {
		return d.o.ArchiveWorkers(e.cfg, e.eng)
	}
	return &fleet.ContainerWorkers{Config: e.cfg, Engine: e.eng, Executable: d.o.Executable, Version: d.version}
}

// newUploader prepares the uploader of a hub link.
func (d *Daemon) newUploader(url string) *fleet.Uploader {
	e := d.current()
	p := e.cfg.Paths
	host := &gitx.Host{Runner: d.o.Runner, HooksDir: p.NoHooksDir(), Background: true,
		Init: func() error { return gitx.EnsureEmptyDir(p.NoHooksDir(), p.Ensure) }}
	u := fleet.NewUploader(p, url, hubnode.ID(url), host, d.scanner, fleet.NewRateLimiter(e.cfg.Global.UploadRate()), d.log)
	return u
}

// archiveLoop runs the uploads of one hub until the link is stopped.
func (l *hubLink) archiveLoop(ctx context.Context) {
	d := l.m.d
	u := l.uploader
	lastErr := ""
	for {
		e := d.current()
		creds, err := hubnode.Load(e.cfg.Paths, l.url)
		switch {
		case err != nil:
			u.SetState(proto.ArchiveNoCreds, err.Error())
		default:
			cl, cerr := fleet.NewClient(creds)
			if cerr != nil {
				u.SetState(proto.ArchiveError, cerr.Error())
				break
			}
			u.Rate.SetRate(e.cfg.Global.UploadRate())
			err = u.Pass(ctx, e.cfg, cl, d.archiveWorkers(e))
			cl.Close()
			if ctx.Err() != nil {
				return
			}
			msg := ""
			if err != nil && !errors.Is(err, fleet.ErrPaused) {
				msg = err.Error()
			}
			if msg != lastErr {
				if msg != "" {
					d.log.Warn("archive upload", "hub", l.url, "err", msg)
				} else if lastErr != "" {
					d.log.Info("archive upload works again", "hub", l.url)
				}
				lastErr = msg
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.o.ArchiveInterval):
		case <-l.archiveWake:
		}
	}
}

// wakeArchive starts a pass of every uploader now (tests, devc sync).
func (m *hubManager) wakeArchive() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.links {
		select {
		case l.archiveWake <- struct{}{}:
		default:
		}
	}
}

// archiveStatus reports every listed hub's archive.
func (m *hubManager) archiveStatus() []proto.ArchiveHubStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []proto.ArchiveHubStatus{}
	for _, u := range m.order {
		if l := m.links[u]; l != nil && l.uploader != nil {
			out = append(out, l.uploader.Status())
		} else {
			out = append(out, proto.ArchiveHubStatus{URL: u, State: proto.ArchiveStarting, Realms: []proto.ArchiveRealmStatus{}})
		}
	}
	return out
}

// apiArchiveStatus serves archive.status on the local socket: with
// {"wake": true} every uploader starts a pass first.
func (d *Daemon) apiArchiveStatus(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var p struct {
		Wake bool `json:"wake"`
	}
	if len(params) > 0 && string(params) != "null" {
		if err := proto.DecodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	d.fresh()
	if p.Wake {
		d.hubs.wakeArchive()
	}
	return proto.ArchiveStatusResult{Hubs: d.hubs.archiveStatus()}, nil
}
