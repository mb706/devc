// Package hub is the hub server (work order C11a): one always-on process on
// a server that machine daemons dial out to. It keeps a projection of every
// session on every machine in SQLite (views, a bounded tail of events per
// session, cursors, pending requests), derives the inbox, and offers a JSON
// API with server-sent events for a user interface. Its CA issues the
// client certificates that are the front door for machines and devices.
// The hub holds nothing that cannot be rebuilt from the machines, except
// its registry of certificates.
//
// Security rules: the hub is public-facing. Nothing speaks HTTP to it
// without a client certificate from its CA, checked against the registry
// (known, not expired, not revoked) on every request and, for long-lived
// connections, whenever the registry changes; the role and name come from
// the registry, never from the request. The reverse-proxy header is
// honoured only in behind-proxy mode, from loopback peers, on a loopback
// listener. Every request has a size limit, the Host header must name the
// hub, state-changing requests must be JSON from the hub's own origin, and
// no CORS headers are sent. Everything a machine sends is decoded strictly
// and validated before it is stored (events through proto.ValidateData);
// a machine can only write rows under its own registry name. Over the
// machine link the hub only calls methods; which ones a hub may call, and
// with what effect, is decided by the daemon (internal/daemon/hublink.go).
// The CA key never leaves the hub directory (0600 in 0700); the server
// itself reads only the CA certificate.
package hub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// EnvHubDir overrides the hub directory.
const EnvHubDir = "DEVC_HUB_DIR"

// ResolveDir returns the hub directory: $DEVC_HUB_DIR, else
// ~/.local/share/devc-hub. It is never mixed with a machine's state.
func ResolveDir(getenv func(string) string, home string) (string, error) {
	if d := getenv(EnvHubDir); d != "" {
		if !filepath.IsAbs(d) {
			abs, err := filepath.Abs(d)
			if err != nil {
				return "", err
			}
			d = abs
		}
		return filepath.Clean(d), nil
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("cannot determine the home directory: set HOME or " + EnvHubDir)
	}
	return filepath.Join(home, ".local", "share", "devc-hub"), nil
}

// Layout names the files of a hub directory.
type Layout struct{ Dir string }

func (l Layout) CACert() string     { return filepath.Join(l.Dir, "ca", "ca.crt") }
func (l Layout) CAKey() string      { return filepath.Join(l.Dir, "ca", "ca.key") }
func (l Layout) ServerCert() string { return filepath.Join(l.Dir, "server", "server.crt") }
func (l Layout) ServerKey() string  { return filepath.Join(l.Dir, "server", "server.key") }
func (l Layout) ServerCSR() string  { return filepath.Join(l.Dir, "server", "server.csr") }
func (l Layout) DB() string         { return filepath.Join(l.Dir, "hub.db") }

var hostRE = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// ValidHost reports whether s is a DNS name (or an IP address when ip is
// allowed by the caller's own check).
func ValidHost(s string) bool { return len(s) <= 253 && hostRE.MatchString(s) }

// Init creates the hub directory (0700), the CA, the database and, when
// publicName is given, the server key, a certificate request and a server
// certificate for it. It refuses a directory that already has a CA.
func Init(dir, publicName string) error {
	l := Layout{Dir: dir}
	if _, err := os.Stat(l.CACert()); err == nil {
		return fmt.Errorf("%s already holds a hub CA", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if publicName != "" {
		if _, _, err := splitPublicName(publicName); err != nil {
			return err
		}
	}
	if err := createCA(l); err != nil {
		return err
	}
	st, err := OpenStore(l.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if publicName != "" {
		if err := st.SetMeta(metaPublicName, publicName); err != nil {
			return err
		}
		ca, err := LoadCA(l)
		if err != nil {
			return err
		}
		host, _, _ := splitPublicName(publicName)
		if err := ca.IssueServer(l, host); err != nil {
			return err
		}
	}
	return nil
}
