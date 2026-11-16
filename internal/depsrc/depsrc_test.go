package depsrc

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// TestFetchChecksums: a registry that normally publishes a checksum must
// publish a usable one, and the download must match it; CRAN's archived
// versions and GitHub's tarballs, which have none, still come (finding
// D2-13).
func TestFetchChecksums(t *testing.T) {
	reg := depsrctest.New()
	srv := httptest.NewServer(reg)
	defer srv.Close()
	vars := map[string]string{}
	for _, kv := range depsrctest.Env(srv.URL) {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	none := Env{Getenv: func(string) string { return "" }, DefaultRLibs: []string{}}
	fetch := func(spec string) (*Result, error) {
		t.Helper()
		s, err := ParseSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		return Fetch(context.Background(), s, none, Options{Root: t.TempDir(), Getenv: func(k string) string { return vars[k] }})
	}
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	sha256sum := func(a []byte) []byte { s := sha256.Sum256(a); return s[:] }
	sha1hex := func(a []byte) string { s := sha1.Sum(a); return hex.EncodeToString(s[:]) }
	for _, c := range []struct {
		name string
		dist func([]byte) (string, string)
		want string // "" means accepted
	}{
		{"sha256 integrity", func(a []byte) (string, string) { return "sha256-" + b64(sha256sum(a)), "" }, ""},
		{"several, the strongest wins", func(a []byte) (string, string) {
			return "sha1-" + b64(make([]byte, 20)) + " sha256-" + b64(sha256sum(a)), ""
		}, ""},
		{"SRI options", func(a []byte) (string, string) { return "sha256-" + b64(sha256sum(a)) + "?foo", "" }, ""},
		{"shasum only", func(a []byte) (string, string) { return "", sha1hex(a) }, ""},
		{"wrong sha256 integrity", func(a []byte) (string, string) { return "sha256-" + b64(make([]byte, 32)), "" }, "refusing"},
		{"unknown algorithm, no shasum", func(a []byte) (string, string) { return "md5-" + b64(make([]byte, 16)), "" }, "no checksum devc can verify"},
		{"unknown algorithm with a shasum", func(a []byte) (string, string) { return "sha3-" + b64(make([]byte, 32)), sha1hex(a) }, "no checksum devc can verify"},
		{"truncated digest", func(a []byte) (string, string) { return "sha512-" + b64(sha256sum(a)), "" }, "no checksum devc can verify"},
		{"bad base64", func(a []byte) (string, string) { return "sha512-!!!", "" }, "no checksum devc can verify"},
		{"nothing", func([]byte) (string, string) { return "", "" }, "neither an integrity value nor a SHA-1 shasum"},
		{"bad shasum", func([]byte) (string, string) { return "", "zz" }, "neither an integrity value nor a SHA-1 shasum"},
	} {
		reg.NPMDist = c.dist
		before := reg.Downloads("npm")
		res, err := fetch("npm:plain@1.0.0")
		switch {
		case c.want == "" && (err != nil || res.Cached):
			t.Errorf("npm %s: %+v %v", c.name, res, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("npm %s: %v, want …%s…", c.name, err, c.want)
		case c.want != "" && c.want != "refusing" && reg.Downloads("npm") != before:
			t.Errorf("npm %s: downloaded without a usable checksum", c.name)
		}
	}
	reg.NPMDist = nil
	for name, d := range map[string]map[string]string{
		"no digests":   {},
		"md5 only":     {"md5": strings.Repeat("a", 32)},
		"short sha256": {"sha256": strings.Repeat("a", 63)},
		"not hex":      {"sha256": strings.Repeat("g", 64)},
	} {
		reg.PyPIDigests = d
		before := reg.Downloads("pypi")
		if _, err := fetch("pypi:my-lib@2.0.0"); err == nil || !strings.Contains(err.Error(), "no usable sha256") || reg.Downloads("pypi") != before {
			t.Errorf("pypi %s: %v", name, err)
		}
	}
	reg.PyPIDigests = nil
	if _, err := fetch("pypi:my-lib@2.0.0"); err != nil {
		t.Errorf("pypi control: %v", err)
	}
	reg.CRANNoMD5 = true
	if _, err := fetch("r:mypkg@1.2.0"); err == nil || !strings.Contains(err.Error(), "no usable MD5sum") {
		t.Errorf("CRAN current release without MD5: %v", err)
	}
	// Archived versions have no checksum on CRAN: they still come.
	if _, err := fetch("r:mypkg@1.1.0"); err != nil {
		t.Errorf("CRAN archived version: %v", err)
	}
	reg.CRANNoMD5 = false
	if _, err := fetch("r:mypkg"); err != nil {
		t.Errorf("CRAN control: %v", err)
	}
}

// TestFetchCachedOffline: a concrete version in the cache is answered
// without any request, also when the registry cannot be reached; versions
// that need the registry to be resolved still fail (finding D2-14).
func TestFetchCachedOffline(t *testing.T) {
	reg := depsrctest.New()
	srv := httptest.NewServer(reg)
	vars := map[string]string{}
	for _, kv := range depsrctest.Env(srv.URL) {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	root := t.TempDir()
	none := Env{Getenv: func(string) string { return "" }, DefaultRLibs: []string{}}
	fetch := func(spec string) (*Result, error) {
		t.Helper()
		s, err := ParseSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		return Fetch(context.Background(), s, none, Options{Root: root, Getenv: func(k string) string { return vars[k] }})
	}
	specs := []string{"r:mypkg@1.2.0", "r:mypkg@1.1.0", "pypi:my-lib@2.0.0", "npm:plain@1.0.0", "github:owner/repo@" + depsrctest.Commit}
	for _, s := range append(specs, "npm:plain@v1.0.0") {
		if _, err := fetch(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// The registry is still up, but nothing asks it.
	n := reg.Requests()
	for _, s := range append(specs, "npm:plain@v1.0.0") {
		if res, err := fetch(s); err != nil || !res.Cached {
			t.Errorf("%s from the cache: %+v %v", s, res, err)
		}
	}
	if reg.Requests() != n {
		t.Errorf("%d requests for cached versions", reg.Requests()-n)
	}
	// The registry goes away: the same answers.
	srv.Close()
	for _, s := range specs {
		if res, err := fetch(s); err != nil || !res.Cached {
			t.Errorf("%s with the registry gone: %+v %v", s, res, err)
		}
	}
	// The alias the registry's normalisation left answers too.
	if res, err := fetch("npm:plain@v1.0.0"); err != nil || !res.Cached || res.Spec.Version != "1.0.0" || res.Path != filepath.Join(root, "npm", "plain", "1.0.0") {
		t.Errorf("normalised version: %+v %v", res, err)
	}
	// Control: what needs the registry (a branch, the latest release, a
	// version not cached, a dist-tag) fails without it.
	for _, s := range []string{"github:owner/repo@main", "npm:plain", "pypi:my-lib@1.0.0", "npm:plain@latest"} {
		if _, err := fetch(s); err == nil {
			t.Errorf("%s answered without the registry", s)
		}
	}
}

// TestFetchMovingNames: an npm dist-tag (or a range) is resolved with the
// registry every time and never stored as an alias, so a moved tag is
// followed; exact versions, also the "v" form the registry normalises,
// still come from the cache (finding E2-06).
func TestFetchMovingNames(t *testing.T) {
	reg := depsrctest.New()
	srv := httptest.NewServer(reg)
	defer srv.Close()
	vars := map[string]string{}
	for _, kv := range depsrctest.Env(srv.URL) {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	root := t.TempDir()
	none := Env{Getenv: func(string) string { return "" }, DefaultRLibs: []string{}}
	fetch := func(spec string) (*Result, error) {
		t.Helper()
		s, err := ParseSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		return Fetch(context.Background(), s, none, Options{Root: root, Getenv: func(k string) string { return vars[k] }})
	}
	res, err := fetch("npm:plain@latest")
	if err != nil || res.Spec.Version != "1.0.0" {
		t.Fatalf("latest: %+v %v", res, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "npm", "plain", ".alias-latest")); err == nil {
		t.Error("the dist-tag was stored as an alias")
	}
	// The tag moves: the next request follows it, online.
	reg.NPMLatest = "1.1.0"
	n := reg.Requests()
	res, err = fetch("npm:plain@latest")
	if err != nil || res.Spec.Version != "1.1.0" || res.Path != filepath.Join(root, "npm", "plain", "1.1.0") || reg.Requests() == n {
		t.Errorf("latest after the tag moved: %+v %v (%d requests)", res, err, reg.Requests()-n)
	}
	// Asked again with the tag where it is: the registry is asked, the
	// tree comes from the cache.
	n = reg.Requests()
	if res, err := fetch("npm:plain@latest"); err != nil || !res.Cached || res.Spec.Version != "1.1.0" || reg.Requests() == n {
		t.Errorf("latest again: %+v %v (%d requests)", res, err, reg.Requests()-n)
	}
	// Exact versions are answered from the cache without the registry.
	if _, err := fetch("npm:plain@v1.0.0"); err != nil {
		t.Fatal(err)
	}
	n = reg.Requests()
	for _, s := range []string{"npm:plain@1.0.0", "npm:plain@v1.0.0", "npm:plain@1.1.0"} {
		if res, err := fetch(s); err != nil || !res.Cached {
			t.Errorf("%s: %+v %v", s, res, err)
		}
	}
	if reg.Requests() != n {
		t.Errorf("%d requests for exact cached versions", reg.Requests()-n)
	}
	// An alias left by an older devc for a tag is not used.
	if err := os.WriteFile(filepath.Join(root, "npm", "plain", ".alias-next"), []byte("1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if res, err := fetch("npm:plain@next"); err == nil {
		t.Errorf("a stored alias answered for a dist-tag: %+v", res)
	}
	for v, exact := range map[string]bool{"1.0.0": true, "v1.2.3": true, "1.0.0-rc.1": true, "1.0.0+build.5": true, "2.0.0-alpha-1": true,
		"latest": false, "next": false, "1.0": false, "1": false, "1.x": false, "01.0.0": false, "1.0.0.0": false, "v": false} {
		if concrete(EcoNPM, v) != exact {
			t.Errorf("concrete(npm, %q) = %v", v, !exact)
		}
	}
}

// TestParseDCF: a control file is read completely or not at all (finding
// E2-03): CRLF and LF, continuation lines; an overlong line, a line that is
// no field, a continuation without a field and a repeated field are errors.
func TestParseDCF(t *testing.T) {
	for name, in := range map[string]string{
		"lf":   "Package: a\nVersion: 1.0\nDepends: R (>= 3),\n    b\n\nPackage: c\nVersion: 2.0\n",
		"crlf": "Package: a\r\nVersion: 1.0\r\nDepends: R (>= 3),\r\n    b\r\n\r\nPackage: c\r\nVersion: 2.0\r\n\r\n",
	} {
		st, err := ParseDCFAll([]byte(in))
		if err != nil || len(st) != 2 || st[0]["Depends"] != "R (>= 3), b" || st[1]["Package"] != "c" || st[1]["Version"] != "2.0" {
			t.Errorf("%s: %v %v", name, st, err)
		}
	}
	for name, in := range map[string]string{
		"overlong line":       "Package: a\nDescription: " + strings.Repeat("x", maxDCFLine+1) + "\nVersion: 1.0\n",
		"not a field":         "Package: a\nthis is no field\nVersion: 1.0\n",
		"name with a space":   "Package: a\nMD5 sum: 00\n",
		"no field name":       "Package: a\n: 1.0\n",
		"orphan continuation": "  Version: 1.0\nPackage: a\n",
		"repeated field":      "Package: a\nVersion: 1.0\nVersion: 2.0\n",
	} {
		if st, err := ParseDCFAll([]byte(in)); err == nil {
			t.Errorf("%s: accepted %v", name, st)
		}
	}
	if _, err := ParseDCF([]byte("Package: a\n\nPackage: b\n")); err == nil {
		t.Error("ParseDCF accepted two stanzas")
	}
	// An installed DESCRIPTION that cannot be read completely is an error,
	// not a version.
	lib := t.TempDir()
	write(t, filepath.Join(lib, "broken", "DESCRIPTION"), "Package: broken\nVersion: 1.0\nRemoteSha "+depsrctest.Commit+"\n")
	e := Env{Getenv: func(k string) string {
		if k == "R_LIBS" {
			return lib
		}
		return ""
	}, DefaultRLibs: []string{}}
	if in, err := Discover(context.Background(), Spec{Eco: EcoR, Name: "broken"}, e); err == nil {
		t.Errorf("a broken DESCRIPTION gave %+v", in)
	}
}

// TestFetchCRANIndex: the package list is parsed completely before any
// download; a list that cannot be read stops the download, a current
// release is verified with its MD5 whatever the line endings, and only an
// archived version comes without a checksum (finding E2-03).
func TestFetchCRANIndex(t *testing.T) {
	reg := depsrctest.New()
	srv := httptest.NewServer(reg)
	defer srv.Close()
	vars := map[string]string{}
	for _, kv := range depsrctest.Env(srv.URL) {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	none := Env{Getenv: func(string) string { return "" }, DefaultRLibs: []string{}}
	fetch := func(spec string) (*Result, error) {
		t.Helper()
		s, err := ParseSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		return Fetch(context.Background(), s, none, Options{Root: t.TempDir(), Getenv: func(k string) string { return vars[k] }})
	}
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	wrong := strings.Repeat("0", 32)
	for _, c := range []struct {
		name  string
		index func(md5 string) string
		spec  string
		want  string // "" means downloaded
	}{
		{"crlf, mypkg second", func(m string) string {
			return crlf("Package: other\nVersion: 0.1\n\nPackage: mypkg\nVersion: 1.2.0\nMD5sum: " + m + "\n\n")
		}, "r:mypkg@1.2.0", ""},
		{"crlf, wrong MD5", func(string) string {
			return crlf("Package: other\nVersion: 0.1\n\nPackage: mypkg\nVersion: 1.2.0\nMD5sum: " + wrong + "\n\n")
		}, "r:mypkg@1.2.0", "refusing it"},
		{"crlf, current without a version", func(m string) string {
			return crlf("Package: other\nVersion: 0.1\n\nPackage: mypkg\nVersion: 1.2.0\nMD5sum: " + m + "\n\n")
		}, "r:mypkg", ""},
		{"overlong line before mypkg", func(m string) string {
			return "Package: other\nDescription: " + strings.Repeat("x", maxDCFLine+1) + "\n\nPackage: mypkg\nVersion: 1.2.0\nMD5sum: " + m + "\n"
		}, "r:mypkg@1.2.0", "cannot be read completely"},
		{"overlong line inside mypkg", func(m string) string {
			return "Package: mypkg\nDescription: " + strings.Repeat("x", maxDCFLine+1) + "\nVersion: 1.2.0\nMD5sum: " + m + "\n"
		}, "r:mypkg@1.2.0", "cannot be read completely"},
		{"a line that is no field", func(m string) string {
			return "Package: other\nVersion 0.1\n\nPackage: mypkg\nVersion: 1.2.0\nMD5sum: " + m + "\n"
		}, "r:mypkg@1.2.0", "cannot be read completely"},
		{"mypkg twice", func(m string) string {
			return "Package: mypkg\nVersion: 1.1.0\n\nPackage: mypkg\nVersion: 1.2.0\nMD5sum: " + m + "\n"
		}, "r:mypkg@1.2.0", "twice"},
		{"a stanza without a package", func(m string) string { return "Version: 0.1\n\nPackage: mypkg\nVersion: 1.2.0\nMD5sum: " + m + "\n" }, "r:mypkg@1.2.0", "names no package"},
		// A version neither current nor archived is not taken from
		// src/contrib unchecked, although the stand-in serves it there.
		{"neither current nor archived", nil, "r:mypkg@1.3.0", "not found"},
		// Controls: the archive, and the default index.
		{"archived version", nil, "r:mypkg@1.1.0", ""},
		{"current release", nil, "r:mypkg@1.2.0", ""},
	} {
		reg.CRANIndex = c.index
		before := reg.Downloads("cran")
		res, err := fetch(c.spec)
		switch {
		case c.want == "" && (err != nil || res.Cached):
			t.Errorf("%s: %+v %v", c.name, res, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want …%s…", c.name, err, c.want)
		case c.want != "" && c.want != "refusing it" && reg.Downloads("cran") != before:
			t.Errorf("%s: downloaded although the index was refused", c.name)
		}
	}
}
