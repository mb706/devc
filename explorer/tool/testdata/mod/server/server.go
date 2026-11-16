// Package server registers handlers in every way the classifier knows.
package server

import "example.com/m/proto"

// Server serves methods.
type Server struct{}

func (s *Server) handlers() map[string]proto.Handler {
	return map[string]proto.Handler{
		proto.MethodPing:  s.ping,
		proto.MethodSend:  s.factory("a"),
		proto.MethodBuilt: s.builder(proto.MethodBuilt),
	}
}

func (s *Server) more(m map[string]proto.Handler) {
	m[proto.MethodPing] = s.ping
}

func (s *Server) ping(p string) (string, error) { return p, nil }

func (s *Server) factory(x string) proto.Handler {
	return func(p string) (string, error) { return s.helper(p + x), nil }
}

func (s *Server) builder(method string) proto.Handler {
	return func(p string) (string, error) { return method, nil }
}

func (s *Server) helper(p string) string { return p }

func (s *Server) dispatch(m string) {
	switch m {
	case proto.MethodListed:
		s.helper(m)
	}
}

var allowed = []string{proto.MethodListed}
