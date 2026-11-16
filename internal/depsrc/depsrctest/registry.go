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
	// PyPIDigests, when set, are the digests PyPI publishes for the sdist;
	// NPMDist, when set, gives the integrity and shasum npm publishes for
	// an archive; CRANNoMD5 leaves the MD5sum out of PACKAGES.
	PyPIDigests map[string]string
	NPMDist     func(archive []byte) (integrity, shasum string)
	CRANNoMD5   bool
	// CRANIndex, when set, is PACKAGES, given the MD5 of mypkg 1.2.0.
	CRANIndex func(md5 string) string
	// NPMLatest is the version the dist-tag latest of plain names (1.0.0
	// when empty); plain 1.0.0 and 1.1.0 exist.
	NPMLatest string
	// NPMTags are further dist-tags of plain: name → version.
	NPMTags map[string]string
	// CRANArchiveRedirect, when set, is where the archive of mypkg 1.1.0
	// redirects to (a path below the server; asked with a query, the
	// archive answers itself).
	CRANArchiveRedirect string
	// GitHubRedirect, when set, is where the tarball of Commit redirects
	// to (a path below the server; everything below /codeload/ serves the
	// tarball).
	GitHubRedirect string
	requests       int
}

// Requests counts every request the registry answered.
func (r *Registry) Requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
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
// my-lib 2.0.0, npm @scope/thing 3.1.4 and plain 1.0.0 and 1.1.0, GitHub
// owner/repo.
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
	r.mu.Lock()
	r.requests++
	pyDigests, npmDist, noMD5, cranIndex, npmLatest := r.PyPIDigests, r.NPMDist, r.CRANNoMD5, r.CRANIndex, r.NPMLatest
	npmTags, cranRedirect, ghRedirect := r.NPMTags, r.CRANArchiveRedirect, r.GitHubRedirect
	r.mu.Unlock()
	if npmLatest == "" {
		npmLatest = "1.0.0"
	}
	p := req.URL.EscapedPath()
	base := "http://" + req.Host
	writeJSON := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	switch {
	case p == "/cran/src/contrib/PACKAGES":
		a := r.archive("cran", "1.2.0")
		sum := md5.Sum(a)
		if cranIndex != nil {
			fmt.Fprint(w, cranIndex(hex.EncodeToString(sum[:])))
			return
		}
		if noMD5 {
			fmt.Fprint(w, "Package: other\nVersion: 0.1\n\nPackage: mypkg\nVersion: 1.2.0\n\n")
			return
		}
		fmt.Fprintf(w, "Package: other\nVersion: 0.1\n\nPackage: mypkg\nVersion: 1.2.0\nMD5sum: %s\n\n", hex.EncodeToString(sum[:]))
	case p == "/cran/src/contrib/mypkg_1.2.0.tar.gz":
		r.count("cran")
		w.Write(r.archive("cran", "1.2.0"))
	case p == "/cran/src/contrib/Archive/mypkg/mypkg_1.1.0.tar.gz" && cranRedirect != "" && req.URL.RawQuery == "":
		http.Redirect(w, req, base+cranRedirect, http.StatusFound)
	case p == "/cran/src/contrib/Archive/mypkg/mypkg_1.1.0.tar.gz":
		r.count("cran")
		w.Write(r.archive("cran", "1.1.0"))
	case p == "/cran/src/contrib/mypkg_1.3.0.tar.gz":
		// In src/contrib but neither the index's current version nor in
		// the archive: devc must not take it unchecked.
		r.count("cran")
		w.Write(r.archive("cran", "1.3.0"))
	case p == "/pypi/pypi/my-lib/json" || p == "/pypi/pypi/my-lib/2.0.0/json":
		a := r.archive("pypi", "2.0.0")
		sum := sha256.Sum256(a)
		digest := hex.EncodeToString(sum[:])
		if r.WrongSum {
			digest = strings.Repeat("0", 64)
		}
		digests := map[string]string{"sha256": digest}
		if pyDigests != nil {
			digests = pyDigests
		}
		writeJSON(map[string]any{"info": map[string]any{"version": "2.0.0"}, "urls": []any{
			map[string]any{"packagetype": "bdist_wheel", "url": base + "/pypi/files/my_lib-2.0.0-py3-none-any.whl"},
			map[string]any{"packagetype": "sdist", "url": base + "/pypi/files/my_lib-2.0.0.tar.gz", "digests": digests},
		}})
	case p == "/pypi/pypi/wheels-only/json":
		writeJSON(map[string]any{"info": map[string]any{"version": "1.0", "project_urls": map[string]string{"Source": "https://github.com/o/wheels-only"}},
			"urls": []any{map[string]any{"packagetype": "bdist_wheel", "url": base + "/x.whl"}}})
	case p == "/pypi/files/my_lib-2.0.0.tar.gz":
		r.count("pypi")
		w.Write(r.archive("pypi", "2.0.0"))
	case p == "/npm/@scope%2Fthing/latest" || p == "/npm/@scope%2Fthing/3.1.4", p == "/npm/plain/1.0.0", p == "/npm/plain/v1.0.0", p == "/npm/plain/1.0.0+build.5",
		p == "/npm/plain/latest", p == "/npm/plain/1.1.0", strings.HasPrefix(p, "/npm/plain/") && npmTags[strings.TrimPrefix(p, "/npm/plain/")] != "":
		// "v1.0.0" and "1.0.0+build.5" are normalised to 1.0.0, as
		// registries do; the dist-tag latest moves with NPMLatest, the
		// others with NPMTags.
		name, v := "@scope/thing", "3.1.4"
		switch p {
		case "/npm/plain/latest":
			name, v = "plain", npmLatest
		case "/npm/plain/1.1.0":
			name, v = "plain", "1.1.0"
		case "/npm/plain/1.0.0", "/npm/plain/v1.0.0", "/npm/plain/1.0.0+build.5":
			name, v = "plain", "1.0.0"
		default:
			if t := npmTags[strings.TrimPrefix(p, "/npm/plain/")]; t != "" {
				name, v = "plain", t
			}
		}
		a := r.archive("npm", v)
		sum := sha512.Sum512(a)
		dist := map[string]any{"tarball": base + "/npm/tarballs/" + v + ".tgz", "integrity": "sha512-" + base64.StdEncoding.EncodeToString(sum[:])}
		if npmDist != nil {
			integrity, shasum := npmDist(a)
			dist = map[string]any{"tarball": base + "/npm/tarballs/" + v + ".tgz"}
			if integrity != "" {
				dist["integrity"] = integrity
			}
			if shasum != "" {
				dist["shasum"] = shasum
			}
		}
		writeJSON(map[string]any{"name": name, "version": v, "dist": dist})
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
		to := "/codeload/owner/repo/tar.gz/" + Commit
		if ghRedirect != "" {
			to = ghRedirect
		}
		http.Redirect(w, req, base+to, http.StatusFound)
	case strings.HasPrefix(p, "/codeload/"):
		r.count("github")
		w.Write(r.archive("github", Commit))
	case p == "/github/repos/owner/repo/tarball/"+EvilCommit:
		http.Redirect(w, req, "http://elsewhere.invalid/x", http.StatusFound)
	default:
		http.NotFound(w, req)
	}
}
