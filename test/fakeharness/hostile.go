package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// hostileHelper listens on socket for d and misbehaves towards every
// connection in the given mode.
func hostileHelper(mode, socket string, d time.Duration) error {
	switch mode {
	case "garbage", "oversized", "malformed":
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	time.AfterFunc(d, func() { ln.Close() })
	for {
		c, err := ln.Accept()
		if err != nil {
			return nil
		}
		go misbehave(mode, c)
	}
}

func misbehave(mode string, c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if mode == "garbage" {
		_, _ = c.Write([]byte(strings.Repeat("\xff\x00 not yamux ", 4096)))
		_, _ = io.Copy(io.Discard, c)
		return
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	s, err := yamux.Server(c, cfg)
	if err != nil {
		return
	}
	defer s.Close()
	st, err := s.AcceptStream()
	if err != nil {
		return
	}
	_, _ = bufio.NewReader(st).ReadString('\n')
	if mode == "oversized" {
		_, _ = st.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"devc_version":"` + strings.Repeat("x", 2<<20) + `"}}` + "\n"))
	} else {
		_, _ = st.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{nope}\n"))
	}
	time.Sleep(5 * time.Second)
}

// agentCall sends one raw request line to the box agent at addr and returns
// the first line of the answer.
func agentCall(addr, method, params string) (string, error) {
	c, err := net.Dial("unix", addr)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := fmt.Fprintf(c, `{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`+"\n", method, params); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), err
}
