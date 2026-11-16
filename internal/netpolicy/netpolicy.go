// Package netpolicy decides how a box is connected to the network (spec
// section 11) and enforces it from the host: the engine arguments of each
// level, the firewall rules of level internet and their installation into
// the box's network namespace, the canary that checks them from outside,
// the port forwarder (`devc forward`) with its bookkeeping, the allowlist
// proxy of level allowlist, network exceptions and the proxy's log.
//
// Security rules: enforcement sits outside the box (R6). The rules are
// installed by the host into the box's network namespace between `podman
// init` and `podman start`, before any box process runs, and a process in
// the box has no capability to list or change them. Readiness is bound to
// that namespace and that start (Binding), never to anything the box can
// write. A level that cannot be enforced fails closed (README rule 6): a
// box of level internet whose rules cannot be installed, bound or verified
// is not started, and a box of level allowlist has no network interface
// besides loopback, so without the host's proxy it has no network at all.
// The proxy (Proxy) runs on the host, refuses by default, resolves names
// itself, refuses IP literals, and checks every address it connects to
// against the private ranges and the host's own addresses, so neither a
// name that resolves to a private address nor one whose answer changes
// between check and connect reaches the LAN or the host. Exceptions are
// created only from the host (CLI or an approved request) and recorded in
// the state directory, which no box can write. Every command is an
// argument vector (rule 3); the rules reach nft on stdin. Files the engine
// writes for the box (resolv.conf, hosts) are read with a size cap, without
// following symlinks, before the box starts, and parsed strictly (rule 2);
// resolver exceptions go only to addresses the host computes from trusted
// inputs (TrustedResolvers). The forwarder runs the host's own devc binary
// inside the box's user and network namespaces only, entered through
// descriptors pinned when the forward starts; it never enters the box's
// mount namespace, so nothing from the box is executed.
//
// Files: netpolicy.go holds the levels and their engine arguments;
// rules.go renders the rules of level internet and reads the engine's
// resolver and hosts files; install.go installs and verifies the rules in
// a box's namespace; binding.go ties readiness to one start; resolvers.go
// computes the resolvers the host trusts and the backend's ways to the
// host; canary.go holds the sentinel check and the outside check;
// allowlist.go and presets.go parse allowlist entries; proxy.go is the
// allowlist proxy and netlog.go its log; exceptions.go keeps network
// exceptions and renders their set elements; forward.go and registry.go
// are `devc forward` and its records.
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

// Names of the nftables objects. Keep them stable: a running box keeps the
// table it was started with, and Verify and ApplyExceptions find it by
// these names.
const (
	// Table is the inet table holding devc's rules in a box's namespace.
	Table = "devc"
	// AllowSet holds IPv4 address and port exceptions with timeouts.
	AllowSet = "allow4"
	// LANSet holds the private ranges of a "lan" exception (an interval set
	// with timeouts), consulted after the host's own addresses are
	// rejected and before the private ranges are (work order C13).
	LANSet = "lan4"
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

// ProxyAddr is where a box of level allowlist reaches the host's proxy:
// the box agent listens there and relays every connection over the box
// channel (work order C13).
const ProxyAddr = "127.0.0.1:3128"

// ProxyURL is ProxyAddr as the proxy variables give it.
const ProxyURL = "http://" + ProxyAddr

// ProxyEnv is the environment of a box of level allowlist: the proxy for
// every tool that honours the usual variables (Node.js needs
// NODE_USE_ENV_PROXY), and nothing proxied for the box's own loopback.
var ProxyEnv = [][2]string{
	{"HTTP_PROXY", ProxyURL},
	{"HTTPS_PROXY", ProxyURL},
	{"http_proxy", ProxyURL},
	{"https_proxy", ProxyURL},
	{"NO_PROXY", "127.0.0.1,localhost"},
	{"no_proxy", "127.0.0.1,localhost"},
	{"NODE_USE_ENV_PROXY", "1"},
}

// Decision is how a box of a given level is created and started.
type Decision struct {
	// Args are added to `podman create`; empty means the engine default.
	Args []string
	// Env is added to the box's environment.
	Env [][2]string
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
		// No interface besides loopback: everything goes through the proxy,
		// which only the host's daemon answers.
		return Decision{Args: []string{"--network", "none"}, Env: ProxyEnv}, nil
	}
	return Decision{}, fmt.Errorf("unknown network level %q", level)
}

// Enforced reports whether boxes of this level can be created; the
// sandbox description must only ever describe such a level.
func Enforced(level string) bool {
	_, err := BoxArgs(level)
	return err == nil
}
