// Package hubnode keeps this machine's side of joining hubs (work order
// C11a section 3): the client certificate, key and optional CA for each hub
// under durable/node/hubs/<id>/, and the hub list in config.toml, which
// `devc join` and `devc leave` edit.
//
// Security rules: the hub list is policy and lives only in the config
// directory (README rule 1); credentials live in the state directory,
// which no box mounts. Keys are written 0600 in 0700 directories and never
// printed (rule 5). config.toml is edited by appending or removing one
// [[hubs]] block, and every edit is verified by decoding the result: the
// file must still parse and its hub list must be the old one with exactly
// that URL added or removed, or nothing is written (an inline hubs = [...]
// list, for example, is left for the user to edit).
package hubnode

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/mb706/devc/internal/paths"
)

// File names in a hub's credentials directory.
const (
	infoFile = "hub.json"
	certFile = "cert.pem"
	keyFile  = "key.pem"
	caFile   = "ca.pem"
)

var safeURLRE = regexp.MustCompile(`^[A-Za-z0-9._~:/\[\]@!$&'()*+,;=%-]+$`)

// NormalizeURL checks a hub URL (https, a host, no user information, query
// or fragment) and returns it without a trailing slash.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("%q is not a hub URL: use https://<host>[:port][/path]", raw)
	}
	s := strings.TrimRight(u.String(), "/")
	if !safeURLRE.MatchString(s) || strings.ContainsAny(s, `"\`) {
		return "", fmt.Errorf("%q contains characters a hub URL cannot have", raw)
	}
	return s, nil
}

// ID names a hub's credentials directory: 16 hex characters of the SHA-256
// of its normalised URL.
func ID(hubURL string) string {
	sum := sha256.Sum256([]byte(hubURL))
	return hex.EncodeToString(sum[:8])
}

// MachineURL is the WebSocket URL of a hub's machine endpoint.
func MachineURL(hubURL string) string {
	return "wss://" + strings.TrimPrefix(hubURL, "https://") + "/machine"
}

type info struct {
	URL    string    `json:"url"`
	Joined time.Time `json:"joined"`
}

// Creds are the credentials for one hub.
type Creds struct {
	URL  string
	Cert tls.Certificate
	// Leaf is the client certificate; RootCAs is nil when the hub's server
	// certificate is checked against the system's roots.
	Leaf    *x509.Certificate
	RootCAs *x509.CertPool
}

// TLSConfig is the client TLS configuration for the hub.
func (c *Creds) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{c.Cert}, RootCAs: c.RootCAs, NextProtos: []string{"http/1.1"}}
}

// Check parses the credentials given to `devc join` and returns the client
// certificate.
func Check(certPEM, keyPEM, caPEM []byte) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the certificate and key do not form a pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if time.Now().After(leaf.NotAfter) {
		return nil, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.Format("2006-01-02"))
	}
	if caPEM != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("the CA file holds no PEM certificate")
		}
	}
	return leaf, nil
}

// writeFile writes data atomically with mode.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Save stores the credentials of a hub (replacing earlier ones).
func Save(p *paths.Paths, hubURL string, certPEM, keyPEM, caPEM []byte) error {
	if _, err := Check(certPEM, keyPEM, caPEM); err != nil {
		return err
	}
	dir := p.NodeHubDir(ID(hubURL))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(info{URL: hubURL, Joined: time.Now().UTC()})
	if err := writeFile(filepath.Join(dir, keyFile), keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, certFile), certPEM, 0o600); err != nil {
		return err
	}
	if caPEM != nil {
		if err := writeFile(filepath.Join(dir, caFile), caPEM, 0o600); err != nil {
			return err
		}
	} else if err := os.Remove(filepath.Join(dir, caFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writeFile(filepath.Join(dir, infoFile), b, 0o600)
}

// ErrNoCreds is returned by Load when the hub was never joined.
var ErrNoCreds = errors.New("no credentials for this hub")

// Load reads the credentials of a hub.
func Load(p *paths.Paths, hubURL string) (*Creds, error) {
	dir := p.NodeHubDir(ID(hubURL))
	b, err := os.ReadFile(filepath.Join(dir, infoFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: run 'devc join %s --cert <file> --key <file>'", ErrNoCreds, hubURL)
	}
	if err != nil {
		return nil, err
	}
	var in info
	if err := json.Unmarshal(b, &in); err != nil || in.URL != hubURL {
		return nil, fmt.Errorf("the credentials in %s are for another hub", dir)
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, certFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the stored certificate for %s is unusable: %w", hubURL, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	c := &Creds{URL: hubURL, Cert: pair, Leaf: leaf}
	if caPEM, err := os.ReadFile(filepath.Join(dir, caFile)); err == nil {
		c.RootCAs = x509.NewCertPool()
		if !c.RootCAs.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("the stored CA for %s holds no certificate", hubURL)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return c, nil
}

// Remove deletes the credentials of a hub; it reports whether there were
// any.
func Remove(p *paths.Paths, hubURL string) (bool, error) {
	dir := p.NodeHubDir(ID(hubURL))
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return true, os.RemoveAll(dir)
}

// Stamp fingerprints a hub's credential files (sizes and modification
// times), so a daemon notices a new join.
func Stamp(p *paths.Paths, hubURL string) string {
	dir := p.NodeHubDir(ID(hubURL))
	var b strings.Builder
	for _, f := range []string{infoFile, certFile, keyFile, caFile} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err == nil {
			fmt.Fprintf(&b, "%s %d %d\n", f, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

// --- config.toml ---

type hubList struct {
	Hubs []struct {
		URL string `toml:"url"`
	} `toml:"hubs"`
}

func listURLs(data []byte) ([]string, error) {
	var l hubList
	if _, err := toml.Decode(string(data), &l); err != nil {
		return nil, err
	}
	var out []string
	for _, h := range l.Hubs {
		u, err := NormalizeURL(h.URL)
		if err != nil {
			u = h.URL
		}
		out = append(out, u)
	}
	return out, nil
}

func readConfig(file string) ([]byte, os.FileMode, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0o644, nil
	}
	if err != nil {
		return nil, 0, err
	}
	fi, err := os.Stat(file)
	if err != nil {
		return nil, 0, err
	}
	return b, fi.Mode().Perm(), nil
}

// AddToConfig appends a [[hubs]] block for hubURL to config.toml unless the
// URL is listed already; it reports whether it added one.
func AddToConfig(file, hubURL string) (bool, error) {
	data, mode, err := readConfig(file)
	if err != nil {
		return false, err
	}
	before, err := listURLs(data)
	if err != nil {
		return false, fmt.Errorf("%s does not parse: %w (fix it first)", file, err)
	}
	if slices.Contains(before, hubURL) {
		return false, nil
	}
	out := append([]byte{}, data...)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	if len(out) > 0 {
		out = append(out, '\n')
	}
	out = append(out, []byte("[[hubs]]\nurl = \""+hubURL+"\"\n")...)
	after, err := listURLs(out)
	if err != nil || !slices.Equal(after, append(before, hubURL)) {
		return false, fmt.Errorf("could not add the hub to %s automatically; add this block yourself:\n[[hubs]]\nurl = %q", file, hubURL)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return false, err
	}
	return true, writeFile(file, out, mode)
}

var (
	hubsHeaderRE = regexp.MustCompile(`^\s*\[\[\s*hubs\s*\]\]\s*(#.*)?$`)
	tableRE      = regexp.MustCompile(`^\s*\[`)
)

// RemoveFromConfig removes the [[hubs]] block of hubURL from config.toml;
// it reports whether there was one.
func RemoveFromConfig(file, hubURL string) (bool, error) {
	data, mode, err := readConfig(file)
	if err != nil || data == nil {
		return false, err
	}
	before, err := listURLs(data)
	if err != nil {
		return false, fmt.Errorf("%s does not parse: %w (fix it first)", file, err)
	}
	if !slices.Contains(before, hubURL) {
		return false, nil
	}
	lines := strings.SplitAfter(string(data), "\n")
	var out []string
	for i := 0; i < len(lines); {
		if !hubsHeaderRE.MatchString(strings.TrimRight(lines[i], "\r\n")) {
			out = append(out, lines[i])
			i++
			continue
		}
		end := i + 1
		for end < len(lines) && !tableRE.MatchString(lines[end]) {
			end++
		}
		// Blank lines before the next table stay.
		for end > i+1 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		block := strings.Join(lines[i:end], "")
		if urls, err := listURLs([]byte(block)); err == nil && len(urls) == 1 && urls[0] == hubURL {
			// Drop the block and the blank line before it.
			if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == "" {
				out = out[:n-1]
			}
			i = end
			continue
		}
		out = append(out, lines[i:end]...)
		i = end
	}
	result := []byte(strings.Join(out, ""))
	want := slices.DeleteFunc(slices.Clone(before), func(u string) bool { return u == hubURL })
	after, err := listURLs(result)
	if err != nil || !slices.Equal(after, want) {
		return false, fmt.Errorf("could not remove the hub from %s automatically; remove its [[hubs]] entry yourself", file)
	}
	return true, writeFile(file, result, mode)
}
