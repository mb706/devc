// This file holds the code of the flow tests: a function that returns to
// its caller, commands whose run literals share a local closure, a method
// looked up in a map and passed on, and functions passed as values.
package client

import "example.com/m/proto"

// Bank clears, plans, then updates; Clear updates too.
func Bank() { Clear(); Plan(); Update() }

// Clear updates.
func Clear() { Update() }

// Plan plans.
func Plan() {}

// Update is called by Bank and by Clear.
func Update() {}

// newCmds builds run functions as literals that call a local closure.
func newCmds() []func() error {
	send := func(m string) error { return Transport(m) }
	return []func() error{
		func() error { return send("one") },
		func() error { Plan(); return nil },
	}
}

// Transport sends.
func Transport(m string) error { return nil }

// methods maps names to methods.
var methods = map[string]string{"send": proto.MethodSend}

// Forward looks a method up and passes it on.
func Forward(c Caller, name string) error {
	m, ok := methods[name]
	if !ok {
		return nil
	}
	return c.Call(m)
}

// Lookup looks a method up and only compares it.
func Lookup(name string) bool { return methods[name] == "" }

// Register takes a function.
func Register(f func()) {}

// AHold passes Plan as a value.
func AHold() { Register(Plan) }

// ZDirect calls Plan.
func ZDirect() { Plan() }

// Both reaches Plan through AHold and through ZDirect.
func Both() { AHold(); ZDirect() }
