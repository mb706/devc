// This file is the client side of the agent's in-box socket: hints,
// requests to the host, and the box's description for box.hello.

package boxside

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// hintTimeout bounds what a hint may cost the hook that sends it.
const hintTimeout = 200 * time.Millisecond

// SendHint tells the host, through the box agent, that the spool of
// session has new data. It never fails visibly and returns within
// hintTimeout: the spool file is the source of truth, and a lost hint only
// costs latency.
func SendHint(session string) { SendHintTo(AgentAbstract, session) }

// SendHintTo is SendHint to the agent at addr; tests pass their own.
func SendHintTo(addr, session string) {
	if !names.ValidSessionID(session) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hintTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", addr)
	if err != nil {
		return
	}
	defer c.Close()
	params, _ := json.Marshal(proto.Hint{Session: session})
	b, _ := json.Marshal(&proto.RPCMessage{JSONRPC: "2.0", Method: proto.MethodHostHint, Params: params})
	_ = c.SetWriteDeadline(time.Now().Add(hintTimeout))
	_, _ = c.Write(append(b, '\n'))
}

// hintHost is replaced by tests.
var hintHost = SendHint

// AgentCall makes one request to the host through the box agent. Without a
// host connection the error is a *proto.Error with CodeHostUnavailable; a
// box without an agent gives a dial error.
func AgentCall(ctx context.Context, method string, params, result any) error {
	return AgentCallAt(ctx, AgentAbstract, method, params, result)
}

// AgentCallAt is AgentCall to the agent at addr (tests).
func AgentCallAt(ctx context.Context, addr, method string, params, result any) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", addr)
	if err != nil {
		return err
	}
	conn := proto.NewConn(c, proto.Options{Context: ctx})
	defer conn.Close()
	return conn.Call(ctx, method, params, result)
}

// AgentHello describes this box for box.hello, from /etc/devc/box.json.
func AgentHello() (proto.HelloReply, error) {
	info, err := ReadInfo("/")
	if err != nil {
		return proto.HelloReply{}, err
	}
	return proto.HelloReply{Protocol: proto.ChannelVersion, DevcVersion: info.DevcVersion, Realm: info.Realm, Project: info.Project, Box: info.BoxName}, nil
}
