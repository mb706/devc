package github

// Installation tokens of a GitHub App (work order C15 section 4): an RS256
// JWT signed with the App's key authenticates devc as the App; the
// installation that covers the repository is looked up unless configured,
// and POST /app/installations/<id>/access_tokens mints a token restricted
// to that one repository and the chosen permissions. Tokens are cached
// until five minutes before they expire.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// RefreshBefore is how long before its expiry a cached token is replaced.
const RefreshBefore = 5 * time.Minute

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

// Minter mints and caches installation tokens. The zero value is ready.
type Minter struct {
	// Now and HTTP are replaced by tests.
	Now  func() time.Time
	HTTP *http.Client

	mu       sync.Mutex
	tokens   map[string]Token
	installs map[string]int64
	locks    map[string]*sync.Mutex
}

func (m *Minter) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// lock serialises minting per cache key, so concurrent asks mint once.
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

// Token returns a token for repo with perms, from the cache while it is
// more than RefreshBefore from its expiry, else newly minted. key is the
// App's private key in PEM form.
func (m *Minter) Token(ctx context.Context, api *url.URL, app App, key []byte, repo Repo, perms map[string]string) (Token, error) {
	if err := CheckPermissions(perms); err != nil {
		return Token{}, err
	}
	if len(perms) == 0 {
		return Token{}, errors.New("no permissions to grant")
	}
	base := api.String() + "|" + strconv.FormatInt(app.ID, 10)
	ckey := base + "|" + strconv.FormatInt(app.InstallationID, 10) + "|" + repo.Owner + "/" + repo.Name + "|" + permissionsKey(perms)
	unlock := m.lock(ckey)
	defer unlock()
	m.mu.Lock()
	t, ok := m.tokens[ckey]
	m.mu.Unlock()
	if ok && m.now().Before(t.Expires.Add(-RefreshBefore)) {
		return t, nil
	}
	pk, err := ParsePrivateKey(key)
	if err != nil {
		return Token{}, err
	}
	jwt, err := AppJWT(pk, app.ID, m.now())
	if err != nil {
		return Token{}, err
	}
	c := NewClient(api, jwt)
	if m.HTTP != nil {
		c.HTTP = m.HTTP
	}
	inst := app.InstallationID
	if inst == 0 {
		ikey := base + "|" + repo.Owner + "/" + repo.Name
		m.mu.Lock()
		inst = m.installs[ikey]
		m.mu.Unlock()
		if inst == 0 {
			var out struct {
				ID int64 `json:"id"`
			}
			if err := c.Call(ctx, http.MethodGet, RepoPath(repo, "/installation"), nil, &out); err != nil {
				return Token{}, fmt.Errorf("finding the App's installation for %s: %w", repo, err)
			}
			if out.ID <= 0 {
				return Token{}, fmt.Errorf("GitHub named no installation of the App for %s", repo)
			}
			inst = out.ID
			m.mu.Lock()
			if m.installs == nil {
				m.installs = map[string]int64{}
			}
			m.installs[ikey] = inst
			m.mu.Unlock()
		}
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := map[string]any{"repositories": []string{repo.Name}, "permissions": perms}
	if err := c.Call(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(inst, 10)+"/access_tokens", body, &out); err != nil {
		return Token{}, fmt.Errorf("minting a token for %s: %w", repo, err)
	}
	if !validToken(out.Token) || out.ExpiresAt.IsZero() {
		return Token{}, fmt.Errorf("GitHub's answer for a token for %s holds no usable token", repo)
	}
	t = Token{Value: out.Token, Expires: out.ExpiresAt}
	m.mu.Lock()
	if m.tokens == nil {
		m.tokens = map[string]Token{}
	}
	m.tokens[ckey] = t
	m.mu.Unlock()
	return t, nil
}
