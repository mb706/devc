package config

import (
	"regexp"
	"slices"
	"strings"
)

// SSHReadOnlySuffix marks a grants.ssh entry as the read-only profile
// (work order C10b): "<host>:ro".
const SSHReadOnlySuffix = ":ro"

// GateKeySecret is the name under which the SSH gate's per-box key is
// delivered into /run/devc-secrets; no granted secret may use it.
const GateKeySecret = "gate_key"

// allowWordRE is one command name of [grants.ssh_allow]: a single word
// without whitespace, quotes or shell metacharacters (the read-only
// filter compares the first word of a command with it literally).
var allowWordRE = regexp.MustCompile(`^[A-Za-z0-9._/+][A-Za-z0-9._/+@=:,-]{0,127}$`)

// ValidHostAlias reports whether s may name an SSH host: letters, digits,
// '.', '_' and '-', not starting with '-' (or any other punctuation), so
// it can never be taken for an option by ssh.
func ValidHostAlias(s string) bool { return hostAliasRE.MatchString(s) }

// SSHGrant is one entry of grants.ssh.
type SSHGrant struct {
	Host string `json:"host"`
	// ReadOnly is set for "<host>:ro": only the commands listed in
	// [grants.ssh_allow] for the host may run, without shell syntax.
	ReadOnly bool `json:"read_only,omitempty"`
}

// ParseSSHGrant splits a grants.ssh entry into host and profile. It does
// not validate the host.
func ParseSSHGrant(s string) SSHGrant {
	host, ro := strings.CutSuffix(s, SSHReadOnlySuffix)
	return SSHGrant{Host: host, ReadOnly: ro}
}

// SSHGrants returns the parsed grants.ssh entries in order.
func (g Grants) SSHGrants() []SSHGrant {
	out := make([]SSHGrant, 0, len(g.SSH))
	for _, s := range g.SSH {
		out = append(out, ParseSSHGrant(s))
	}
	return out
}

// SSHGrantFor returns the grant of host, if the project has one.
func (g Grants) SSHGrantFor(host string) (SSHGrant, bool) {
	for _, s := range g.SSH {
		if gr := ParseSSHGrant(s); gr.Host == host {
			return gr, true
		}
	}
	return SSHGrant{}, false
}

// SSHAllowed returns the command names allowed on a read-only host.
func (g Grants) SSHAllowed(host string) []string {
	return slices.Clone(g.SSHAllow[host])
}

// validateGrants checks [grants] of a project.
func validateGrants(k checker, g Grants) {
	seen := map[string]bool{}
	ro := map[string]bool{}
	for i, s := range g.SSH {
		gr := ParseSSHGrant(s)
		switch {
		case !ValidHostAlias(gr.Host):
			k.bad(idx("grants.ssh", i), "%q is not a valid SSH host alias (letters, digits, '.', '_' and '-', optionally followed by %q)", s, SSHReadOnlySuffix)
		case seen[gr.Host]:
			k.bad(idx("grants.ssh", i), "%s is granted more than once", gr.Host)
		}
		seen[gr.Host] = true
		ro[gr.Host] = gr.ReadOnly
	}
	for _, host := range sortedKeys(g.SSHAllow) {
		key := "grants.ssh_allow." + host
		if !ValidHostAlias(host) {
			k.bad(key, "%q is not a valid SSH host alias", host)
			continue
		}
		if !ro[host] {
			k.bad(key, "%s is not granted read-only: add %q to grants.ssh, or remove this list", host, host+SSHReadOnlySuffix)
		}
		words := g.SSHAllow[host]
		for i, w := range words {
			if !allowWordRE.MatchString(w) {
				k.bad(idx(key, i), "%q is not a command name (one word without spaces, quotes or shell metacharacters)", w)
			}
		}
		k.noDuplicates(key, words)
	}
	for i, s := range g.Secrets {
		if s == GateKeySecret {
			k.bad(idx("grants.secrets", i), "the secret name %s is reserved for the SSH gate's key; rename the secret", GateKeySecret)
		}
	}
}
