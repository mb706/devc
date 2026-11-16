package netpolicy

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
)

// Facts are what the host learns about a box's network between init and
// start.
type Facts struct {
	// Namespace is the path of the box's network namespace.
	Namespace string
	// Resolvers are the IPv4 nameservers of the box's resolv.conf that the
	// host trusts (finding B13): only they get DNS exceptions.
	Resolvers []netip.Addr
	// Untrusted are the box's other nameservers; they get no exception.
	Untrusted []netip.Addr
	// HostAlias are the addresses that reach the host other than its own:
	// those the box's hosts file gives the engine's host alias and those
	// the network backend maps to the host (BackendHostAddrs). The rules
	// reject them with the host's addresses, before the LAN exception
	// set, and the canary tries them.
	HostAlias []netip.Addr
}

// Installer installs and checks the rules of level internet from the host,
// through the engine's user namespace: `podman unshare nsenter
// --net=<namespace> nft …`. The box's namespace belongs to a user
// namespace below the engine's, so root there may change it while no
// process of the box (which has no capabilities) can.
type Installer struct {
	Engine engine.Engine
	// LookPath finds nsenter and nft (execx.LookPath when nil): nft usually
	// lives in an sbin directory that is not in a user's PATH.
	LookPath func(string) (string, error)
	// HostAddrs lists the host's addresses (HostAddrs when nil).
	HostAddrs func() ([]netip.Addr, error)
	// Proc reads the box's main process from the host's /proc (nil: the
	// real one).
	Proc HostProc
}

func (in *Installer) tools() (nsenter, nft string, err error) {
	look := in.LookPath
	if look == nil {
		look = execx.LookPath
	}
	if nsenter, err = look("nsenter"); err != nil {
		return "", "", fmt.Errorf("nsenter is needed for network level internet (package util-linux): %w", err)
	}
	if nft, err = look("nft"); err != nil {
		return "", "", fmt.Errorf("nft is needed for network level internet (package nftables): %w", err)
	}
	return nsenter, nft, nil
}

// Gather reads the namespace and the engine files of an initialised
// container. Its resolv.conf and hosts file are read now, before any box
// process can change them. Of the box's nameservers only those in trusted
// (TrustedResolvers: computed on the host from trusted inputs) are kept:
// the file lives beside the container and is not proof of anything.
func Gather(ci *engine.ContainerInfo, trusted []netip.Addr) (Facts, error) {
	f := Facts{Namespace: ci.NetworkSettings.SandboxKey}
	if err := checkNamespace(f.Namespace); err != nil {
		return f, err
	}
	if ci.ResolvConfPath != "" {
		b, err := ReadEngineFile(ci.ResolvConfPath)
		if err != nil {
			return f, fmt.Errorf("reading the box's resolver configuration: %w", err)
		}
		all, err := ParseResolvConf(b)
		if err != nil {
			return f, err
		}
		for _, a := range all {
			if slices.Contains(trusted, a) {
				f.Resolvers = append(f.Resolvers, a)
			} else {
				f.Untrusted = append(f.Untrusted, a)
			}
		}
	}
	if ci.HostsPath != "" {
		b, err := ReadEngineFile(ci.HostsPath)
		if err != nil {
			return f, fmt.Errorf("reading the box's hosts file: %w", err)
		}
		f.HostAlias = ParseHosts(b, HostAliases...)
	}
	for _, a := range BackendHostAddrs(ci) {
		if !slices.Contains(f.HostAlias, a) {
			f.HostAlias = append(f.HostAlias, a)
		}
	}
	return f, nil
}

func checkNamespace(ns string) error {
	if ns == "" {
		return fmt.Errorf("the engine reports no network namespace for the box")
	}
	if !filepath.IsAbs(ns) || filepath.Clean(ns) != ns || strings.ContainsAny(ns, "\x00\n") {
		return fmt.Errorf("the engine reports an unusable network namespace path %q", ns)
	}
	return nil
}

// Install writes the rules into the namespace and verifies them.
func (in *Installer) Install(ctx context.Context, f Facts) error {
	nsenter, nft, err := in.tools()
	if err != nil {
		return err
	}
	if err := checkNamespace(f.Namespace); err != nil {
		return err
	}
	hosts := in.HostAddrs
	if hosts == nil {
		hosts = HostAddrs
	}
	addrs, err := hosts()
	if err != nil {
		return err
	}
	// The host alias and the backend's way to the host are the host too
	// (finding D1-21): a "lan" exception must not open them.
	rules := Rules(f.Resolvers, append(slices.Clip(addrs), f.HostAlias...))
	if res, err := in.Engine.Unshare(ctx, []string{nsenter, "--net=" + f.Namespace, nft, "-f", "-"}, strings.NewReader(rules)); err != nil {
		return fmt.Errorf("installing the network rules: %w%s", err, tail(res))
	}
	return in.Verify(ctx, f.Namespace)
}

// Verify checks that devc's table, set, chain and sentinel are present in
// the namespace.
func (in *Installer) Verify(ctx context.Context, namespace string) error {
	_, err := in.verify(ctx, namespace)
	return err
}

// verify is Verify returning the table's listing.
func (in *Installer) verify(ctx context.Context, namespace string) (string, error) {
	nsenter, nft, err := in.tools()
	if err != nil {
		return "", err
	}
	if err := checkNamespace(namespace); err != nil {
		return "", err
	}
	res, err := in.Engine.Unshare(ctx, []string{nsenter, "--net=" + namespace, nft, "list", "table", "inet", Table}, nil)
	if err != nil {
		return "", fmt.Errorf("the network rules are not in place: %w", err)
	}
	out := string(res.Stdout)
	for _, want := range []string{"set " + AllowSet + " ", "chain " + Chain + " ", SentinelAddr.String()} {
		if !strings.Contains(out, want) {
			return "", fmt.Errorf("the network rules are not in place: %q missing from table inet %s", strings.TrimSpace(want), Table)
		}
	}
	return out, nil
}

// ApplyExceptions makes the exception sets in the namespace of a running
// box of level internet match list at now (ExceptionScript). The caller has
// checked that the namespace is the one devc bound the box's readiness to
// (finding B1); the rules themselves are verified here first. Adding and
// removing set elements changes neither the namespace nor the box's main
// process, so the binding and the readiness checks stay valid.
func (in *Installer) ApplyExceptions(ctx context.Context, namespace string, list []Exception, now time.Time) error {
	out, err := in.verify(ctx, namespace)
	if err != nil {
		return err
	}
	script, err := ExceptionScript(list, now, strings.Contains(out, "set "+LANSet+" "))
	if err != nil {
		return err
	}
	nsenter, nft, err := in.tools()
	if err != nil {
		return err
	}
	if res, err := in.Engine.Unshare(ctx, []string{nsenter, "--net=" + namespace, nft, "-f", "-"}, strings.NewReader(script)); err != nil {
		return fmt.Errorf("applying the network exceptions: %w%s", err, tail(res))
	}
	return nil
}

func tail(res execx.Result) string {
	s := strings.TrimSpace(string(res.Stderr))
	if s == "" {
		return ""
	}
	if len(s) > 1000 {
		s = s[len(s)-1000:]
	}
	return "\n" + s
}
