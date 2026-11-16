// Package fleet is the machine side of the archive of durable state (work
// order C17): the client of a hub's archive and secrets endpoints, the
// upload pass the daemon runs for every hub with its bookkeeping, the
// workers that read box-writable harness state, restoring a realm from a
// hub or an export, the export format, and carrying encrypted secrets
// between machines.
//
// Security rules: shared harness state is box-writable (README rule 2).
// The host never walks it: the helper container's archive worker lists or
// packs it, and the host reads each listed file only through safefs
// (beneath the realm's durable directory, no symlinks, regular files,
// size-capped), checking every name again. Credential files the manifests
// list never enter the archive or an export, and are never restored
// (archive.Excluded, applied to every path in and out). Relay bundles are
// made by host git on the relay; the context repository is bundled and
// restored inside its container (ctxrepo). Everything that comes from a
// hub or an export is hostile: paths are validated before they name
// anything, files are created beneath directory descriptors without
// following symlinks and never replace existing ones, and bundles are
// fetched with fsck into scratch refs before refs are created from a
// validated list. Secrets are encrypted on the machine with a key derived
// from a passphrase that is read from a hidden prompt or stdin, never from
// an argument; the hub sees only ciphertext.
package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/hubnode"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// Client talks to one hub's archive and secrets endpoints with this
// machine's certificate. It uses its own HTTP connections, never the
// machine link, so uploads cannot hold up events or commands.
type Client struct {
	URL string
	// Machine is the name the hub knows this machine by (the certificate's
	// common name).
	Machine string
	HTTP    *http.Client
}

// NewClient returns a client for the hub of creds.
func NewClient(creds *hubnode.Creds) (*Client, error) {
	machine := creds.Leaf.Subject.CommonName
	if !names.Valid(machine) {
		return nil, fmt.Errorf("the certificate for %s names the machine %q, which is not a valid name", creds.URL, proto.Printable(proto.Truncate(machine, 64)))
	}
	tr := &http.Transport{
		TLSClientConfig:     creds.TLSConfig(),
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 30 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 2,
		ForceAttemptHTTP2:   false,
	}
	return &Client{URL: creds.URL, Machine: machine, HTTP: &http.Client{Transport: tr, Timeout: 10 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Close releases idle connections.
func (c *Client) Close() { c.HTTP.CloseIdleConnections() }

// HubError is a refusal by the hub.
type HubError struct {
	Status int
	Reply  archive.ErrorReply
}

func (e *HubError) Error() string {
	msg := proto.Printable(proto.Truncate(e.Reply.Error, 500))
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("the hub answered %d: %s", e.Status, msg)
}

// Conflict reports a 409: the stored length (when given) and whether the
// bundle is complete.
func (e *HubError) Conflict() (length int64, known, complete bool) {
	if e.Status != http.StatusConflict {
		return 0, false, false
	}
	if e.Reply.Length != nil {
		return *e.Reply.Length, true, e.Reply.Complete
	}
	return 0, false, e.Reply.Complete
}

// Quota reports a reached archive quota (507).
func (e *HubError) Quota() bool { return e.Status == http.StatusInsufficientStorage }

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

const maxReply = 64 << 20

func (c *Client) do(req *http.Request, v any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return err
	}
	if len(body) > maxReply {
		return errors.New("the hub's answer is too large")
	}
	if resp.StatusCode != http.StatusOK {
		he := &HubError{Status: resp.StatusCode}
		_ = json.Unmarshal(body, &he.Reply)
		return he
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("the hub's answer is not valid: %w", err)
	}
	return nil
}

// Put appends data at offset to the archive path p of realm; final makes a
// bundle complete. An offset that is not the stored length gives a
// *HubError whose Conflict reports the length.
func (c *Client) Put(ctx context.Context, realm, p string, offset int64, data []byte, final bool) (archive.PutReply, error) {
	q := "?offset=" + strconv.FormatInt(offset, 10)
	if final {
		q += "&final=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.URL+archive.UploadPrefix+realm+"/"+escapePath(p)+q, bytes.NewReader(data))
	if err != nil {
		return archive.PutReply{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(archive.RequestHeader, "1")
	var r archive.PutReply
	err = c.do(req, &r)
	return r, err
}

// List returns every machine's archives on the hub.
func (c *Client) List(ctx context.Context) ([]archive.Summary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+archive.IndexPath, nil)
	if err != nil {
		return nil, err
	}
	var r archive.ListReply
	if err := c.do(req, &r); err != nil {
		return nil, err
	}
	for _, s := range r.Archives {
		if !names.Valid(s.Machine) || !names.ValidRealm(s.Realm) {
			return nil, errors.New("the hub listed an invalid archive name")
		}
	}
	return r.Archives, nil
}

// Index returns one machine's archive of a realm; entries with invalid
// paths are refused.
func (c *Client) Index(ctx context.Context, machine, realm string) (*archive.Index, error) {
	if !names.Valid(machine) || !names.ValidRealm(realm) {
		return nil, errors.New("invalid machine or realm name")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+archive.IndexPath+"/"+machine+"/"+realm, nil)
	if err != nil {
		return nil, err
	}
	var idx archive.Index
	if err := c.do(req, &idx); err != nil {
		return nil, err
	}
	for _, e := range idx.Entries {
		if _, err := archive.ParsePath(e.Path); err != nil || e.Length < 0 {
			return nil, fmt.Errorf("the hub's index holds an invalid entry: %w", err)
		}
	}
	return &idx, nil
}

// Fetch copies a stored file (from offset on) to w, at most max bytes; it
// returns the bytes copied.
func (c *Client) Fetch(ctx context.Context, machine, realm, p string, offset, max int64, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+archive.FilePrefix+machine+"/"+realm+"/"+escapePath(p), nil)
	if err != nil {
		return 0, err
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && !(offset > 0 && resp.StatusCode == http.StatusPartialContent) {
		he := &HubError{Status: resp.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = json.Unmarshal(b, &he.Reply)
		return 0, he
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, fmt.Errorf("%s is larger than %d bytes", p, max)
	}
	return n, nil
}

// FetchPrefix copies exactly the first length bytes of a stored file to w.
func (c *Client) FetchPrefix(ctx context.Context, machine, realm, p string, length int64, w io.Writer) error {
	if length == 0 {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+archive.FilePrefix+machine+"/"+realm+"/"+escapePath(p), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-"+strconv.FormatInt(length-1, 10))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		he := &HubError{Status: resp.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = json.Unmarshal(b, &he.Reply)
		return he
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, length))
	if err == nil && n < length {
		err = fmt.Errorf("the hub sent %d of %d bytes of %s", n, length, p)
	}
	return err
}

// PrefixHash asks for the SHA-256 of a stored file's first length bytes.
func (c *Client) PrefixHash(ctx context.Context, realm, p string, length int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+archive.FilePrefix+c.Machine+"/"+realm+"/"+escapePath(p)+"?sha256="+strconv.FormatInt(length, 10), nil)
	if err != nil {
		return "", err
	}
	var r archive.HashReply
	if err := c.do(req, &r); err != nil {
		return "", err
	}
	if r.Length != length || len(r.SHA256) != 64 {
		return "", errors.New("the hub's hash answer does not fit the question")
	}
	return r.SHA256, nil
}

// PutSecrets stores an encrypted secrets bundle of realm on the hub and
// returns its version.
func (c *Client) PutSecrets(ctx context.Context, realm string, s *archive.Sealed) (int, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.URL+archive.SecretsPrefix+realm, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(archive.RequestHeader, "1")
	var r struct {
		Version int `json:"version"`
	}
	err = c.do(req, &r)
	return r.Version, err
}

// GetSecrets fetches the newest encrypted secrets bundle of realm (of one
// machine when machine is set).
func (c *Client) GetSecrets(ctx context.Context, realm, machine string) (*archive.SecretsReply, error) {
	u := c.URL + archive.SecretsPrefix + realm
	if machine != "" {
		u += "?machine=" + url.QueryEscape(machine)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var r archive.SecretsReply
	if err := c.do(req, &r); err != nil {
		return nil, err
	}
	if err := r.Sealed.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
