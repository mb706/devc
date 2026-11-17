// Package github is devc's side of GitHub (work order C15): the
// repository a project's upstream names, the REST requests of the
// read-only facade that boxes use through the daemon (built here from
// validated parts only), the host's own credential, GitHub App
// installation tokens (an RS256 JWT signed with the standard library,
// minted per repository with chosen permissions and cached), and the
// permission rules of [grants.github_token].
//
// Files: github.go holds the endpoints and the repository an upstream URL
// names; client.go the REST client every call goes through, the two writes
// (pull requests and comments) and the host credential; read.go the
// facade's validation (PlanRead) and its reads; mint.go installation
// tokens; jwt.go the App's JWT; perms.go the permission rules.
//
// Security rules: everything a box sends is hostile input (README rule 2).
// A box never supplies a URL, a method, a header or a repository outside
// its grant: PlanRead and ParseAPIPath accept only known operations and
// validated names, and the client builds every request itself on the
// configured API base. The host credential and minted tokens never appear
// in argument lists, logs or error messages (rule 5); every answer body and
// error is scrubbed of the client's own credential before devc passes it
// on. Requests are built only on the API base from DEVC_GITHUB_API
// (default https://api.github.com); a redirect is followed only to an
// https URL (job logs live on signed storage URLs) or, for a plain-http
// test API, to that API's own host. Go's client keeps the credential
// on a redirect to the API's host name, on any port, or to a subdomain of
// it, and drops it for every other host. DEVC_GITHUB_URL
// (default https://github.com) is the web and git host: it decides which
// upstreams are GitHub's, where the box's remote "github" points and which
// host `gh auth token` is asked about. Tests point both at fake servers.
package github

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Environment variables that point devc at another GitHub (tests, GitHub
// Enterprise): the REST API base and the web and git base.
const (
	EnvAPI     = "DEVC_GITHUB_API"
	EnvURL     = "DEVC_GITHUB_URL"
	DefaultAPI = "https://api.github.com"
	DefaultURL = "https://github.com"
)

// Endpoints are the base URLs devc talks to.
type Endpoints struct {
	// API is the REST API base; Web is the web and git host base.
	API *url.URL
	Web *url.URL
}

// DefaultEndpoints are github.com's.
func DefaultEndpoints() Endpoints {
	api, _ := url.Parse(DefaultAPI)
	web, _ := url.Parse(DefaultURL)
	return Endpoints{API: api, Web: web}
}

// EndpointsFromEnv reads DEVC_GITHUB_API and DEVC_GITHUB_URL (unset: the
// defaults).
func EndpointsFromEnv(getenv func(string) string) (Endpoints, error) {
	ep := DefaultEndpoints()
	for _, v := range []struct {
		env string
		dst **url.URL
	}{{EnvAPI, &ep.API}, {EnvURL, &ep.Web}} {
		s := getenv(v.env)
		if s == "" {
			continue
		}
		u, err := ParseBase(s)
		if err != nil {
			return ep, fmt.Errorf("%s: %w", v.env, err)
		}
		*v.dst = u
	}
	return ep, nil
}

// ParseBase checks a base URL: http or https, a host, no user information,
// query or fragment. A trailing slash is removed.
func ParseBase(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL", s)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("%q must be an http or https URL with a host and nothing after the path", s)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// Repo is a GitHub repository.
type Repo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Equal compares repositories as GitHub does, ignoring case.
func (r Repo) Equal(o Repo) bool {
	return strings.EqualFold(r.Owner, o.Owner) && strings.EqualFold(r.Name, o.Name)
}

var (
	ownerRE    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// ValidOwner and ValidName apply GitHub's rules for account and repository
// names.
func ValidOwner(s string) bool { return ownerRE.MatchString(s) }
func ValidName(s string) bool {
	return repoNameRE.MatchString(s) && s != "." && s != ".."
}

// ParseRepoName reads "owner/repo".
func ParseRepoName(s string) (Repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || !ValidOwner(owner) || !ValidName(name) {
		return Repo{}, fmt.Errorf("%q is not a GitHub repository (owner/name)", s)
	}
	return Repo{Owner: owner, Name: name}, nil
}

// ErrNotGitHub: the project's upstream is not on GitHub.
var ErrNotGitHub = errors.New("not a GitHub project")

// ParseRepo derives the repository from a project's upstream URL: an
// http(s), ssh or git URL, or [user@]host:owner/repo, on the host of web,
// with or without ".git".
func ParseRepo(upstream string, web *url.URL) (Repo, error) {
	notGH := func() (Repo, error) {
		if upstream == "" {
			return Repo{}, fmt.Errorf("%w: the project has no upstream", ErrNotGitHub)
		}
		return Repo{}, fmt.Errorf("%w: its upstream %q is not a repository on %s", ErrNotGitHub, upstream, web.Host)
	}
	if upstream == "" || strings.ContainsAny(upstream, " \t\r\n\x00") {
		return notGH()
	}
	var host, p string
	if strings.Contains(upstream, "://") {
		u, err := url.Parse(upstream)
		if err != nil || u.RawQuery != "" || u.Fragment != "" {
			return notGH()
		}
		host, p = u.Hostname(), u.Path
		switch u.Scheme {
		case "https", "http":
			if !sameHost(u, web) {
				return notGH()
			}
			// A web base with a path (GitHub Enterprise under a prefix).
			if web.Path != "" {
				rest, ok := strings.CutPrefix(p, web.Path+"/")
				if !ok {
					return notGH()
				}
				p = rest
			}
		case "ssh", "git":
		default:
			return notGH()
		}
	} else {
		// [user@]host:path
		hp, rest, ok := strings.Cut(upstream, ":")
		if !ok {
			return notGH()
		}
		if i := strings.LastIndex(hp, "@"); i >= 0 {
			hp = hp[i+1:]
		}
		host, p = hp, rest
	}
	if !strings.EqualFold(host, web.Hostname()) {
		return notGH()
	}
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimSuffix(p, ".git")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || !ValidOwner(parts[0]) || !ValidName(parts[1]) {
		return notGH()
	}
	return Repo{Owner: parts[0], Name: parts[1]}, nil
}

// sameHost compares the host and effective port of two http(s) URLs.
func sameHost(a, b *url.URL) bool {
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "http" {
		return "80"
	}
	return "443"
}

// RemoteURL is the repository's HTTPS (or, for a test server, HTTP) URL on
// the web host: what the box's second remote "github" points at.
func RemoteURL(web *url.URL, r Repo) string {
	u := *web
	u.Path = web.Path + "/" + r.Owner + "/" + r.Name + ".git"
	u.RawPath = ""
	return u.String()
}

// CredentialScope is the URL a git credential helper is registered for:
// scheme and host of the web base (credential.<scope>.helper).
func CredentialScope(web *url.URL) string {
	return web.Scheme + "://" + web.Host
}
