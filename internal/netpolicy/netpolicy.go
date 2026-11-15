// Package netpolicy decides how a box is connected to the network (spec
// section 11) and enforces it from the host: the engine arguments of each
// level, the firewall rules of level internet and their installation into
// the box's network namespace, the canary that checks them from outside,
// and the port forwarder (`devc forward`) with its bookkeeping.
//
// Security rules: enforcement sits outside the box (R6). The rules are
// installed by the host into the box's network namespace between `podman
// init` and `podman start`, before any box process runs, and a process in
// the box has no capability to list or change them. Readiness is bound to
// that namespace and that start (Binding), never to anything the box can
// write. A level that cannot be enforced fails closed (README rule 6):
// `allowlist` is refused, and a box of level internet whose rules cannot be
// installed, bound or verified is not started. Every command is an argument
// vector (rule 3); the rules reach nft on stdin. Files the engine writes for
// the box (resolv.conf, hosts) are read with a size cap, without following
// symlinks, before the box starts, and parsed strictly (rule 2); resolver
// exceptions go only to addresses the host computes from trusted inputs
// (TrustedResolvers). The forwarder runs the host's own devc binary inside
// the box's user and network namespaces only, entered through descriptors
// pinned when the forward starts; it never enters the box's mount
// namespace, so nothing from the box is executed.
package netpolicy

import (
	"fmt"
	"net/netip"
)

// Network levels.
const (
	None      = "none"
	Allowlist = "allowlist"
	Internet  = "internet"
	Open      = "open"
)

// Names of the nftables objects. C13 builds on them: keep them stable.
const (
	// Table is the inet table holding devc's rules in a box's namespace.
	Table = "devc"
	// AllowSet holds IPv4 address and port exceptions with timeouts.
	AllowSet = "allow4"
	// Chain is the output chain.
	Chain = "output"
)

// The sentinel: TCP to SentinelAddr:SentinelPort is answered with a reset
// by devc's rules. Box init connects to it before reporting ready: an
// immediate "connection refused" proves the rules are loaded. 192.0.2.1 is
// TEST-NET-1 (RFC 5737); without the rules a connection times out or is
// unreachable, never refused.
var SentinelAddr = netip.MustParseAddr("192.0.2.1")

// SentinelPort is the TCP port of the sentinel (discard).
const SentinelPort = 9

// HostAliases are the names the engine gives the host inside a box.
var HostAliases = []string{"host.containers.internal", "host.docker.internal"}

// Decision is how a box of a given level is created and started.
type Decision struct {
	// Args are added to `podman create`; empty means the engine default.
	Args []string
	// Rules is set when the host must install firewall rules between init
	// and start, and check them from outside after the start.
	Rules bool
}

// ipv6Off are the sysctls that switch IPv6 off in a box's network
// namespace. The rules drop IPv6 as well; the sysctls make sure no address
// is configured at all.
var ipv6Off = []string{
	"--sysctl", "net.ipv6.conf.all.disable_ipv6=1",
	"--sysctl", "net.ipv6.conf.default.disable_ipv6=1",
}

// BoxArgs returns the decision for a level, or an error for a level that
// cannot be enforced.
func BoxArgs(level string) (Decision, error) {
	switch level {
	case None:
		return Decision{Args: []string{"--network", "none"}}, nil
	case Open:
		return Decision{}, nil
	case Internet:
		return Decision{Args: append([]string(nil), ipv6Off...), Rules: true}, nil
	case Allowlist:
		return Decision{}, fmt.Errorf("network level %q is not available yet and devc does not fall back to a wider network: "+
			"set the level to \"internet\", \"none\" or \"open\" (network.level in project.toml or network.default in realm.toml)", level)
	}
	return Decision{}, fmt.Errorf("unknown network level %q", level)
}

// Enforced reports whether boxes of this level can be created; the
// sandbox description must only ever describe such a level.
func Enforced(level string) bool {
	_, err := BoxArgs(level)
	return err == nil
}
