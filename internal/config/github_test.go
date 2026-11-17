package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/names"
)

// TestGitHubGrants: a valid App configuration resolves with the realm's App
// and the project's permissions; without a table the defaults apply.
func TestGitHubGrants(t *testing.T) {
	dir := copyExample(t)
	realm := filepath.Join(dir, "realms", "demo", "realm.toml")
	b, _ := os.ReadFile(realm)
	write(t, dir, "realms/demo/realm.toml", string(b)+"\n[github.app]\napp_id = 42\nkey_secret = \"app-key\"\n")
	hello := "realms/demo/projects/hello/project.toml"
	hb, _ := os.ReadFile(filepath.Join(dir, hello))
	write(t, dir, hello, string(hb)+"\n[grants]\ngithub = \"app\"\ngithub_read = [\"acme/lib\"]\n[grants.github_token]\npermissions = { contents = \"write\", pull_requests = \"write\" }\n")
	c, probs := load(t, dir)
	if len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}
	r, err := c.Resolved(names.ProjectRef{Realm: "demo", Project: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if r.GitHubApp == nil || r.GitHubApp.AppID != 42 || r.GitHubApp.KeySecret != "app-key" || !r.Grants.GitHubTokenMode() ||
		len(r.Grants.GitHubRead) != 1 || r.Grants.GitHubPermissions()["contents"] != "write" || len(r.Grants.GitHubPermissions()) != 2 {
		t.Errorf("resolved: %+v %+v %v", r.GitHubApp, r.Grants, r.Grants.GitHubPermissions())
	}
	a, _ := c.Resolved(names.ProjectRef{Realm: "demo", Project: "analysis"})
	if a.Grants.GitHubTokenMode() || a.Grants.GitHubTokenSecret() != "" || a.Grants.GitHubPermissions()["contents"] != "read" || a.Grants.GitHubRead == nil {
		t.Errorf("relay: %+v", a.Grants)
	}
	if g := (Grants{GitHub: "token:pat"}); !g.GitHubTokenMode() || g.GitHubTokenSecret() != "pat" {
		t.Errorf("token mode: %v %q", g.GitHubTokenMode(), g.GitHubTokenSecret())
	}
}

// TestGitHubSecretsNotGranted: a project may not receive the realm's
// GitHub credential or its App's private key as an ordinary secret,
// through grants.secrets or as a harness's credential; the problem names
// the key and says why, as for a service's credential (finding X10 1).
// Control: another secret is granted without a problem.
func TestGitHubSecretsNotGranted(t *testing.T) {
	hello := "realms/demo/projects/hello/project.toml"
	for _, tc := range []struct {
		name, realm, project, key, want string
	}{
		{"control", "", "secrets = [\"pat\", \"app-key-2\"]", "", ""},
		{"host credential", "", "secrets = [\"pat\", \"github-token\"]", "grants.secrets[1]", "the secret github-token holds the realm's GitHub credential, and the project also receives it (grants.secrets): the credential would enter the box"},
		{"app key", "[github.app]\napp_id = 42\nkey_secret = \"app-key\"\n", "secrets = [\"app-key\"]", "grants.secrets[0]", "the secret app-key holds the private key of the realm's GitHub App (github.app.key_secret), and the project also receives it (grants.secrets): the key would enter the box"},
		{"harness credential", "[harness.claude]\nsecret = \"github-token\"\n", "", "harnesses", "(as the credential of harness claude): the credential would enter the box"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyExample(t)
			realm := filepath.Join(dir, "realms", "demo", "realm.toml")
			b, _ := os.ReadFile(realm)
			if strings.HasPrefix(tc.realm, "[harness.claude]") {
				b = []byte(strings.Replace(string(b), "[harness.claude]               # per-harness overrides, keyed by harness id\nauth = \"\"                      # overrides the manifest's auth method\nsecret = \"\"                    # overrides the secret name\n", "", 1))
			}
			write(t, dir, "realms/demo/realm.toml", string(b)+"\n"+tc.realm)
			hb, _ := os.ReadFile(filepath.Join(dir, hello))
			write(t, dir, hello, string(hb)+"\n[grants]\n"+tc.project+"\n")
			_, probs := load(t, dir)
			var found []string
			for _, p := range probs {
				found = append(found, p.Key+": "+p.Message)
			}
			switch {
			case tc.want == "" && len(probs) > 0:
				t.Errorf("problems: %q", found)
			case tc.want != "" && len(probs) == 0:
				t.Errorf("no problem, want %s: %s", tc.key, tc.want)
			}
			// The realm's override reaches every project with the harness.
			for _, p := range probs {
				if tc.want != "" && (p.Key != tc.key || !strings.Contains(p.Message, tc.want)) {
					t.Errorf("problem %s: %s, want %s: %s", p.Key, p.Message, tc.key, tc.want)
				}
			}
		})
	}
}

// TestReservedSecrets: one guard (Config.checkBoxSecret, on
// Config.reservedSecret) refuses every way a project names a reserved
// secret for its box, grants.github = "token:<secret>" included, and a
// service's credential or a cookie jar for every project of the realm,
// granted the service or not; a service's definition may not use the
// GitHub credential, the App's key or a cookie jar (findings G4-02,
// G4-03). Controls: a token in a secret of its own and another secret
// granted beside a service; two services sharing a credential.
func TestReservedSecrets(t *testing.T) {
	const hello = "realms/demo/projects/hello/project.toml"
	const wiki = "realms/demo/services/wiki/service.toml"
	const app = "[github.app]\napp_id = 42\nkey_secret = \"app-key\"\n"
	wikiWith := func(secret string) string { return strings.Replace(goodService, `"WIKI_TOKEN"`, `"`+secret+`"`, 1) }
	shared := strings.Replace(goodService, "port = 8601", "port = 8603", 1)
	for _, tc := range []struct {
		name, realm, service, service2, project, file, key, want string
	}{
		{"control", app, goodService, shared, "github = \"token:gh_pat\"\nservices = [\"wiki\"]\nsecrets = [\"other\"]", "", "", ""},
		{"token host credential", "", "", "", `github = "token:github-token"`, hello, "grants.github",
			`the secret github-token holds the realm's GitHub credential, and the project also receives it (as its GitHub token, grants.github = "token:github-token"): the credential would enter the box`},
		{"token app key", app, "", "", `github = "token:app-key"`, hello, "grants.github", "holds the private key of the realm's GitHub App (github.app.key_secret), and the project also receives it (as its GitHub token"},
		{"token service credential", "", goodService, "", `github = "token:WIKI_TOKEN"`, hello, "grants.github", `holds the credential of service "wiki", and the project also receives it (as its GitHub token`},
		{"token cookie jar", "", "", "", `github = "token:_jar.wiki"`, hello, "grants.github", `holds the cookie jar of service "wiki"`},
		{"service credential not granted the service", "", goodService, "", `secrets = ["WIKI_TOKEN"]`, hello, "grants.secrets[0]", `holds the credential of service "wiki", and the project also receives it (grants.secrets)`},
		{"service credential granted with it", "", goodService, "", "services = [\"wiki\"]\nsecrets = [\"WIKI_TOKEN\"]", hello, "grants.secrets[0]", `holds the credential of service "wiki"`},
		{"service uses the host credential", "", wikiWith("github-token"), "", "", wiki, "auth.value_secret", "the secret github-token holds the realm's GitHub credential, not a credential of this service"},
		{"service uses the app key", app, wikiWith("app-key"), "", "", wiki, "auth.value_secret", "the private key of the realm's GitHub App"},
		{"service uses a cookie jar", "", wikiWith("_jar.intranet"), "", "", wiki, "auth.value_secret", `the cookie jar of service "intranet"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyExample(t)
			if tc.realm != "" {
				appendTo(t, dir, "realms/demo/realm.toml", tc.realm)
			}
			if tc.service != "" {
				write(t, dir, wiki, tc.service)
			}
			if tc.service2 != "" {
				write(t, dir, "realms/demo/services/wiki2/service.toml", tc.service2)
			}
			if tc.project != "" {
				appendTo(t, dir, hello, "[grants]\n"+tc.project)
			}
			_, probs := load(t, dir)
			if tc.want == "" {
				if len(probs) > 0 {
					t.Errorf("problems: %v", probs)
				}
				return
			}
			if len(probs) != 1 || !strings.HasSuffix(probs[0].File, tc.file) || probs[0].Key != tc.key || !strings.Contains(probs[0].Message, tc.want) {
				t.Errorf("problems %v, want only %s: %s: …%s…", probs, tc.file, tc.key, tc.want)
			}
		})
	}
}
