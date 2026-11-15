package hubnode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/paths"
)

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://hub.example.org":         "https://hub.example.org",
		"https://hub.example.org/":        "https://hub.example.org",
		"https://hub.example.org:8443/x/": "https://hub.example.org:8443/x",
		"https://127.0.0.1:1":             "https://127.0.0.1:1",
		"http://hub.example.org":          "",
		"https://user@hub.example.org":    "",
		"https://hub.example.org/?a=1":    "",
		"https://hub.example.org/#x":      "",
		"hub.example.org":                 "",
		"https:hub":                       "",
		"https://hub\".org":               "",
	} {
		got, err := NormalizeURL(in)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	if MachineURL("https://h:1/p") != "wss://h:1/p/machine" {
		t.Error(MachineURL("https://h:1/p"))
	}
	if len(ID("https://h")) != 16 || ID("https://h") == ID("https://i") {
		t.Error("ID")
	}
}

func TestCredentials(t *testing.T) {
	base := t.TempDir()
	env := map[string]string{paths.EnvConfigDir: filepath.Join(base, "c"), paths.EnvStateDir: filepath.Join(base, "s")}
	p, err := paths.Resolve(paths.Options{Home: base, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	hdir := filepath.Join(base, "hub")
	if err := hub.Init(hdir, ""); err != nil {
		t.Fatal(err)
	}
	ca, _ := hub.LoadCA(hub.Layout{Dir: hdir})
	is, _ := ca.IssueClient(hub.RoleMachine, "m1", false)
	other, _ := ca.IssueClient(hub.RoleMachine, "m2", false)
	caPEM, _ := os.ReadFile(hub.Layout{Dir: hdir}.CACert())
	u := "https://hub.example.org"

	if _, err := Load(p, u); err == nil || !strings.Contains(err.Error(), "devc join") {
		t.Errorf("load before join: %v", err)
	}
	if _, err := Check(is.CertPEM, other.KeyPEM, nil); err == nil {
		t.Error("a mismatched key was accepted")
	}
	if err := Save(p, u, is.CertPEM, is.KeyPEM, caPEM); err != nil {
		t.Fatal(err)
	}
	dir := p.NodeHubDir(ID(u))
	for f, mode := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "key.pem"): 0o600} {
		if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", f, fi.Mode(), err)
		}
	}
	c, err := Load(p, u)
	if err != nil || c.Leaf.Subject.CommonName != "m1" || c.RootCAs == nil {
		t.Fatalf("load: %+v %v", c, err)
	}
	if cfg := c.TLSConfig(); len(cfg.Certificates) != 1 || cfg.RootCAs == nil {
		t.Error("TLS configuration")
	}
	s1 := Stamp(p, u)
	// Joining again without a CA replaces the credentials and drops the CA.
	if err := Save(p, u, other.CertPEM, other.KeyPEM, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := Load(p, u); c.Leaf.Subject.CommonName != "m2" || c.RootCAs != nil {
		t.Errorf("after rejoining: %+v", c)
	}
	if Stamp(p, u) == s1 {
		t.Error("the stamp did not change")
	}
	// Credentials of another URL are not taken for this one.
	os.Rename(dir, p.NodeHubDir(ID("https://other.example.org")))
	if _, err := Load(p, "https://other.example.org"); err == nil || !strings.Contains(err.Error(), "another hub") {
		t.Errorf("moved credentials: %v", err)
	}
	if had, err := Remove(p, "https://other.example.org"); !had || err != nil {
		t.Errorf("remove: %v %v", had, err)
	}
	if had, _ := Remove(p, u); had {
		t.Error("removed nothing twice")
	}
}

func TestConfigEdits(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	orig := "# my settings\nprojects_root = \"~/devc\" # where\n\n[box]\nmemory = \"8g\"\n\n[[hubs]]\nurl = \"https://a.example.org\" # first\n\n[secrets]\ndelivery = \"files\"\n"
	os.WriteFile(file, []byte(orig), 0o640)
	added, err := AddToConfig(file, "https://b.example.org")
	if err != nil || !added {
		t.Fatalf("add: %v %v", added, err)
	}
	if added, _ := AddToConfig(file, "https://b.example.org"); added {
		t.Error("added twice")
	}
	if added, _ := AddToConfig(file, "https://a.example.org"); added {
		t.Error("added a listed hub")
	}
	b, _ := os.ReadFile(file)
	if !strings.HasPrefix(string(b), orig) || !strings.HasSuffix(string(b), "\n[[hubs]]\nurl = \"https://b.example.org\"\n") {
		t.Errorf("after adding:\n%s", b)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode())
	}
	urls, _ := listURLs(b)
	if strings.Join(urls, " ") != "https://a.example.org https://b.example.org" {
		t.Errorf("urls %q", urls)
	}
	// Removing the first keeps the tables around it.
	if removed, err := RemoveFromConfig(file, "https://a.example.org"); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	b, _ = os.ReadFile(file)
	want := "# my settings\nprojects_root = \"~/devc\" # where\n\n[box]\nmemory = \"8g\"\n\n[secrets]\ndelivery = \"files\"\n\n[[hubs]]\nurl = \"https://b.example.org\"\n"
	if string(b) != want {
		t.Errorf("after removing:\n%s\nwant:\n%s", b, want)
	}
	if removed, _ := RemoveFromConfig(file, "https://a.example.org"); removed {
		t.Error("removed twice")
	}
	// An inline list cannot be edited line by line: refused.
	os.WriteFile(file, []byte("hubs = [{url = \"https://a.example.org\"}]\n"), 0o644)
	if _, err := RemoveFromConfig(file, "https://a.example.org"); err == nil || !strings.Contains(err.Error(), "yourself") {
		t.Errorf("inline list: %v", err)
	}
	// A broken file is not touched.
	os.WriteFile(file, []byte("projects_root = \n"), 0o644)
	if _, err := AddToConfig(file, "https://b.example.org"); err == nil {
		t.Error("added to a broken file")
	}
	// A missing file is created.
	missing := filepath.Join(t.TempDir(), "new", "config.toml")
	if added, err := AddToConfig(missing, "https://b.example.org"); err != nil || !added {
		t.Errorf("missing file: %v %v", added, err)
	}
}
