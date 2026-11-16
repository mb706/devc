package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/names"
)

const goodService = `upstream = "https://wiki.example.org"
port = 8601

[auth]
kind = "header"
header = "Authorization"
value_secret = "WIKI_TOKEN"

[[allow]]
methods = ["GET", "HEAD"]
path = "/api/"
`

const goodCookieService = `upstream = "https://intranet.example.org:8443"
port = 8602

[auth]
kind = "cookie"
login = ["bash", "login.sh"]
secrets = ["INTRANET_USER", "INTRANET_PASSWORD"]
login_path = "/login"
auto = true

[[allow]]
methods = ["GET"]
path = "/"
`

func TestServices(t *testing.T) {
	dir := copyExample(t)
	write(t, dir, "realms/demo/services/wiki/service.toml", goodService)
	write(t, dir, "realms/demo/services/intranet/service.toml", goodCookieService)
	write(t, dir, "realms/demo/services/intranet/login.sh", "#!/bin/sh\n")
	appendTo(t, dir, "realms/demo/projects/hello/project.toml", "[grants]\nservices = [\"wiki\", \"intranet\"]")
	c, probs := load(t, dir)
	if len(probs) != 0 {
		t.Fatalf("problems: %v", probs)
	}
	s, ok := c.Service("demo", "intranet")
	if !ok || s.Auth.Refresh != DefaultServiceRefresh || s.Limits.MaxBody != DefaultServiceMaxBody || s.MaxBodyBytes() != 10<<20 || s.TimeoutDuration().Seconds() != 60 {
		t.Fatalf("intranet: %+v", s)
	}
	if u, err := s.UpstreamURL(); err != nil || u.String() != "https://intranet.example.org:8443" || u.Hostname() != "intranet.example.org" {
		t.Errorf("upstream: %v %v", u, err)
	}
	if got := s.CredentialSecrets(); strings.Join(got, ",") != "_jar.intranet,INTRANET_USER,INTRANET_PASSWORD" {
		t.Errorf("credential secrets: %v", got)
	}
	if len(c.Services("demo")) != 2 || len(c.Services("other")) != 0 || len(c.Services("")) != 2 {
		t.Errorf("services: %d", len(c.Services("")))
	}
	ref := names.ProjectRef{Realm: "demo", Project: "hello"}
	r, err := c.Resolved(ref)
	if err != nil || len(r.Services) != 2 || r.Services[0].Name != "wiki" || strings.Join(r.Grants.Services, ",") != "wiki,intranet" {
		t.Fatalf("resolved: %+v %v", r.Services, err)
	}
	if got := c.GrantedTo(s); len(got) != 1 || got[0] != ref {
		t.Errorf("granted to: %v", got)
	}
	// The fingerprint follows what the box may reach, not the login.
	fp := s.Fingerprint()
	s2 := *s
	s2.Auth.Login = []string{"other"}
	s2.Auth.Refresh = "1h"
	if s2.Fingerprint() != fp {
		t.Error("the login command changed the fingerprint")
	}
	s2.Allow = append([]ServiceRule{}, s.Allow...)
	s2.Allow[0].Path = "/admin/"
	if s2.Fingerprint() == fp {
		t.Error("a rule change kept the fingerprint")
	}
	s3 := *s
	s3.Upstream = "https://other.example.org"
	if s3.Fingerprint() == fp {
		t.Error("an upstream change kept the fingerprint")
	}

	// A broken granted service blocks the project; a broken service that
	// is not granted does not.
	write(t, dir, "realms/demo/services/wiki/service.toml", "bogus = 1\n"+goodService)
	write(t, dir, "realms/demo/services/unused/service.toml", "port = 1\n")
	c, _ = load(t, dir)
	if got := c.ProblemsFor(ref); len(got) != 1 || got[0].Key != "bogus" {
		t.Errorf("problems for hello: %v", got)
	}
	if got := c.ProblemsFor(names.ProjectRef{Realm: "demo", Project: "analysis"}); len(got) != 0 {
		t.Errorf("problems for analysis: %v", got)
	}
	if got := c.ServiceProblems("demo", "unused"); len(got) == 0 {
		t.Error("no problems for the broken service")
	}
}

func TestBrokenServices(t *testing.T) {
	const svc = "realms/demo/services/wiki/service.toml"
	const hello = "realms/demo/projects/hello/project.toml"
	withService := func(content string) func(*testing.T, string) {
		return func(t *testing.T, dir string) { write(t, dir, svc, content) }
	}
	replace := func(old, new string) func(*testing.T, string) {
		return withService(strings.Replace(goodService, old, new, 1))
	}
	cookie := func(old, new string) func(*testing.T, string) {
		return withService(strings.Replace(goodCookieService, old, new, 1))
	}
	grant := func(extra string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			write(t, dir, svc, goodService)
			appendTo(t, dir, hello, extra)
		}
	}
	cases := []brokenCase{
		{"http upstream", replace("https://wiki", "http://wiki"), svc, "upstream", "allow_insecure"},
		{"upstream path", replace("wiki.example.org", "wiki.example.org/api"), svc, "upstream", "without a path"},
		{"upstream user", replace("https://wiki", "https://u:p@wiki"), svc, "upstream", "only a scheme and a host"},
		{"upstream scheme", replace("https://wiki", "ftp://wiki"), svc, "upstream", "https://"},
		{"upstream query", replace("wiki.example.org", "wiki.example.org?x=1"), svc, "upstream", "only a scheme and a host"},
		{"low port", replace("8601", "80"), svc, "port", "above 1024"},
		{"proxy port", replace("8601", "3128"), svc, "port", "allowlist proxy"},
		{"kind", replace(`kind = "header"`, `kind = "basic"`), svc, "auth.kind", "not one of"},
		{"header name", replace(`header = "Authorization"`, `header = "Bad Header"`), svc, "auth.header", "not an HTTP header name"},
		{"reserved header", replace(`header = "Authorization"`, `header = "Host"`), svc, "auth.header", "cannot carry"},
		{"cookie header", replace(`header = "Authorization"`, `header = "Cookie"`), svc, "auth.header", "cannot carry"},
		{"proxy header", replace(`header = "Authorization"`, `header = "Proxy-Authorization"`), svc, "auth.header", "cannot carry"},
		{"no secret", replace(`value_secret = "WIKI_TOKEN"`, ``), svc, "auth.value_secret", "realm secret"},
		{"secret name", replace(`"WIKI_TOKEN"`, `"../x"`), svc, "auth.value_secret", "invalid secret name"},
		{"login on header", replace(`value_secret = "WIKI_TOKEN"`, "value_secret = \"WIKI_TOKEN\"\nlogin = [\"x\"]"), svc, "auth.login", "only to kind = \"cookie\""},
		{"no rules", withService(strings.Split(goodService, "[[allow]]")[0]), svc, "allow", "at least one rule"},
		{"method", replace(`"GET", "HEAD"`, `"GET", "TRACE"`), svc, "allow[0].methods[1]", "not one of"},
		{"lower-case method", replace(`"GET", "HEAD"`, `"get"`), svc, "allow[0].methods[0]", "not one of"},
		{"no methods", replace(`"GET", "HEAD"`, ``), svc, "allow[0].methods", "at least one"},
		{"repeated method", replace(`"GET", "HEAD"`, `"GET", "GET"`), svc, "allow[0].methods[1]", "listed twice"},
		{"relative path", replace(`path = "/api/"`, `path = "api/"`), svc, "allow[0].path", "must start with /"},
		{"dot-dot path", replace(`path = "/api/"`, `path = "/api/../x"`), svc, "allow[0].path", "segment"},
		{"double slash", replace(`path = "/api/"`, `path = "//api"`), svc, "allow[0].path", "empty segment"},
		{"encoded slash", replace(`path = "/api/"`, `path = "/api%2Fx"`), svc, "allow[0].path", "never allowed"},
		{"encoded letter", replace(`path = "/api/"`, `path = "/%61pi"`), svc, "allow[0].path", "the character itself"},
		{"space in path", replace(`path = "/api/"`, `path = "/a b"`), svc, "allow[0].path", "not allowed in a URL path"},
		{"max body", withService(goodService + "[limits]\nmax_body = \"lots\"\n"), svc, "limits.max_body", "size"},
		{"timeout", withService(goodService + "[limits]\ntimeout = \"2h\"\n"), svc, "limits.timeout", "between 1s and 1h"},
		{"unknown key", withService(goodService + "[auth2]\nx = 1\n"), svc, "auth2", "unknown key"},
		{"missing file", func(t *testing.T, dir string) { write(t, dir, "realms/demo/services/wiki/login.sh", "x") }, svc, "", "missing"},
		{"bad name", func(t *testing.T, dir string) { write(t, dir, "realms/demo/services/Wiki/service.toml", goodService) }, "services/Wiki", "", "invalid service name"},
		{"long name", func(t *testing.T, dir string) {
			write(t, dir, "realms/demo/services/"+strings.Repeat("a", 60)+"/service.toml", goodService)
		}, strings.Repeat("a", 60), "", "longer than"},
		{"cookie without login", cookie(`login = ["bash", "login.sh"]`, ``), svc, "auth.login", "name the command"},
		{"cookie with header", cookie(`kind = "cookie"`, "kind = \"cookie\"\nheader = \"X\""), svc, "auth.header", "only to kind = \"header\""},
		{"cookie refresh", cookie(`auto = true`, "auto = true\nrefresh = \"10s\""), svc, "auth.refresh", "between 1m"},
		{"cookie image", cookie(`auto = true`, "auto = true\nimage = \"--privileged\""), svc, "auth.image", "image reference"},
		{"cookie login path", cookie(`login_path = "/login"`, `login_path = "login"`), svc, "auth.login_path", "must start with /"},
		{"cookie jar as login secret", cookie(`"INTRANET_USER", `, `"_jar.other", `), svc, "auth.secrets[0]", "cookie jar"},
		{"cookie secret twice", cookie(`"INTRANET_USER", "INTRANET_PASSWORD"`, `"A", "A"`), svc, "auth.secrets[1]", "listed twice"},
		{"duplicate port", func(t *testing.T, dir string) {
			write(t, dir, svc, goodService)
			write(t, dir, "realms/demo/services/wiki2/service.toml", goodService)
		}, "services/wiki2/service.toml", "port", "also the port of service \"wiki\""},
		// Grants.
		{"unknown service", grant("[grants]\nservices = [\"nope\"]"), hello, "grants.services[0]", "no service \"nope\" in realm \"demo\""},
		{"service of another realm", func(t *testing.T, dir string) {
			write(t, dir, "realms/other/services/wiki/service.toml", goodService)
			appendTo(t, dir, hello, "[grants]\nservices = [\"wiki\"]")
		}, hello, "grants.services[0]", "no service \"wiki\" in realm \"demo\""},
		{"service granted twice", grant("[grants]\nservices = [\"wiki\", \"wiki\"]"), hello, "grants.services[1]", "listed twice"},
		{"service name", grant("[grants]\nservices = [\"../wiki\"]"), hello, "grants.services[0]", "invalid service name"},
		{"credential granted too", grant("[grants]\nservices = [\"wiki\"]\nsecrets = [\"WIKI_TOKEN\"]"), hello, "grants.services[0]", "would enter the box"},
		{"jar granted", grant("[grants]\nsecrets = [\"_jar.wiki\"]"), hello, "grants.secrets[0]", "cookie jar"},
		{"port clash", grant("[grants]\nservices = [\"wiki\"]\n[box]\nports = [8601]"), hello, "grants.services[0]", "box.ports"},
		{"broken granted service", func(t *testing.T, dir string) {
			write(t, dir, svc, "upstream = [\n")
			appendTo(t, dir, hello, "[grants]\nservices = [\"wiki\"]")
		}, hello, "grants.services[0]", "failed to load"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyExample(t)
			tc.mutate(t, dir)
			_, probs := load(t, dir)
			for _, p := range probs {
				if strings.HasSuffix(p.File, tc.file) && p.Key == tc.key && strings.Contains(p.Message, tc.msg) {
					return
				}
			}
			t.Errorf("no problem %s: %s: …%s…; got:", tc.file, tc.key, tc.msg)
			for _, p := range probs {
				t.Errorf("  %s", p)
			}
		})
	}
}

func TestCheckRulePath(t *testing.T) {
	for _, ok := range []string{"/", "/api/", "/api", "/a/b;c=d/", "/x%20y", "/~user/", "/a:b@c"} {
		if err := CheckRulePath(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "api", "/api/./x", "/api/..;/x", "/a//b", "/a%2fb", "/a%25", "/a%2a", "/a%7E", "/a\\b", "/é", "/a?b", "/a#b", "/a%3Bb", "/a%3A", "/a%40"} {
		if err := CheckRulePath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestExampleService: the worked example under examples/services loads
// without problems.
func TestExampleService(t *testing.T) {
	dir := copyExample(t)
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "services", "intranet", "service.toml"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "realms/demo/services/intranet/service.toml", string(b))
	appendTo(t, dir, "realms/demo/projects/hello/project.toml", "[grants]\nservices = [\"intranet\"]")
	c, probs := load(t, dir)
	if len(probs) != 0 {
		t.Fatalf("problems: %v", probs)
	}
	if s, ok := c.Service("demo", "intranet"); !ok || s.Auth.Kind != ServiceAuthCookie || len(s.Allow) != 2 {
		t.Errorf("example: %+v", s)
	}
}
