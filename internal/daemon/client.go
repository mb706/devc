// This file implements the client of the daemon's API that the CLI uses.

package daemon

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// ErrNotRunning is returned by Dial when no daemon answers.
var ErrNotRunning = errors.New("the devc daemon is not running")

// Client is a connection to the daemon's API.
type Client struct {
	*proto.Conn
	Info proto.DaemonInfo
}

// Dial connects to the daemon of a runtime directory and asks for its
// info. notify handles the notifications of a subscription (may be nil).
func Dial(ctx context.Context, runtime string, notify map[string]proto.NotifyHandler) (*Client, error) {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(dctx, "unix", SocketPath(runtime))
	if err != nil {
		return nil, ErrNotRunning
	}
	conn := proto.NewConn(c, proto.Options{Notify: notify, CallTimeout: 5 * time.Minute, Context: ctx})
	cl := &Client{Conn: conn}
	ictx, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	if err := conn.Call(ictx, proto.MethodDaemonInfo, nil, &cl.Info); err != nil {
		conn.Close()
		return nil, err
	}
	return cl, nil
}

// Serves reports whether the daemon works on the given configuration and
// state directories: a CLI with other directories must not use it.
func (c *Client) Serves(configDir, stateDir string) bool {
	return filepath.Clean(c.Info.ConfigDir) == filepath.Clean(configDir) && filepath.Clean(c.Info.StateDir) == filepath.Clean(stateDir)
}

// Sessions lists sessions, following every page.
func (c *Client) Sessions(ctx context.Context, p proto.SessionsListParams) (proto.SessionsListResult, error) {
	var all proto.SessionsListResult
	for {
		var res proto.SessionsListResult
		if err := c.Call(ctx, proto.MethodSessionsList, p, &res); err != nil {
			return all, err
		}
		all.Sessions = append(all.Sessions, res.Sessions...)
		all.Warnings = append(all.Warnings, res.Warnings...)
		if !res.More || len(res.Sessions) == 0 {
			return all, nil
		}
		last := res.Sessions[len(res.Sessions)-1]
		p.After, p.Sync = proto.SessionKey(last.Ref, last.Session), false
	}
}

// Events reads all events of a session with seq above from (the ID must
// be unique among the daemon's sessions; EventsOf names the project).
func (c *Client) Events(ctx context.Context, session string, from int64) (proto.EventsReadResult, error) {
	return c.EventsOf(ctx, "", session, from)
}

// EventsOf reads all events of a session of project ref with seq above
// from.
func (c *Client) EventsOf(ctx context.Context, ref, session string, from int64) (proto.EventsReadResult, error) {
	var all proto.EventsReadResult
	for {
		var res proto.EventsReadResult
		if err := c.Call(ctx, proto.MethodEventsRead, proto.EventsReadParams{Session: session, Ref: ref, From: from}, &res); err != nil {
			return all, err
		}
		all.Ref, all.LastSeq = res.Ref, res.LastSeq
		all.Events = append(all.Events, res.Events...)
		if !res.More || len(res.Events) == 0 {
			return all, nil
		}
		from = res.Events[len(res.Events)-1].Seq
	}
}
