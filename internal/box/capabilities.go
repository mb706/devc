package box

// The capabilities a box was started with (finding C1-02). Secrets the
// daemon hands out on demand, SSH hosts behind the gate and (work order
// C13) the network level and allowlist the host's proxy serves are granted
// by the configuration, but a running box keeps the isolation it was
// started with: granting it something new while its network policy or definition
// has drifted would hand the capability to a box that does not implement
// the policy the user now expects. So every successful start records what
// the box was started with, next to its other generated state (beside the
// home, out of every box's reach), and the daemon and gate-exec authorise
// against the intersection of that record and the current configuration:
// removing a grant takes effect at once, adding one needs the box to be
// brought up again. A box without a record (started before this record
// existed, or a start that failed) gets nothing on demand.
//
// Network exceptions (netpolicy, `devc net allow`) are different on
// purpose: they are granted while the box runs, by the user on the host or
// through an approved request, and kept in their own record; nothing here
// is changed by them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/github"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/paths"
)

// maxCapabilitiesFile bounds the record when it is read.
const maxCapabilitiesFile = 1 << 20

// Capabilities is the record of one start.
type Capabilities struct {
	V int `json:"v"`
	// ContainerID is the container the record belongs to.
	ContainerID string    `json:"container_id"`
	Recorded    time.Time `json:"recorded"`
	// Secrets are the names the box may receive (GrantedSecrets).
	Secrets []string `json:"secrets"`
	// SSH and SSHAllow are grants.ssh and grants.ssh_allow.
	SSH      []string            `json:"ssh"`
	SSHAllow map[string][]string `json:"ssh_allow,omitempty"`
	// Network is the level the box was started with; Allowlist the
	// allowlist entries in canonical form (level allowlist only).
	Network   string   `json:"network,omitempty"`
	Allowlist []string `json:"allowlist,omitempty"`
	// GitHub is grants.github (work order C15); GitHubPermissions the
	// permissions of mode "app"; GitHubRead grants.github_read.
	GitHub            string            `json:"github,omitempty"`
	GitHubPermissions map[string]string `json:"github_permissions,omitempty"`
	GitHubRead        []string          `json:"github_read,omitempty"`
}

// GrantedSecrets lists the secrets a project may receive: its granted
// secrets and the credentials of its env-token and api-key harnesses.
func GrantedSecrets(r *config.Resolved) []string {
	out := append([]string{}, r.Grants.Secrets...)
	for _, h := range r.Harnesses {
		if (h.AuthMethod == "env-token" || h.AuthMethod == "api-key") && h.Secret != "" && !slices.Contains(out, h.Secret) {
			out = append(out, h.Secret)
		}
	}
	return out
}

// CapabilitiesOf is the record of a start of container with r.
func CapabilitiesOf(r *config.Resolved, containerID string, now time.Time) Capabilities {
	allow := map[string][]string{}
	for h, l := range r.Grants.SSHAllow {
		allow[h] = slices.Clone(l)
	}
	c := Capabilities{V: 1, ContainerID: containerID, Recorded: now.UTC(), Secrets: GrantedSecrets(r),
		SSH: slices.Clone(r.Grants.SSH), SSHAllow: allow, Network: r.Network,
		GitHub: r.Grants.GitHub, GitHubRead: slices.Clone(r.Grants.GitHubRead)}
	if r.Grants.GitHub == config.GitHubModeApp {
		c.GitHubPermissions = r.Grants.GitHubPermissions()
	}
	if r.Network == netpolicy.Allowlist {
		// Validated with the configuration; an entry that does not parse
		// is simply not recorded, so it is never allowed.
		if l, err := netpolicy.Expand(r.NetAllowlist.Domains, r.NetAllowlist.Presets); err == nil {
			c.Allowlist = l.Strings()
		}
	}
	return c
}

// WriteCapabilities stores the record of ref's box (0600, replaced whole).
func WriteCapabilities(p *paths.Paths, ref names.ProjectRef, c Capabilities) error {
	file := p.BoxCapabilitiesFile(ref)
	if err := p.Ensure(filepath.Dir(file)); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".tmp-capabilities-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// RemoveCapabilities removes the record (the box stopped, or a start
// begins).
func RemoveCapabilities(p *paths.Paths, ref names.ProjectRef) error {
	if err := os.Remove(p.BoxCapabilitiesFile(ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ErrNoCapabilities: the box has no record of its start.
var ErrNoCapabilities = errors.New("the box has no record of what it was started with")

// ReadCapabilities reads the record of ref's box; ErrNoCapabilities when
// there is none.
func ReadCapabilities(p *paths.Paths, ref names.ProjectRef) (*Capabilities, error) {
	f, err := os.OpenFile(p.BoxCapabilitiesFile(ref), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoCapabilities
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCapabilitiesFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCapabilitiesFile {
		return nil, fmt.Errorf("%s is too large", p.BoxCapabilitiesFile(ref))
	}
	var c Capabilities
	if err := json.Unmarshal(b, &c); err != nil || c.V != 1 {
		return nil, fmt.Errorf("%s is corrupt", p.BoxCapabilitiesFile(ref))
	}
	return &c, nil
}

// AllowsSecret reports whether the box may receive the secret: it was
// started with it and the configuration r still grants it.
func (c *Capabilities) AllowsSecret(r *config.Resolved, name string) bool {
	return slices.Contains(c.Secrets, name) && slices.Contains(GrantedSecrets(r), name)
}

// ErrNotAllowlist: the box was not started at level allowlist, or the
// configuration no longer says allowlist.
var ErrNotAllowlist = errors.New("this box does not use the allowlist proxy")

// ProxyAllowlist is the allowlist the proxy serves for the box: the box was
// started at level allowlist and the configuration r still says so, and
// only entries that were in force at the start and still are count.
func (c *Capabilities) ProxyAllowlist(r *config.Resolved) (netpolicy.EntryList, error) {
	if c.Network != netpolicy.Allowlist || r.Network != netpolicy.Allowlist {
		return nil, ErrNotAllowlist
	}
	cur, err := netpolicy.Expand(r.NetAllowlist.Domains, r.NetAllowlist.Presets)
	if err != nil {
		return nil, err
	}
	return cur.Intersect(c.Allowlist), nil
}

// SSHGrant is the grant of host in force for the box: granted at its start
// and still now, read-only when either says so, and the commands a
// read-only host allows both then and now.
func (c *Capabilities) SSHGrant(r *config.Resolved, host string) (config.SSHGrant, []string, bool) {
	cur, ok := r.Grants.SSHGrantFor(host)
	if !ok {
		return config.SSHGrant{}, nil, false
	}
	started, ok := config.Grants{SSH: c.SSH}.SSHGrantFor(host)
	if !ok {
		return config.SSHGrant{}, nil, false
	}
	g := config.SSHGrant{Host: host, ReadOnly: cur.ReadOnly || started.ReadOnly}
	if !g.ReadOnly {
		return g, nil, true
	}
	var allow []string
	switch {
	case cur.ReadOnly && started.ReadOnly:
		for _, cmd := range r.Grants.SSHAllowed(host) {
			if slices.Contains(c.SSHAllow[host], cmd) {
				allow = append(allow, cmd)
			}
		}
	case started.ReadOnly:
		allow = slices.Clone(c.SSHAllow[host])
	default:
		allow = r.Grants.SSHAllowed(host)
	}
	return g, allow, true
}

// ErrNoGitHubToken: the box holds no GitHub token, now or since its start.
var ErrNoGitHubToken = errors.New("no GitHub token for this box")

// GitHubToken is the token mode in force for the box (work order C15):
// grants.github was "app" or "token:<secret>" at its start and is the same
// now. For "app" the permissions are those granted then and now, each at
// the lower level.
func (c *Capabilities) GitHubToken(r *config.Resolved) (string, map[string]string, error) {
	switch {
	case !r.Grants.GitHubTokenMode():
		return "", nil, fmt.Errorf("%w: grants.github is %q", ErrNoGitHubToken, r.Grants.GitHub)
	case c.GitHub != r.Grants.GitHub:
		return "", nil, fmt.Errorf("%w: grants.github changed to %q after the box started with %q", ErrNoGitHubToken, r.Grants.GitHub, c.GitHub)
	}
	if r.Grants.GitHub != config.GitHubModeApp {
		return r.Grants.GitHub, nil, nil
	}
	perms := github.IntersectPermissions(c.GitHubPermissions, r.Grants.GitHubPermissions())
	if len(perms) == 0 {
		return "", nil, fmt.Errorf("%w: the permissions granted at the start and now have nothing in common", ErrNoGitHubToken)
	}
	return r.Grants.GitHub, perms, nil
}

// GitHubReadRepos lists the further repositories the read facade serves the box:
// listed in grants.github_read at its start and now.
func (c *Capabilities) GitHubReadRepos(r *config.Resolved) []string {
	var out []string
	for _, s := range r.Grants.GitHubRead {
		if slices.ContainsFunc(c.GitHubRead, func(o string) bool { return strings.EqualFold(o, s) }) {
			out = append(out, s)
		}
	}
	return out
}
