// This file holds the machine name of the data manifest and the check of
// data remotes.

package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// machineRE is the rule for machine_name: it is written into the data
// manifest, which travels between machines with the context repository.
var machineRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidMachineName reports whether s may be a machine name.
func ValidMachineName(s string) bool { return machineRE.MatchString(s) }

// Machine is this machine's name in the data manifest: machine_name, or
// the host name with characters outside the rule replaced by "-".
func (g *Global) Machine() string {
	if g.MachineName != "" {
		return g.MachineName
	}
	return MachineFromHostname(os.Hostname())
}

// MachineFromHostname turns a host name into a machine name.
func MachineFromHostname(host string, err error) string {
	if err != nil {
		return "localhost"
	}
	var b strings.Builder
	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.TrimLeft(b.String(), "._-")
	if len(s) > 63 {
		s = s[:63]
	}
	if s == "" {
		return "localhost"
	}
	return s
}

// CheckRemote checks a data remote (work order C14): host is an SSH host
// alias, or empty for a directory on this machine (a mounted disk, a NAS);
// path is the remote's data root. rsync gets both on its command line, so
// nothing in them may pass for an option, a second host, a wildcard (rsync
// expands those on the remote), a control character or a parent directory.
// A local path must be absolute; a remote one is absolute or relative to
// the remote account's home directory.
func CheckRemote(r Remote) error {
	if r.Host != "" && !ValidHostAlias(r.Host) {
		return fmt.Errorf("host %q is not a valid SSH host alias (letters, digits, '.', '_' and '-'; empty for a local directory)", r.Host)
	}
	p := r.Path
	switch {
	case p == "":
		return errors.New("path is empty")
	case strings.HasPrefix(p, "-"):
		return fmt.Errorf("path %q starts with '-'", p)
	case strings.HasPrefix(p, "~"):
		return fmt.Errorf("path %q starts with '~': give an absolute path, or one relative to the remote home directory", p)
	case strings.ContainsAny(p, `*?[\`):
		return fmt.Errorf("path %q contains a wildcard character (* ? [ or \\)", p)
	case r.Host == "" && !strings.HasPrefix(p, "/"):
		return fmt.Errorf("path %q of a local remote (no host) must be absolute", p)
	case len(p) > 4096:
		return errors.New("path is too long")
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("path %q contains a control character", p)
		}
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return fmt.Errorf("path %q contains \"..\"", p)
		}
	}
	return nil
}
