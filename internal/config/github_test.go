package config

import (
	"os"
	"path/filepath"
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
