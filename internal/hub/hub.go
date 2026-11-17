// Package hub is the hub server (work orders C11a and C11b): one always-on
// process on a server that machine daemons dial out to. It keeps a
// projection of every session on every machine in SQLite (views, a bounded
// tail of events per session, cursors, pending requests), derives the
// inbox, offers a JSON API with server-sent events and a terminal relay
// for the web app, and sends notifications through ntfy. Its CA issues the
// client certificates that are the front door for machines and devices, by
// hand or through single-use enrolment tokens. It also keeps the archive
// of the machines' durable state and their encrypted secrets bundles (work
// order C17, archive.go). Its view of the sessions can be rebuilt from the
// machines; its registry of certificates, the user's password and the
// archive cannot.
//
// Security rules: the hub is public-facing and can type into agent
// sessions. Nothing reaches a route without a client certificate from its
// CA, checked against the registry (known, not expired, not revoked) on
// every request; a long-lived connection ends when a change of the
// registry leaves its certificate invalid, and when the certificate
// expires. The role and name come from the registry, never from the
// request. The one exception is the enrolment path, while a single-use
// enrolment token is outstanding; one gate (Server.gate, front.go) decides
// that. The API also needs a login session bound to the request's
// certificate (C11b): argon2id password, optional TOTP, 256-bit tokens
// stored hashed, constant-time comparisons, lockout and rate limits. The
// reverse-proxy header is honoured only in behind-proxy mode, from
// loopback peers, on a loopback listener. Every request has a size limit,
// the Host header must name the hub, every POST and PUT must carry
// X-Devc-Request: 1 (and those of the web app an Origin equal to the
// hub's), and no CORS headers are sent. Secrets never reach the log.
// Everything a machine sends is decoded strictly and validated before it
// is stored (events through proto.ValidateData); a machine can only write
// rows under its own registry name, and only append to the archive below
// its own registry name (a ratchet with budgets: archive.go). Over the
// machine link the hub only calls methods; which ones a hub may call, and
// with what effect, is decided by the daemon (internal/daemon/hublink.go).
// The CA key never leaves the hub directory (0600 in 0700); the server
// reads it only to sign a machine's certificate request at enrolment.
//
// Files:
//   - hub.go: the hub directory and its creation; ca.go: the CA and the
//     certificates it issues.
//   - server.go: start, the listeners of both modes, who a request comes
//     from, long-lived connections, housekeeping; front.go: the route
//     table, the gate, access classes, cross-site rules, rate limits and
//     the access log.
//   - auth.go, login.go: passwords, TOTP, tokens, login sessions;
//     enrol.go: enrolment; doctor.go: devc hub doctor; config.go: hub.toml.
//   - store.go, store_access.go: the database.
//   - link.go, validate.go: the machine link and the checks of what a
//     machine sends.
//   - api.go, stream.go, terminal.go: the API, the event stream and the
//     terminal relay; inbox.go, notify.go: the inbox and notifications.
//   - archive.go, retention.go: the archive; web.go, static.go: the web
//     app's API and files; devmode.go, devmachine.go, devterm.go: the
//     development mode.
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
func Init(dir, publicName string) error { return InitFrom(dir, publicName, "") }

// InitFrom is Init with the CA of another hub directory (caFrom, a second
// hub of the same fleet: work order C17) instead of a new one.
func InitFrom(dir, publicName, caFrom string) error {
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
	if caFrom != "" {
		src := Layout{Dir: caFrom}
		if _, err := LoadCA(src); err != nil {
			return fmt.Errorf("reading the CA of %s: %w", caFrom, err)
		}
		if err := os.MkdirAll(filepath.Dir(l.CAKey()), 0o700); err != nil {
			return err
		}
		for _, f := range []struct {
			from, to string
			mode     os.FileMode
		}{{src.CAKey(), l.CAKey(), 0o600}, {src.CACert(), l.CACert(), 0o644}} {
			b, err := os.ReadFile(f.from)
			if err != nil {
				return err
			}
			if err := writeNew(f.to, b, f.mode); err != nil {
				return err
			}
		}
	} else if err := createCA(l); err != nil {
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
