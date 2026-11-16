// Package client calls methods and holds the call chains of the path tests.
package client

import "example.com/m/proto"

// Caller sends a method.
type Caller interface {
	Call(method string) error
}

// Send sends MethodSend through an interface.
func Send(c Caller) error { return c.Call(proto.MethodSend) }

// Hint builds a message.
func Hint() proto.Msg { return proto.Msg{Method: proto.MethodPing} }

type conn struct{}

func (conn) Call(m string) error { return nil }

// A calls B.
func A() { B() }

// B calls C.
func B() { C() }

// C is a leaf.
func C() {}

// D is not connected.
func D() {}

var table = map[string]func(){"c": C}

// E calls through a dispatch table.
func E() { table["c"]() }

// T is a type that E2 uses.
type T struct{ F int }

// E2 uses a field of T.
func E2() int { var t T; return t.F }
