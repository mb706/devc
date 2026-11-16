package config

// Service definitions (work order C18): realms/<realm>/services/<name>/
// service.toml describes a service that boxes of the realm's projects reach
// through the daemon, which holds the credential and adds it on the way
// out. A project gets a service with grants.services. Everything here is
// policy from the configuration directory (README rule 1); the daemon
// enforces it on every request.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/names"
)

// Service is realms/<realm>/services/<name>/service.toml.
type Service struct {
	Name  string `toml:"-" json:"name"`
	Realm string `toml:"-" json:"realm"`
	File  string `toml:"-" json:"-"`
	Dir   string `toml:"-" json:"-"`

	// Upstream is the scheme and host (with an optional port) the proxy
	// talks to; https unless AllowInsecure.
	Upstream      string `toml:"upstream" json:"upstream"`
	AllowInsecure bool   `toml:"allow_insecure" json:"allow_insecure,omitempty"`
	// Port is the loopback port in granted boxes.
	Port   int           `toml:"port" json:"port"`
	Auth   ServiceAuth   `toml:"auth" json:"auth"`
	Allow  []ServiceRule `toml:"allow" json:"allow"`
	Limits ServiceLimits `toml:"limits" json:"limits"`
}

// ServiceAuth says how the credential is added. Kind "header" sets Header
// to the value of the realm secret ValueSecret; kind "cookie" attaches the
// cookie jar that Login produced (devc service login): the command runs in
// a throwaway container from Image with the realm secrets Secrets as files,
// and a jar older than Refresh, a 401 or a redirect to LoginPath means the
// service needs a new login, which the daemon performs by itself only with
// Auto.
type ServiceAuth struct {
	Kind        string   `toml:"kind" json:"kind"`
	Header      string   `toml:"header" json:"header,omitempty"`
	ValueSecret string   `toml:"value_secret" json:"value_secret,omitempty"`
	Login       []string `toml:"login" json:"login,omitempty"`
	Refresh     string   `toml:"refresh" json:"refresh,omitempty"`
	Image       string   `toml:"image" json:"image,omitempty"`
	Secrets     []string `toml:"secrets" json:"secrets,omitempty"`
	LoginPath   string   `toml:"login_path" json:"login_path,omitempty"`
	Auto        bool     `toml:"auto" json:"auto,omitempty"`
}

// ServiceRule allows the listed methods on paths below Path (first match
// wins; everything else is refused).
type ServiceRule struct {
	Methods []string `toml:"methods" json:"methods"`
	Path    string   `toml:"path" json:"path"`
}

// ServiceLimits bound one request: MaxBody each body (request and
// response), Timeout the whole exchange.
type ServiceLimits struct {
	MaxBody string `toml:"max_body" json:"max_body"`
	Timeout string `toml:"timeout" json:"timeout"`
}

// Service authentication kinds.
const (
	ServiceAuthHeader = "header"
	ServiceAuthCookie = "cookie"
)

// Defaults and bounds of service definitions.
const (
	DefaultServiceRefresh = "12h"
	DefaultServiceMaxBody = "10m"
	DefaultServiceTimeout = "60s"
	// MaxServiceNameLen keeps the name of the cookie jar's secret
	// (JarSecretPrefix + name) within the secret name limit.
	MaxServiceNameLen = names.MaxLen - len(JarSecretPrefix)
	// maxServiceRules bounds [[allow]].
	maxServiceRules = 64
	// maxServiceSecrets bounds auth.secrets.
	maxServiceSecrets = 16
	// boxProxyPort is the allowlist proxy's port in boxes (C13).
	boxProxyPort = 3128
)

// JarSecretPrefix starts the name of the realm secret that holds a cookie
// service's jar: "_jar.<service>".
const JarSecretPrefix = "_jar."

// JarSecret is the realm secret that holds the cookie jar of service name.
func JarSecret(name string) string { return JarSecretPrefix + name }

// ServiceMethods are the methods a rule may allow: the standard ones
// without CONNECT and TRACE.
var ServiceMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// serviceKey indexes Config.services.
func serviceKey(realm, name string) string { return realm + "/" + name }

func defaultService() Service {
	return Service{Limits: ServiceLimits{MaxBody: DefaultServiceMaxBody, Timeout: DefaultServiceTimeout}}
}

// Service returns a realm's service by name.
func (c *Config) Service(realm, name string) (*Service, bool) {
	s, ok := c.services[serviceKey(realm, name)]
	return s, ok
}

// Services returns the services of a realm ("" for all), sorted by realm
// and name.
func (c *Config) Services(realm string) []*Service {
	var out []*Service
	for _, s := range c.services {
		if realm == "" || s.Realm == realm {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b *Service) int {
		return strings.Compare(serviceKey(a.Realm, a.Name), serviceKey(b.Realm, b.Name))
	})
	return out
}

// ServiceProblems returns the problems of a service's definition.
func (c *Config) ServiceProblems(realm, name string) []Problem {
	if !names.Valid(name) || !names.ValidRealm(realm) {
		return nil
	}
	dir := c.Paths.ServiceDefDir(realm, name)
	var out []Problem
	for _, pr := range c.Problems {
		if pr.File == dir || strings.HasPrefix(pr.File, dir+string(filepath.Separator)) {
			out = append(out, pr)
		}
	}
	return out
}

// GrantedTo lists the projects of the service's realm that grant it.
func (c *Config) GrantedTo(s *Service) []names.ProjectRef {
	var out []names.ProjectRef
	for _, p := range c.Projects(s.Realm) {
		if slices.Contains(p.Grants.Services, s.Name) {
			out = append(out, p.Ref())
		}
	}
	return out
}

func (c *Config) loadServices(realm string, ps *problems) {
	dir := c.Paths.ServiceDefsDir(realm)
	for _, name := range subdirs(dir, ps) {
		if err := names.Check(names.KindService, name); err != nil {
			ps.add(filepath.Join(dir, name), "", "%v", err)
			continue
		}
		if len(name) > MaxServiceNameLen {
			ps.add(filepath.Join(dir, name), "", "service name %q is longer than %d characters (its cookie jar is the secret %s<name>)", name, MaxServiceNameLen, JarSecretPrefix)
			continue
		}
		s := defaultService()
		s.Name, s.Realm, s.Dir, s.File = name, realm, c.Paths.ServiceDefDir(realm, name), c.Paths.ServiceFile(realm, name)
		data, ok := readOptional(s.File, ps)
		if !ok {
			ps.add(s.File, "", "missing: every service directory needs a service.toml")
			continue
		}
		if !decode(s.File, data, &s, ps) {
			continue
		}
		if s.Auth.Kind == ServiceAuthCookie && s.Auth.Refresh == "" {
			s.Auth.Refresh = DefaultServiceRefresh
		}
		c.services[serviceKey(realm, name)] = &s
	}
}

// UpstreamURL is the parsed upstream (validated with the configuration).
func (s *Service) UpstreamURL() (*url.URL, error) {
	return parseServiceUpstream(s.Upstream, s.AllowInsecure)
}

// MaxBodyBytes is limits.max_body in bytes.
func (s *Service) MaxBodyBytes() int64 {
	return sizeOr(s.Limits.MaxBody, 10<<20)
}

// TimeoutDuration is limits.timeout.
func (s *Service) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(s.Limits.Timeout); err == nil && d > 0 {
		return d
	}
	return time.Minute
}

// RefreshDuration is auth.refresh (cookie services).
func (s *Service) RefreshDuration() time.Duration {
	if d, err := time.ParseDuration(s.Auth.Refresh); err == nil && d > 0 {
		return d
	}
	return 12 * time.Hour
}

// BaseURL is the service's address in a granted box.
func (s *Service) BaseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.Port)
}

// CredentialSecrets are the realm secrets that hold the service's
// credential or what produces it; none may be granted to a box that uses
// the service.
func (s *Service) CredentialSecrets() []string {
	switch s.Auth.Kind {
	case ServiceAuthHeader:
		return []string{s.Auth.ValueSecret}
	case ServiceAuthCookie:
		return append([]string{JarSecret(s.Name)}, s.Auth.Secrets...)
	}
	return nil
}

// Fingerprint identifies what a box started with this service may reach
// and with which credential: upstream, port, authentication (without the
// login command) and rules. A box is served only while the fingerprint of
// its start still holds (finding C1-02: nothing grows without a restart).
func (s *Service) Fingerprint() string {
	b, _ := json.Marshal(struct {
		Upstream      string
		AllowInsecure bool
		Port          int
		Kind, Header  string
		ValueSecret   string
		Allow         []ServiceRule
	}{s.Upstream, s.AllowInsecure, s.Port, s.Auth.Kind, s.Auth.Header, s.Auth.ValueSecret, s.Allow})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// parseServiceUpstream checks an upstream: https (http only with
// allowInsecure), a host, nothing but an optional "/" as path, no user
// information, query or fragment.
func parseServiceUpstream(v string, allowInsecure bool) (*url.URL, error) {
	if strings.ContainsAny(v, " \t\x00\r\n\\") {
		return nil, fmt.Errorf("%q contains whitespace, control characters or a backslash", v)
	}
	u, err := url.Parse(v)
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL", v)
	}
	switch {
	case u.Scheme == "http" && !allowInsecure:
		return nil, fmt.Errorf("%q is plain http: the credential would travel unencrypted; use https, or set allow_insecure = true", v)
	case u.Scheme != "https" && u.Scheme != "http":
		return nil, fmt.Errorf("%q must be an https:// URL", v)
	case u.Host == "" || u.Hostname() == "":
		return nil, fmt.Errorf("%q has no host", v)
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return nil, fmt.Errorf("%q must be only a scheme and a host (no user, query or fragment)", v)
	case u.Path != "" && u.Path != "/":
		return nil, fmt.Errorf("%q must be only a scheme and a host, without a path: rules name the paths", v)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%q has an invalid port", v)
		}
	}
	return &url.URL{Scheme: u.Scheme, Host: strings.ToLower(u.Host)}, nil
}

// headerTokenRE characters of an HTTP header name (RFC 9110 token).
func validHeaderName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

// reservedHeaders may not carry a service's credential: framing, routing
// and hop-by-hop headers, and Cookie (kind "cookie" does that).
var reservedHeaders = []string{"host", "content-length", "transfer-encoding", "connection", "keep-alive", "upgrade", "te", "trailer",
	"proxy-connection", "proxy-authenticate", "expect", "cookie", "set-cookie", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto"}

// CheckRulePath reports why p is not a valid rule path: it must start with
// "/" and already be in the normal form requests are compared in (no "."
// or ".." segments, no empty segment except a final one, no
// percent-encoding of letters, digits or "-._~", no encoded "/", "\" or
// "%", only characters allowed in a URL path).
func CheckRulePath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%q must start with /", p)
	}
	if len(p) > 1024 {
		return fmt.Errorf("%q is too long", p)
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '%':
			if i+2 >= len(p) || !isHex(p[i+1]) || !isHex(p[i+2]) {
				return fmt.Errorf("%q has an invalid percent-encoding", p)
			}
			b := unhex(p[i+1])<<4 | unhex(p[i+2])
			if isUnreserved(b) || b == '/' || b == '\\' || b == '%' || b < 0x20 || b == 0x7f || p[i+1] >= 'a' || p[i+2] >= 'a' ||
				strings.IndexByte(":?#[]@!$&'()*+,;=", b) >= 0 {
				return fmt.Errorf("%q: write %%%c%c as the character itself or in upper case; encoded /, \\, %%, reserved and control characters are never allowed", p, p[i+1], p[i+2])
			}
			i += 2
		case !isPathChar(c):
			return fmt.Errorf("%q contains %q, which is not allowed in a URL path", p, string(c))
		}
	}
	segs := strings.Split(p[1:], "/")
	for i, seg := range segs {
		base, _, _ := strings.Cut(seg, ";")
		if base == "." || base == ".." {
			return fmt.Errorf("%q contains a %q segment", p, base)
		}
		if seg == "" && i != len(segs)-1 {
			return fmt.Errorf("%q contains an empty segment (//)", p)
		}
	}
	return nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

// isPathChar: unreserved, sub-delims, ':' and '@' (RFC 3986 pchar) and '/'.
func isPathChar(c byte) bool {
	return isUnreserved(c) || strings.IndexByte("!$&'()*+,;=:@/", c) >= 0
}

// validateServices checks the services of a realm.
func validateServices(c *Config, realm string, ps *problems) {
	ports := map[int]string{}
	for _, s := range c.Services(realm) {
		k := checker{file: s.File, ps: ps}
		validateService(k, s)
		if s.Port > 0 {
			if other, dup := ports[s.Port]; dup {
				k.bad("port", "port %d is also the port of service %q: every service of a realm needs its own", s.Port, other)
			}
			ports[s.Port] = s.Name
		}
	}
}

func validateService(k checker, s *Service) {
	if _, err := parseServiceUpstream(s.Upstream, s.AllowInsecure); err != nil {
		k.bad("upstream", "%v", err)
	}
	switch {
	case s.Port <= 1024 || s.Port > 65535:
		k.bad("port", "%d must be a port above 1024 (the loopback port in granted boxes)", s.Port)
	case s.Port == boxProxyPort:
		k.bad("port", "%d is the allowlist proxy's port in boxes", s.Port)
	}
	a := s.Auth
	only := func(key string, set bool, kind string) {
		if set {
			k.bad("auth."+key, "applies only to kind = %q", kind)
		}
	}
	switch a.Kind {
	case ServiceAuthHeader:
		switch {
		case !validHeaderName(a.Header):
			k.bad("auth.header", "%q is not an HTTP header name", a.Header)
		case slices.Contains(reservedHeaders, strings.ToLower(a.Header)) || strings.HasPrefix(strings.ToLower(a.Header), "proxy-"):
			k.bad("auth.header", "%q cannot carry the credential (use kind = \"cookie\" for cookies)", a.Header)
		}
		if a.ValueSecret == "" {
			k.bad("auth.value_secret", "name the realm secret that holds the header's value ('devc secret set %s <name>')", s.Realm)
		} else {
			k.name("auth.value_secret", names.KindSecret, a.ValueSecret)
		}
		only("login", len(a.Login) > 0, ServiceAuthCookie)
		only("refresh", a.Refresh != "", ServiceAuthCookie)
		only("image", a.Image != "", ServiceAuthCookie)
		only("secrets", len(a.Secrets) > 0, ServiceAuthCookie)
		only("login_path", a.LoginPath != "", ServiceAuthCookie)
		only("auto", a.Auto, ServiceAuthCookie)
	case ServiceAuthCookie:
		only("header", a.Header != "", ServiceAuthHeader)
		only("value_secret", a.ValueSecret != "", ServiceAuthHeader)
		if len(a.Login) == 0 {
			k.bad("auth.login", "name the command that logs in and prints the cookies as JSON (see examples/services)")
		}
		for i, w := range a.Login {
			if w == "" && i == 0 || strings.ContainsAny(w, "\x00") || len(w) > 4096 {
				k.bad(idx("auth.login", i), "%q is not a usable argument", w)
			}
		}
		if len(a.Login) > 64 {
			k.bad("auth.login", "has more than 64 arguments")
		}
		if d, err := time.ParseDuration(a.Refresh); err != nil || d < time.Minute || d > 90*24*time.Hour {
			k.bad("auth.refresh", "%q must be a duration between 1m and 2160h, such as 12h", a.Refresh)
		}
		if a.Image != "" {
			k.image("auth.image", a.Image)
		}
		if len(a.Secrets) > maxServiceSecrets {
			k.bad("auth.secrets", "lists more than %d secrets", maxServiceSecrets)
		}
		for i, n := range a.Secrets {
			if k.name(idx("auth.secrets", i), names.KindSecret, n) && strings.HasPrefix(n, JarSecretPrefix) {
				k.bad(idx("auth.secrets", i), "%q is a cookie jar, not a login secret", n)
			}
		}
		k.noDuplicates("auth.secrets", a.Secrets)
		if a.LoginPath != "" {
			if err := CheckRulePath(a.LoginPath); err != nil {
				k.bad("auth.login_path", "%v", err)
			}
		}
	default:
		k.bad("auth.kind", "%q is not one of %s, %s", a.Kind, ServiceAuthHeader, ServiceAuthCookie)
	}
	if len(s.Allow) == 0 {
		k.bad("allow", "name at least one rule ([[allow]] with methods and path); everything else is refused")
	}
	if len(s.Allow) > maxServiceRules {
		k.bad("allow", "has more than %d rules", maxServiceRules)
	}
	for i, r := range s.Allow {
		key := idx("allow", i)
		if len(r.Methods) == 0 {
			k.bad(key+".methods", "name at least one method")
		}
		for j, m := range r.Methods {
			if !slices.Contains(ServiceMethods, m) {
				k.bad(idx(key+".methods", j), "%q is not one of %s (in upper case)", m, strings.Join(ServiceMethods, ", "))
			}
		}
		k.noDuplicates(key+".methods", r.Methods)
		if err := CheckRulePath(r.Path); err != nil {
			k.bad(key+".path", "%v", err)
		}
	}
	if n, err := ParseSize(s.Limits.MaxBody); err != nil || n < 1 || n > 4<<30 {
		k.bad("limits.max_body", "%q must be a size between 1b and 4g, such as 10m", s.Limits.MaxBody)
	}
	if d, err := time.ParseDuration(s.Limits.Timeout); err != nil || d < time.Second || d > time.Hour {
		k.bad("limits.timeout", "%q must be a duration between 1s and 1h, such as 60s", s.Limits.Timeout)
	}
}

// validateServiceGrants checks grants.services of a project: services of
// its realm that loaded, listed once, whose credential secrets the project
// does not also receive, and whose ports are free in its box.
func validateServiceGrants(c *Config, k checker, p *Project, harnessSecrets []string) {
	k.noDuplicates("grants.services", p.Grants.Services)
	for i, name := range p.Grants.Services {
		key := idx("grants.services", i)
		if !k.name(key, names.KindService, name) {
			continue
		}
		s, ok := c.services[serviceKey(p.Realm, name)]
		if !ok {
			if len(name) <= MaxServiceNameLen && fileExists(c.Paths.ServiceFile(p.Realm, name)) {
				k.bad(key, "the definition of service %q failed to load (see its problems)", name)
			} else {
				k.bad(key, "no service %q in realm %q (realms/%s/services/%s/service.toml)", name, p.Realm, p.Realm, name)
			}
			continue
		}
		for _, sec := range s.CredentialSecrets() {
			if slices.Contains(p.Grants.Secrets, sec) || slices.Contains(harnessSecrets, sec) {
				k.bad(key, "the secret %s holds the credential of service %q, and the project also receives it (grants.secrets or a harness): the credential would enter the box", sec, name)
			}
		}
		if slices.Contains(p.Box.Ports, s.Port) {
			k.bad(key, "service %q uses port %d in the box, which box.ports lists for the box's own server", name, s.Port)
		}
	}
	for i, sec := range p.Grants.Secrets {
		if strings.HasPrefix(sec, JarSecretPrefix) {
			k.bad(idx("grants.secrets", i), "%q is a service's cookie jar; grant the service instead (grants.services)", sec)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
