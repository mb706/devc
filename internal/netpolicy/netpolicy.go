// Package netpolicy decides how a box is connected to the network (spec
// section 11). It is the one place that maps a network level to engine
// arguments; firewall rules, the forwarder, the gateway and exceptions are
// added here by later chunks.
//
// Security rules: enforcement sits outside the box (R6). Until the
// `internet` and `allowlist` levels are enforced, BoxArgs says so in a
// warning that devc up prints; it never pretends a level is in force.
package netpolicy

import "fmt"

// Network levels.
const (
	None      = "none"
	Allowlist = "allowlist"
	Internet  = "internet"
	Open      = "open"
)

// Decision is how a box of a given level is created.
type Decision struct {
	// Args are added to `podman create`; empty means the engine default.
	Args []string
	// Warning, when set, is printed by devc up.
	Warning string
}

// BoxArgs returns the decision for a level.
func BoxArgs(level string) (Decision, error) {
	switch level {
	case None:
		return Decision{Args: []string{"--network", "none"}}, nil
	case Open:
		return Decision{}, nil
	case Internet, Allowlist:
		return Decision{Warning: fmt.Sprintf("network level %q is not enforced yet: this box has the engine's default, unrestricted network "+
			"(LAN and host services included) until a later devc version adds the rules", level)}, nil
	}
	return Decision{}, fmt.Errorf("unknown network level %q", level)
}
