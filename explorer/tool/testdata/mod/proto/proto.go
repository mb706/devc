// Package proto is the synthetic protocol of the explorer's tests.
package proto

// Methods.
const (
	MethodPing   = "x.ping"
	MethodSend   = "x.send"
	MethodListed = "x.listed"
	MethodBuilt  = "x.built"
)

// Handler handles one method.
type Handler func(params string) (string, error)

// Msg is a message.
type Msg struct {
	Method string
}
