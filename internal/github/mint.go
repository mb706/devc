package github

// Installation tokens of a GitHub App (work order C15 section 4): an RS256
// JWT signed with the App's key authenticates devc as the App, and POST
// /app/installations/<id>/access_tokens mints a token restricted to one
// repository and the chosen permissions. Tokens are cached until five
// minutes before they expire.
//
// Before the first token for a repository (finding D2-03), the
// installation is verified to cover exactly that owner and repository: the
// installation GitHub names for the repository must belong to its owner
// and be the configured one, if any; then a metadata-only probe token for
// the repository's name reads the one repository it can see from GET
// /installation/repositories, whose full name must be the project's, and
// is revoked. Tokens are restricted by that verified repository ID.
//
// Caches are per project and per App key (finding D2-09): their keys hold
// the project's scope and a fingerprint of the key's public half, and the
// key is parsed before any cache lookup, so a project with a broken or
// different key never receives another project's token.

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RefreshBefore is how long before its expiry a cached token is replaced.
const RefreshBefore = 5 * time.Minute

// verifiedFor is how long a verified installation and repository ID are
// trusted before they are verified again (a failed mint forgets them at
// once).
const verifiedFor = 24 * time.Hour

// App is a realm's [github.app].
type App struct {
	ID             int64
	InstallationID int64
}

// Token is a minted installation token.
type Token struct {
	Value   string
	Expires time.Time
}

// target is a verified installation and repository.
type target struct {
	Installation int64
	RepoID       int64
	At           time.Time
}

// Minter mints and caches installation tokens. The zero value is ready.
type Minter struct {
	// Now and HTTP are replaced by tests.
	Now  func() time.Time
	HTTP *http.Client

	mu       sync.Mutex
	tokens   map[string]Token
	verified map[string]target
	locks    map[string]*sync.Mutex
}

func (m *Minter) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// lock serialises verification and minting per key, so concurrent asks
// mint once.
func (m *Minter) lock(key string) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	l := m.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// KeyFingerprint identifies an App key by its public half.
func KeyFingerprint(pk *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&pk.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// Token returns a token for repo with perms for the project scope (its
// "<realm>/<project>"), from the cache while it is more than RefreshBefore
// from its expiry, else newly minted. key is the App's private key in PEM
// form.
func (m *Minter) Token(ctx context.Context, api *url.URL, scope string, app App, key []byte, repo Repo, perms map[string]string) (Token, error) {
	if scope == "" || strings.Contains(scope, "|") {
		return Token{}, fmt.Errorf("bad project scope %q", scope)
	}
	if app.ID <= 0 || app.InstallationID < 0 {
		return Token{}, fmt.Errorf("bad App identity %d/%d", app.ID, app.InstallationID)
	}
	if err := CheckPermissions(perms); err != nil {
		return Token{}, err
	}
	if len(perms) == 0 {
		return Token{}, errors.New("no permissions to grant")
	}
	// The key is checked before any cache lookup.
	pk, err := ParsePrivateKey(key)
	if err != nil {
		return Token{}, err
	}
	fp, err := KeyFingerprint(pk)
	if err != nil {
		return Token{}, err
	}
	base := strings.Join([]string{api.String(), scope, strconv.FormatInt(app.ID, 10), fp,
		strconv.FormatInt(app.InstallationID, 10), strings.ToLower(repo.String())}, "|")
	ckey := base + "|" + permissionsKey(perms)
	unlock := m.lock(base)
	defer unlock()
	m.mu.Lock()
	t, ok := m.tokens[ckey]
	m.mu.Unlock()
	if ok && m.now().Before(t.Expires.Add(-RefreshBefore)) {
		return t, nil
	}
	jwt, err := AppJWT(pk, app.ID, m.now())
	if err != nil {
		return Token{}, err
	}
	c := m.client(api, jwt)
	tg, err := m.verify(ctx, api, c, base, app, repo)
	if err != nil {
		return Token{}, err
	}
	t, err = m.mint(ctx, api, c, tg.Installation, map[string]any{"repository_ids": []int64{tg.RepoID}, "permissions": perms}, tg.RepoID)
	if err != nil {
		m.mu.Lock()
		delete(m.verified, base)
		m.mu.Unlock()
		return Token{}, fmt.Errorf("minting a token for %s: %w", repo, err)
	}
	m.mu.Lock()
	if m.tokens == nil {
		m.tokens = map[string]Token{}
	}
	m.tokens[ckey] = t
	m.mu.Unlock()
	return t, nil
}

func (m *Minter) client(api *url.URL, tok string) *Client {
	c := NewClient(api, tok)
	if m.HTTP != nil {
		c.HTTP = m.HTTP
	}
	return c
}

// verify establishes that the App's installation covers exactly repo and
// returns it with the repository's ID (cached under base).
func (m *Minter) verify(ctx context.Context, api *url.URL, c *Client, base string, app App, repo Repo) (target, error) {
	m.mu.Lock()
	tg, ok := m.verified[base]
	m.mu.Unlock()
	if ok && m.now().Sub(tg.At) < verifiedFor && !m.now().Before(tg.At) {
		return tg, nil
	}
	var inst struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := c.Call(ctx, http.MethodGet, RepoPath(repo, "/installation"), nil, &inst); err != nil {
		return target{}, fmt.Errorf("finding the App's installation for %s: %w", repo, err)
	}
	switch {
	case inst.ID <= 0:
		return target{}, fmt.Errorf("GitHub named no installation of the App for %s", repo)
	case !strings.EqualFold(inst.Account.Login, repo.Owner):
		return target{}, fmt.Errorf("the App's installation for %s belongs to %q, not to %s", repo, inst.Account.Login, repo.Owner)
	case app.InstallationID != 0 && inst.ID != app.InstallationID:
		return target{}, fmt.Errorf("the configured installation %d does not cover %s (GitHub names installation %d for it): correct installation_id under [github.app] or remove it",
			app.InstallationID, repo, inst.ID)
	}
	// A metadata-only probe token for the repository's name shows which
	// repository the name means in this installation, and its ID.
	probe, err := m.mint(ctx, api, c, inst.ID, map[string]any{"repositories": []string{repo.Name}, "permissions": map[string]string{"metadata": "read"}}, 0)
	if err != nil {
		return target{}, fmt.Errorf("checking %s in the App's installation %d: %w", repo, inst.ID, err)
	}
	pc := m.client(api, probe.Value)
	// The probe never leaves the daemon; revoking it is a courtesy.
	defer m.revoke(ctx, api, probe.Value)
	var list struct {
		TotalCount   int `json:"total_count"`
		Repositories []struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := pc.Call(ctx, http.MethodGet, "/installation/repositories", nil, &list); err != nil {
		return target{}, fmt.Errorf("checking %s in the App's installation %d: %w", repo, inst.ID, err)
	}
	if list.TotalCount != 1 || len(list.Repositories) != 1 || list.Repositories[0].ID <= 0 ||
		!strings.EqualFold(list.Repositories[0].FullName, repo.String()) {
		names := []string{}
		for _, r := range list.Repositories {
			names = append(names, r.FullName)
		}
		return target{}, fmt.Errorf("the App's installation %d does not cover exactly %s (a token for its name sees %d repositories: %s)",
			inst.ID, repo, list.TotalCount, strings.Join(names, ", "))
	}
	tg = target{Installation: inst.ID, RepoID: list.Repositories[0].ID, At: m.now()}
	m.mu.Lock()
	if m.verified == nil {
		m.verified = map[string]target{}
	}
	m.verified[base] = tg
	m.mu.Unlock()
	return tg, nil
}

// revoke ends a token devc does not hand out (best effort).
func (m *Minter) revoke(ctx context.Context, api *url.URL, tok string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = m.client(api, tok).Call(rctx, http.MethodDelete, "/installation/token", nil, nil)
}

// mint asks for an installation token. With repoID set, a token whose
// answer lists other repositories than that one is revoked and refused.
func (m *Minter) mint(ctx context.Context, api *url.URL, c *Client, inst int64, body map[string]any, repoID int64) (Token, error) {
	var out struct {
		Token        string    `json:"token"`
		ExpiresAt    time.Time `json:"expires_at"`
		Repositories []struct {
			ID int64 `json:"id"`
		} `json:"repositories"`
	}
	if err := c.Call(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(inst, 10)+"/access_tokens", body, &out); err != nil {
		return Token{}, err
	}
	if !validToken(out.Token) || out.ExpiresAt.IsZero() {
		return Token{}, errors.New("GitHub's answer holds no usable token")
	}
	if repoID != 0 && out.Repositories != nil {
		ids := make([]int64, 0, len(out.Repositories))
		for _, r := range out.Repositories {
			ids = append(ids, r.ID)
		}
		if !slices.Equal(ids, []int64{repoID}) {
			m.revoke(ctx, api, out.Token)
			return Token{}, fmt.Errorf("GitHub's token covers the repositories %v, not only %d", ids, repoID)
		}
	}
	return Token{Value: out.Token, Expires: out.ExpiresAt}, nil
}
