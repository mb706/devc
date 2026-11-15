//go:build !amd64

package boxside

// probeSocketcall: only amd64 boxes run 32-bit x86 programs here.
func probeSocketcall() (errno int, supported bool) { return 0, false }
