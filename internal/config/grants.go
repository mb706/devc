// This file holds the helpers and the validation of a project's [grants]
// (SSH hosts and their read-only profile, GitHub modes, secrets) and of a
// realm's [github] table.

package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mb706/devc/internal/github"
	"github.com/mb706/devc/internal/names"
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

// GitHubTokenMode reports whether grants.github hands the box a token
// ("app" or "token:<secret>").
func (g Grants) GitHubTokenMode() bool {
	return g.GitHub == GitHubModeApp || strings.HasPrefix(g.GitHub, GitHubTokenPrefix)
}

// GitHubTokenSecret is the secret of mode "token:<secret>", else "".
func (g Grants) GitHubTokenSecret() string {
	s, _ := strings.CutPrefix(g.GitHub, GitHubTokenPrefix)
	if s == g.GitHub {
		return ""
	}
	return s
}

// GitHubPermissions are the permissions of mode "app": [grants.github_token]
// or, without that table, contents and metadata read.
func (g Grants) GitHubPermissions() map[string]string {
	if g.GitHubToken == nil || g.GitHubToken.Permissions == nil {
		return github.DefaultPermissions()
	}
	out := make(map[string]string, len(g.GitHubToken.Permissions))
	for k, v := range g.GitHubToken.Permissions {
		out[k] = v
	}
	return out
}

// validateGitHub checks the GitHub keys of [grants]; app is the realm's
// [github.app].
func validateGitHub(k checker, g Grants, app *GitHubApp) {
	switch gh := g.GitHub; {
	case gh == GitHubModeRelay, gh == GitHubModeNone:
	case gh == GitHubModeApp:
		if app == nil {
			k.bad("grants.github", "\"app\" needs [github.app] with app_id and key_secret in the realm's realm.toml")
		}
	case strings.HasPrefix(gh, GitHubTokenPrefix):
		k.name("grants.github", names.KindSecret, strings.TrimPrefix(gh, GitHubTokenPrefix))
	default:
		k.bad("grants.github", "%q must be \"relay\", \"none\", \"app\" or \"token:<secret-name>\"", gh)
	}
	if g.GitHubToken != nil {
		if g.GitHub != GitHubModeApp {
			k.bad("grants.github_token", "applies only to grants.github = \"app\"")
		}
		if len(g.GitHubToken.Permissions) == 0 {
			k.bad("grants.github_token.permissions", "name at least one permission, such as contents = \"read\"")
		}
		for _, name := range sortedKeys(g.GitHubToken.Permissions) {
			if err := github.CheckPermission(name, g.GitHubToken.Permissions[name]); err != nil {
				k.bad("grants.github_token.permissions."+name, "%v", err)
			}
		}
	}
	for i, s := range g.GitHubRead {
		if _, err := github.ParseRepoName(s); err != nil {
			k.bad(idx("grants.github_read", i), "%v", err)
		}
	}
	k.noDuplicates("grants.github_read", g.GitHubRead)
}

// A reservedSecret is a realm secret that no box may receive.
type reservedSecret struct {
	holds string // what it holds, for messages
	noun  string // the word for it in "the … would enter the box"
	// service is the service whose credential or cookie jar it is; empty
	// for the GitHub credential and the App's key.
	service string
	jar     bool
}

// reservedSecret says whether the secret name of realm is reserved, and
// what it holds: the realm's GitHub credential (github.HostTokenSecret),
// the private key of its App (github.app.key_secret), a credential of one
// of its services (value_secret, a cookie login's secrets) or a cookie jar
// (any name starting with _jar.). With the GitHub credential or a
// service's credential a box would get past the read facade's or the
// service's rules; with the App's key it could mint its own tokens, past
// every check of github.Minter.
func (c *Config) reservedSecret(realm, name string) (reservedSecret, bool) {
	if name == github.HostTokenSecret {
		return reservedSecret{holds: "the realm's GitHub credential", noun: "credential"}, true
	}
	if r, ok := c.realms[realm]; ok && r.GitHub.App != nil && r.GitHub.App.KeySecret == name {
		return reservedSecret{holds: "the private key of the realm's GitHub App (github.app.key_secret)", noun: "key"}, true
	}
	if svc, ok := strings.CutPrefix(name, JarSecretPrefix); ok {
		return reservedSecret{holds: fmt.Sprintf("the cookie jar of service %q", svc), noun: "cookie jar", service: svc, jar: true}, true
	}
	for _, s := range c.Services(realm) {
		if slices.Contains(s.CredentialSecrets(), name) {
			return reservedSecret{holds: fmt.Sprintf("the credential of service %q", s.Name), noun: "credential", service: s.Name}, true
		}
	}
	return reservedSecret{}, false
}

// checkBoxSecret is the one guard for every way a project's configuration
// names a realm secret for its box: an entry of grants.secrets, a
// harness's credential and grants.github = "token:<secret>" (how says
// which, for the message). A reserved secret (reservedSecret) is a problem
// of the project, so nothing of it is served (findings X10 1, G4-02,
// G4-03).
func (c *Config) checkBoxSecret(k checker, key, realm, name, how string) {
	if r, ok := c.reservedSecret(realm, name); ok {
		k.bad(key, "the secret %s holds %s, and the project also receives it (%s): the %s would enter the box", name, r.holds, how, r.noun)
	}
}

// validateRealmGitHub checks [github] of a realm.
func validateRealmGitHub(k checker, g RealmGitHub) {
	a := g.App
	if a == nil {
		return
	}
	if a.AppID <= 0 {
		k.bad("github.app.app_id", "set the App's numeric ID (shown on the App's settings page)")
	}
	if a.KeySecret == "" {
		k.bad("github.app.key_secret", "name the realm secret that holds the App's private key ('devc secret set <realm> <name> < key.pem')")
	} else {
		k.name("github.app.key_secret", names.KindSecret, a.KeySecret)
	}
	if a.InstallationID < 0 {
		k.bad("github.app.installation_id", "must be a positive number, or left out to look the installation up")
	}
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
