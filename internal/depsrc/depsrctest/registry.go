// Package depsrctest is a stand-in for CRAN, PyPI, the npm registry and
// GitHub's API, for the tests of devc src (unit and integration). It
// serves one package per ecosystem from archives built in memory and
// counts the archive downloads.
package depsrctest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Commit is the commit the fake GitHub resolves every ref to; the tarball
// of EvilCommit redirects to plain http on another host.
const (
	Commit     = "0123456789abcdef0123456789abcdef01234567"
	EvilCommit = "ffffffffffffffffffffffffffffffffffffffff"
)

// Registry is the fake. Mount it at a base URL B: the variables of
// Env(B) point devc src at it.
type Registry struct {
	mu        sync.Mutex
	downloads map[string]int
	// Hostile makes every archive carry a link out of the tree and a file
	// named with "..".
	Hostile bool
	// WrongSum makes PyPI publish a wrong checksum.
	WrongSum bool
}

// New returns an empty registry.
func New() *Registry { return &Registry{downloads: map[string]int{}} }

// Env are the variables that point devc src at the registry served at
// base (no trailing slash).
func Env(base string) []string {
	return []string{"DEVC_SRC_CRAN=" + base + "/cran", "DEVC_SRC_PYPI=" + base + "/pypi", "DEVC_SRC_NPM=" + base + "/npm",
		"DEVC_SRC_GITHUB_API=" + base + "/github"}
}

// Downloads counts the archive downloads of one ecosystem.
func (r *Registry) Downloads(eco string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.downloads[eco]
}

func (r *Registry) count(eco string) {
	r.mu.Lock()
	r.downloads[eco]++
	r.mu.Unlock()
}

// Tarball builds a gzip-compressed tar with the given files below top/.
func Tarball(top string, files map[string]string, hostile bool) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names) // the same bytes every time: checksums are published
	for _, name := range names {
		body := files[name]
		tw.WriteHeader(&tar.Header{Name: top + "/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	if hostile {
		tw.WriteHeader(&tar.Header{Name: top + "/escape", Typeflag: tar.TypeSymlink, Linkname: "../../../../etc/passwd", Mode: 0o777})
		tw.WriteHeader(&tar.Header{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
		tw.Write([]byte("x"))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// The packages: CRAN mypkg 1.2.0 (current) and 1.1.0 (archived), PyPI
// my-lib 2.0.0, npm @scope/thing 3.1.4 and plain 1.0.0, GitHub owner/repo.
func (r *Registry) archive(eco, version string) []byte {
	switch eco {
	case "cran":
		return Tarball("mypkg", map[string]string{"DESCRIPTION": "Package: mypkg\nVersion: " + version + "\n", "R/code.R": "f <- function() " + version + "\n"}, r.Hostile)
	case "pypi":
		return Tarball("my_lib-"+version, map[string]string{"setup.py": "raise SystemExit('never run')\n", "my_lib/__init__.py": "VERSION = '" + version + "'\n"}, r.Hostile)
	case "npm":
		return Tarball("package", map[string]string{"package.json": `{"name":"x","version":"` + version + `"}`, "src/index.js": "module.exports = 1\n"}, r.Hostile)
	case "github":
		return Tarball("owner-repo-0123456", map[string]string{"README.md": "repo at " + version + "\n", "pkg/DESCRIPTION": "Package: ghpkg\n"}, r.Hostile)
	}
	return nil
}

// ServeHTTP implements the four registries below /cran, /pypi, /npm and
// /github.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p := req.URL.EscapedPath()
	base := "http://" + req.Host
	writeJSON := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	switch {
	case p == "/cran/src/contrib/PACKAGES":
		a := r.archive("cran", "1.2.0")
		sum := md5.Sum(a)
		fmt.Fprintf(w, "Package: other\nVersion: 0.1\n\nPackage: mypkg\nVersion: 1.2.0\nMD5sum: %s\n\n", hex.EncodeToString(sum[:]))
	case p == "/cran/src/contrib/mypkg_1.2.0.tar.gz":
		r.count("cran")
		w.Write(r.archive("cran", "1.2.0"))
	case p == "/cran/src/contrib/Archive/mypkg/mypkg_1.1.0.tar.gz":
		r.count("cran")
		w.Write(r.archive("cran", "1.1.0"))
	case p == "/pypi/pypi/my-lib/json" || p == "/pypi/pypi/my-lib/2.0.0/json":
		a := r.archive("pypi", "2.0.0")
		sum := sha256.Sum256(a)
		digest := hex.EncodeToString(sum[:])
		if r.WrongSum {
			digest = strings.Repeat("0", 64)
		}
		writeJSON(map[string]any{"info": map[string]any{"version": "2.0.0"}, "urls": []any{
			map[string]any{"packagetype": "bdist_wheel", "url": base + "/pypi/files/my_lib-2.0.0-py3-none-any.whl"},
			map[string]any{"packagetype": "sdist", "url": base + "/pypi/files/my_lib-2.0.0.tar.gz", "digests": map[string]string{"sha256": digest}},
		}})
	case p == "/pypi/pypi/wheels-only/json":
		writeJSON(map[string]any{"info": map[string]any{"version": "1.0", "project_urls": map[string]string{"Source": "https://github.com/o/wheels-only"}},
			"urls": []any{map[string]any{"packagetype": "bdist_wheel", "url": base + "/x.whl"}}})
	case p == "/pypi/files/my_lib-2.0.0.tar.gz":
		r.count("pypi")
		w.Write(r.archive("pypi", "2.0.0"))
	case p == "/npm/@scope%2Fthing/latest" || p == "/npm/@scope%2Fthing/3.1.4", p == "/npm/plain/1.0.0":
		name, v := "@scope/thing", "3.1.4"
		if strings.HasPrefix(p, "/npm/plain") {
			name, v = "plain", "1.0.0"
		}
		a := r.archive("npm", v)
		sum := sha512.Sum512(a)
		writeJSON(map[string]any{"name": name, "version": v, "dist": map[string]any{
			"tarball": base + "/npm/tarballs/" + v + ".tgz", "integrity": "sha512-" + base64.StdEncoding.EncodeToString(sum[:])}})
	case strings.HasPrefix(p, "/npm/tarballs/"):
		r.count("npm")
		w.Write(r.archive("npm", strings.TrimSuffix(strings.TrimPrefix(p, "/npm/tarballs/"), ".tgz")))
	case strings.HasPrefix(p, "/github/repos/owner/repo/commits/"):
		if req.Header.Get("Accept") != "application/vnd.github.sha" {
			http.Error(w, "wrong accept header", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, Commit)
	case p == "/github/repos/owner/repo/tarball/"+Commit:
		// GitHub redirects to codeload.
		http.Redirect(w, req, base+"/codeload/owner/repo/tar.gz/"+Commit, http.StatusFound)
	case p == "/codeload/owner/repo/tar.gz/"+Commit:
		r.count("github")
		w.Write(r.archive("github", Commit))
	case p == "/github/repos/owner/repo/tarball/"+EvilCommit:
		http.Redirect(w, req, "http://elsewhere.invalid/x", http.StatusFound)
	default:
		http.NotFound(w, req)
	}
}
