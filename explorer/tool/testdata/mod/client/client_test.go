package client

// fakeCaller is a test double; production paths must not go through it.
type fakeCaller struct{}

func (fakeCaller) Call(m string) error { return nil }

func exerciseA() { A() }
