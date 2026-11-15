// Package netpolicy decides how a box is connected to the network (spec
// section 11). It is the one place that maps a network level to engine
// arguments; firewall rules, the forwarder, the gateway and exceptions are
// added here by later chunks.
//
// Security rules: enforcement sits outside the box (R6), and a level that
// cannot be enforced fails closed (README rule 6). Until C07 installs the
// rules, `internet` and `allowlist` are refused rather than silently given
// an unrestricted network; only `none` and an explicit `open` work.
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
}

// BoxArgs returns the decision for a level, or an error for a level that
// cannot be enforced yet.
func BoxArgs(level string) (Decision, error) {
	switch level {
	case None:
		return Decision{Args: []string{"--network", "none"}}, nil
	case Open:
		return Decision{}, nil
	case Internet, Allowlist:
		return Decision{}, fmt.Errorf("network level %q is not implemented yet and devc does not fall back to an unrestricted network: "+
			"set the level to \"none\", or to \"open\" if this box may reach the LAN and host services "+
			"(network.level in project.toml or network.default in realm.toml)", level)
	}
	return Decision{}, fmt.Errorf("unknown network level %q", level)
}

// Enforced reports whether boxes of this level can be created; the
// sandbox description must only ever describe such a level.
func Enforced(level string) bool {
	_, err := BoxArgs(level)
	return err == nil
}
