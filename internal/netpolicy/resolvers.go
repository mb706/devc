// This file computes, from the host's files and the engine's inspect data
// only, which resolvers a box of level internet may reach on port 53 even
// at a private or host address (public addresses are open anyway), and
// which addresses of its network backend lead to the host.

package netpolicy

import (
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mb706/devc/internal/engine"
)

// Host files that name the host's resolvers. systemd-resolved points
// /etc/resolv.conf at a local stub and keeps the real upstream servers in
// its own file, which the engine copies into containers instead (the stub
// is unreachable from a container).
const (
	hostResolvConf     = "etc/resolv.conf"
	resolvedUpstream   = "run/systemd/resolve/resolv.conf"
	slirpDefaultCIDR   = "10.0.2.0/24"
	slirpResolverIndex = 3
)

// pastaResolver is the address Podman gives pasta's DNS forwarder
// (--dns-forward) and writes into the containers' resolv.conf.
var pastaResolver = netip.MustParseAddr("169.254.1.1")

// TrustedResolvers computes on the host, from inputs no box can write, the
// resolver addresses that the rules of level internet open on port 53 even
// where they would reject the address (finding B13): the host's own IPv4 nameservers, those of systemd-resolved's
// upstream file when /etc/resolv.conf names a loopback stub, and the
// built-in resolver of the box's network backend (slirp4netns: the third
// address of its network; pasta: Podman's DNS forward address; named
// networks: their gateways, where the engine's DNS server listens).
// hostRoot prefixes host paths ("/" normally). Files that cannot be read
// contribute nothing: a resolver devc cannot vouch for gets no exception.
func TrustedResolvers(hostRoot string, ci *engine.ContainerInfo) []netip.Addr {
	var out []netip.Addr
	add := func(as ...netip.Addr) {
		for _, a := range as {
			a = a.Unmap()
			if a.Is4() && !a.IsLoopback() && !a.IsUnspecified() && !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	host, stub := hostNameservers(filepath.Join(hostRoot, hostResolvConf))
	add(host...)
	if stub || len(host) == 0 {
		upstream, _ := hostNameservers(filepath.Join(hostRoot, resolvedUpstream))
		add(upstream...)
	}
	if ci != nil {
		add(backendResolvers(ci)...)
	}
	return out
}

// hostNameservers returns the nameservers of a resolv.conf on the host and
// whether any of them is a loopback address (a local stub).
func hostNameservers(path string) ([]netip.Addr, bool) {
	b, err := readHostFile(path)
	if err != nil {
		return nil, false
	}
	as, err := ParseResolvConf(b)
	if err != nil {
		return nil, false
	}
	stub := false
	var out []netip.Addr
	for _, a := range as {
		if a.IsLoopback() {
			stub = true
			continue
		}
		out = append(out, a)
	}
	return out, stub
}

// readHostFile reads a small configuration file of the host. Unlike the
// engine's per-container files it may be a symlink (/etc/resolv.conf
// usually is): it lives in directories only root can change.
func readHostFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxEngineFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxEngineFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, MaxEngineFile)
	}
	return b, nil
}

// backendResolvers returns the built-in resolver of a container's network
// backend, from the engine's inspect data.
func backendResolvers(ci *engine.ContainerInfo) []netip.Addr {
	mode, opts, _ := strings.Cut(ci.HostConfig.NetworkMode, ":")
	switch mode {
	case "slirp4netns":
		cidr := slirpDefaultCIDR
		for _, o := range strings.Split(opts, ",") {
			if v, ok := strings.CutPrefix(o, "cidr="); ok {
				cidr = v
			}
		}
		if a, ok := nthAddr(cidr, slirpResolverIndex); ok {
			return []netip.Addr{a}
		}
		return nil
	case "pasta":
		return []netip.Addr{pastaResolver}
	}
	var out []netip.Addr
	for _, n := range ci.NetworkSettings.Networks {
		if a, err := netip.ParseAddr(n.Gateway); err == nil {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(x, y netip.Addr) int { return x.Compare(y) })
	return out
}

// pastaHostAddr is the address Podman maps to the host for pasta
// (--map-guest-addr, the host alias since Podman 5.3).
var pastaHostAddr = netip.MustParseAddr("169.254.1.2")

// slirpGatewayIndex is the address of slirp4netns's gateway in its
// network; with allow_host_loopback it reaches the host's loopback.
const slirpGatewayIndex = 2

// BackendHostAddrs returns the addresses at which a container's network
// backend may expose the host to it, from the engine's inspect data
// (finding D1-21): slirp4netns's gateway (the host's loopback when
// allow_host_loopback is set), pasta's mapped host address, and the
// gateways of named networks. They are rejected like the host's own
// addresses, before the LAN exception set.
func BackendHostAddrs(ci *engine.ContainerInfo) []netip.Addr {
	mode, opts, _ := strings.Cut(ci.HostConfig.NetworkMode, ":")
	switch mode {
	case "slirp4netns":
		cidr := slirpDefaultCIDR
		for _, o := range strings.Split(opts, ",") {
			if v, ok := strings.CutPrefix(o, "cidr="); ok {
				cidr = v
			}
		}
		if a, ok := nthAddr(cidr, slirpGatewayIndex); ok {
			return []netip.Addr{a}
		}
		return nil
	case "pasta":
		return []netip.Addr{pastaHostAddr}
	}
	var out []netip.Addr
	for _, n := range ci.NetworkSettings.Networks {
		if a, err := netip.ParseAddr(n.Gateway); err == nil && !slices.Contains(out, a.Unmap()) {
			out = append(out, a.Unmap())
		}
	}
	slices.SortFunc(out, func(x, y netip.Addr) int { return x.Compare(y) })
	return out
}

// nthAddr returns the n-th address of an IPv4 network.
func nthAddr(cidr string, n int) (netip.Addr, bool) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() {
		return netip.Addr{}, false
	}
	a := p.Masked().Addr()
	for i := 0; i < n; i++ {
		a = a.Next()
	}
	if !p.Contains(a) {
		return netip.Addr{}, false
	}
	return a, true
}
