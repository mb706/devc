package depsrc

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/depsrc/depsrctest"
)

func TestParseSpec(t *testing.T) {
	for in, want := range map[string]Spec{
		"r:dplyr":                     {Eco: EcoR, Name: "dplyr"},
		"r:data.table@1.15.4":         {Eco: EcoR, Name: "data.table", Version: "1.15.4"},
		"pypi:My_Lib.Ext@2.0rc1":      {Eco: EcoPyPI, Name: "my-lib-ext", Version: "2.0rc1"},
		"pypi:pkg@1!2.0.post1":        {Eco: EcoPyPI, Name: "pkg", Version: "1!2.0.post1"},
		"npm:react":                   {Eco: EcoNPM, Name: "react"},
		"npm:@types/node@22.10.1":     {Eco: EcoNPM, Name: "@types/node", Version: "22.10.1"},
		"npm:@scope/x":                {Eco: EcoNPM, Name: "@scope/x"},
		"npm:left-pad@1.3.0-beta+b.1": {Eco: EcoNPM, Name: "left-pad", Version: "1.3.0-beta+b.1"},
		"github:owner/repo":           {Eco: EcoGitHub, Name: "owner/repo"},
		"github:o-1/r.x.git@v1.2/rc":  {Eco: EcoGitHub, Name: "o-1/r.x", Version: "v1.2/rc"},
	} {
		got, err := ParseSpec(in)
		if err != nil || got != want {
			t.Errorf("ParseSpec(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"dplyr", "cargo:serde", "r:", "r:1bad", "r:x@", "r:x@../../etc", "r:x@1/2", "pypi:-x", "npm:@scope",
		"npm:a b", "github:owner", "github:../x", "github:o/..", "github:o/r@../x", "github:o/r@-x", "github:o/r@a b", "pypi:x@1 2"} {
		if s, err := ParseSpec(bad); err == nil {
			t.Errorf("ParseSpec(%q) accepted: %+v", bad, s)
		}
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover(t *testing.T) {
	base := t.TempDir()
	ctx := context.Background()
	lib1, lib2 := filepath.Join(base, "lib1"), filepath.Join(base, "lib2")
	write(t, filepath.Join(lib2, "dplyr", "DESCRIPTION"), "Package: dplyr\nTitle: A Grammar\n  of Data Manipulation\nVersion: 1.1.4\n")
	write(t, filepath.Join(lib1, "ghpkg", "DESCRIPTION"), "Package: ghpkg\nVersion: 0.9.0.9000\nRemoteType: github\nRemoteHost: api.github.com\n"+
		"RemoteUsername: someone\nRemoteRepo: ghpkg\nRemoteSha: 0123456789abcdef0123456789abcdef01234567\nRemoteSubdir: pkg\n")
	write(t, filepath.Join(lib1, "oldgh", "DESCRIPTION"), "Package: oldgh\nVersion: 1.0\nGithubRepo: oldgh\nGithubUsername: u\nGithubSHA1: 89abcdef0123456789abcdef0123456789abcdef\n")
	write(t, filepath.Join(lib1, "ghe", "DESCRIPTION"), "Package: ghe\nVersion: 2.0\nRemoteType: github\nRemoteHost: github.corp.example/api/v3\n"+
		"RemoteUsername: u\nRemoteRepo: ghe\nRemoteSha: 0123456789abcdef0123456789abcdef01234567\n")
	write(t, filepath.Join(lib1, "liar", "DESCRIPTION"), "Package: other\nVersion: 1.0\n")
	venv := filepath.Join(base, "venv")
	sp := filepath.Join(venv, "lib", "python3.12", "site-packages")
	write(t, filepath.Join(sp, "my_lib-2.0.0.dist-info", "METADATA"), "Metadata-Version: 2.1\nName: My-Lib\nVersion: 2.0.0\n\nDescription")
	write(t, filepath.Join(sp, "old_thing-0.3.egg-info", "PKG-INFO"), "Name: old.thing\nVersion: 0.3\n")
	proj := filepath.Join(base, "proj", "sub")
	write(t, filepath.Join(base, "proj", "node_modules", "@scope", "thing", "package.json"), `{"name":"@scope/thing","version":"3.1.4"}`)
	write(t, filepath.Join(base, "proj", "node_modules", "fake", "package.json"), `{"name":"other","version":"1.0.0"}`)
	os.MkdirAll(proj, 0o755)
	env := map[string]string{"R_LIBS": lib1 + ":relative:" + lib2, "VIRTUAL_ENV": venv, "HOME": filepath.Join(base, "home")}
	e := Env{Getenv: func(k string) string { return env[k] }, Getwd: func() (string, error) { return proj, nil }, DefaultRLibs: []string{}}

	for _, c := range []struct {
		spec, version, gh string
	}{
		{"r:dplyr", "1.1.4", ""},
		{"r:ghpkg", "0.9.0.9000", "github:someone/ghpkg@0123456789abcdef0123456789abcdef01234567"},
		{"r:oldgh", "1.0", "github:u/oldgh@89abcdef0123456789abcdef0123456789abcdef"},
		{"r:ghe", "2.0", ""},
		{"pypi:my-lib", "2.0.0", ""},
		{"pypi:MY.LIB", "2.0.0", ""},
		{"pypi:old-thing", "0.3", ""},
		{"npm:@scope/thing", "3.1.4", ""},
	} {
		s, _ := ParseSpec(c.spec)
		in, err := Discover(ctx, s, e)
		if err != nil || in == nil || in.Version != c.version {
			t.Errorf("%s: %+v %v", c.spec, in, err)
			continue
		}
		if gh := ""; in.GitHub != nil {
			gh = in.GitHub.String()
			if gh != c.gh {
				t.Errorf("%s: GitHub %s, want %s", c.spec, gh, c.gh)
			}
		} else if c.gh != "" {
			t.Errorf("%s: no GitHub remote", c.spec)
		}
	}
	if in, _ := Discover(ctx, Spec{Eco: EcoR, Name: "ghpkg"}, e); in == nil || in.GitHub == nil || in.GitHub.Subdir != "pkg" {
		t.Errorf("subdir: %+v", in)
	}
	for _, spec := range []string{"r:liar", "r:missing", "pypi:missing", "npm:fake", "npm:missing"} {
		s, _ := ParseSpec(spec)
		if in, _ := Discover(ctx, s, e); in != nil {
			t.Errorf("%s found: %+v", spec, in)
		}
	}
	// R's and Python's own answers are used when they are there.
	e.Command = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		switch name {
		case "Rscript":
			return []byte(filepath.Join(base, "rlib") + "\n"), nil
		case "python3":
			return []byte(`["", "` + filepath.Join(base, "pylib") + `"]`), nil
		}
		return nil, errors.New("absent")
	}
	write(t, filepath.Join(base, "rlib", "fromr", "DESCRIPTION"), "Package: fromr\nVersion: 3.0\n")
	write(t, filepath.Join(base, "pylib", "frompy-1.0.dist-info", "METADATA"), "Name: frompy\nVersion: 1.0\n")
	env["VIRTUAL_ENV"] = ""
	if in, _ := Discover(ctx, Spec{Eco: EcoR, Name: "fromr"}, e); in == nil || in.Version != "3.0" {
		t.Errorf("Rscript library: %+v", in)
	}
	if in, _ := Discover(ctx, Spec{Eco: EcoPyPI, Name: "frompy"}, e); in == nil || in.Version != "1.0" {
		t.Errorf("python sys.path: %+v", in)
	}
}

func TestFetch(t *testing.T) {
	reg := depsrctest.New()
	srv := httptest.NewServer(reg)
	defer srv.Close()
	vars := map[string]string{}
	for _, kv := range depsrctest.Env(srv.URL) {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	root := t.TempDir()
	var notes bytes.Buffer
	o := Options{Root: root, Getenv: func(k string) string { return vars[k] }, Out: &notes}
	none := Env{Getenv: func(string) string { return "" }, DefaultRLibs: []string{}}
	ctx := context.Background()
	fetch := func(spec string, e Env) (*Result, error) {
		s, err := ParseSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		return Fetch(ctx, s, e, o)
	}
	for _, c := range []struct{ spec, path, file, eco string }{
		{"r:mypkg", "r/mypkg/1.2.0", "R/code.R", "cran"},
		{"r:mypkg@1.1.0", "r/mypkg/1.1.0", "DESCRIPTION", "cran"},
		{"pypi:my_lib", "pypi/my-lib/2.0.0", "my_lib/__init__.py", "pypi"},
		{"npm:@scope/thing", "npm/@scope/thing/3.1.4", "src/index.js", "npm"},
		{"npm:plain@1.0.0", "npm/plain/1.0.0", "package.json", "npm"},
		{"github:owner/repo@main", "github/owner/repo/" + depsrctest.Commit, "README.md", "github"},
	} {
		before := reg.Downloads(c.eco)
		res, err := fetch(c.spec, none)
		if err != nil {
			t.Errorf("%s: %v", c.spec, err)
			continue
		}
		if res.Path != filepath.Join(root, c.path) || res.Cached {
			t.Errorf("%s: %+v", c.spec, res)
		}
		if _, err := os.Stat(filepath.Join(res.Path, c.file)); err != nil {
			t.Errorf("%s: %v", c.spec, err)
		}
		// The second call is answered from the cache.
		res, err = fetch(c.spec, none)
		if err != nil || !res.Cached || reg.Downloads(c.eco) != before+1 {
			t.Errorf("%s again: %+v %v (downloads %d)", c.spec, res, err, reg.Downloads(c.eco)-before)
		}
	}
	if des, _ := os.ReadDir(filepath.Join(root, "r", "mypkg")); len(des) != 2 {
		t.Errorf("temporary directories left: %v", des)
	}

	// An R package installed from GitHub is fetched from that commit, and
	// the path points at its subdirectory.
	lib := t.TempDir()
	write(t, filepath.Join(lib, "ghpkg", "DESCRIPTION"), "Package: ghpkg\nVersion: 0.1\nRemoteType: github\nRemoteUsername: owner\nRemoteRepo: repo\n"+
		"RemoteSha: "+depsrctest.Commit+"\nRemoteSubdir: pkg\n")
	withLib := Env{Getenv: func(k string) string {
		if k == "R_LIBS" {
			return lib
		}
		return ""
	}, DefaultRLibs: []string{}}
	res, err := fetch("r:ghpkg", withLib)
	if err != nil || res.Path != filepath.Join(root, "github/owner/repo", depsrctest.Commit, "pkg") || !res.Cached || res.Spec.Eco != EcoGitHub {
		t.Errorf("R from GitHub: %+v %v", res, err)
	}

	// Refusals.
	if _, err := fetch("pypi:wheels-only", none); err == nil || !strings.Contains(err.Error(), "no source distribution") || !strings.Contains(err.Error(), "github.com/o/wheels-only") {
		t.Errorf("wheels only: %v", err)
	}
	if _, err := fetch("r:nosuch@1.0", none); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing CRAN version: %v", err)
	}
	if _, err := fetch("github:owner/repo@"+depsrctest.EvilCommit, none); err == nil || !strings.Contains(err.Error(), "refusing a redirect") {
		t.Errorf("redirect to plain http elsewhere: %v", err)
	}
	reg.WrongSum = true
	os.RemoveAll(filepath.Join(root, "pypi"))
	if _, err := fetch("pypi:my-lib", none); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("wrong checksum: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pypi", "my-lib", "2.0.0")); err == nil {
		t.Error("an unverified download was cached")
	}
	reg.WrongSum = false

	// A hostile archive: nothing lands outside the version directory.
	reg.Hostile = true
	res, err = fetch("npm:plain@1.0.0", none)
	if err != nil || !res.Cached {
		t.Errorf("cached before hostile: %+v %v", res, err)
	}
	os.RemoveAll(filepath.Join(root, "npm"))
	res, err = fetch("npm:plain@1.0.0", none)
	if err != nil || res.Skipped != 2 {
		t.Fatalf("hostile archive: %+v %v", res, err)
	}
	if _, err := os.Lstat(filepath.Join(res.Path, "escape")); err == nil {
		t.Error("the escaping link was kept")
	}
	for _, p := range []string{filepath.Join(root, "outside"), filepath.Join(root, "npm", "outside"), filepath.Join(root, "npm", "plain", "outside")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s exists", p)
		}
	}

	// At level allowlist a failed download names the preset.
	o.Network = "allowlist"
	vars["DEVC_SRC_NPM"] = "http://127.0.0.1:1"
	if _, err := fetch("npm:react@1.0.0", none); err == nil || !strings.Contains(err.Error(), `preset "npm"`) || !strings.Contains(err.Error(), "devc request net 127.0.0.1:1:443") {
		t.Errorf("allowlist hint: %v", err)
	}
}
