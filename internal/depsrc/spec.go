// Package depsrc is `devc src` (work order C16): it fetches the source of a
// dependency at the installed version into the realm's cache at /src and
// prints the path. It runs inside a box, with the box's own network access
// (at level allowlist through the host's proxy, which must allow the
// registry), and only downloads and unpacks: nothing fetched is ever built,
// installed, imported or executed.
//
// Sources: CRAN source tarballs (the current release, then the archive),
// the source distribution named by PyPI's JSON API, the npm registry
// tarball, and GitHub's tarball endpoint. Checksums the registries publish
// (PyPI's sha256, npm's integrity, CRAN's MD5 for current releases) are
// verified. Every archive is unpacked by internal/extract, the same
// extractor shares use, into a new directory that is renamed into place
// when complete, so the cache never holds a partial tree.
//
// Security rules: the cache is shared by the boxes of one realm and
// writable by them; that is acceptable within one trust domain, and the
// documentation says so. What a registry returns is hostile input: names
// and versions are validated before they become paths, download URLs must
// be https (or the configured base's own scheme and host, for tests), and
// sizes are capped.
package depsrc

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Ecosystems.
const (
	EcoR      = "r"
	EcoPyPI   = "pypi"
	EcoNPM    = "npm"
	EcoGitHub = "github"
)

// Spec is what to fetch: an ecosystem, a package (owner/repo for GitHub)
// and a version or ref ("" means the installed one, else the current
// release; for GitHub the default branch).
type Spec struct {
	Eco     string `json:"ecosystem"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Subdir is a directory inside a GitHub tree where an R package lives
	// (RemoteSubdir in its DESCRIPTION).
	Subdir string `json:"subdir,omitempty"`
}

func (s Spec) String() string {
	if s.Version == "" {
		return s.Eco + ":" + s.Name
	}
	return s.Eco + ":" + s.Name + "@" + s.Version
}

var (
	rNameRE      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]*[A-Za-z0-9]$`)
	pypiNameRE   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	npmNameRE    = regexp.MustCompile(`^(@[A-Za-z0-9][A-Za-z0-9._~-]*/)?[A-Za-z0-9][A-Za-z0-9._~-]*$`)
	ghOwnerRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	ghRepoRE     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	versionRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+!~-]{0,127}$`)
	ghRefRE      = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,200}$`)
	shaRE        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pypiNormRE   = regexp.MustCompile(`[-_.]+`)
	usageExample = "r:<package>[@version], pypi:<package>[@version], npm:<package>[@version] or github:<owner>/<repo>[@ref]"
)

// ParseSpec reads a spec such as r:dplyr, pypi:requests@2.32.3,
// npm:@types/node@22.10.1 or github:owner/repo@v1.2.
func ParseSpec(s string) (Spec, error) {
	eco, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return Spec{}, fmt.Errorf("%q is not a dependency spec: use %s", s, usageExample)
	}
	sp := Spec{Eco: eco}
	hasVersion := false
	switch eco {
	case EcoNPM:
		// A scope starts with "@"; the version follows the second one.
		if at := strings.LastIndex(rest, "@"); at > 0 {
			sp.Name, sp.Version, hasVersion = rest[:at], rest[at+1:], true
		} else {
			sp.Name = rest
		}
	case EcoR, EcoPyPI, EcoGitHub:
		sp.Name, sp.Version, hasVersion = strings.Cut(rest, "@")
	default:
		return Spec{}, fmt.Errorf("unknown ecosystem %q: use %s", eco, usageExample)
	}
	if hasVersion && sp.Version == "" {
		return Spec{}, fmt.Errorf("%q: the version after @ is empty", s)
	}
	if err := sp.check(); err != nil {
		return Spec{}, err
	}
	if eco == EcoPyPI {
		sp.Name = NormalizePyPI(sp.Name)
	}
	if eco == EcoGitHub {
		sp.Name = strings.TrimSuffix(sp.Name, ".git")
	}
	return sp, nil
}

// check validates the name and version of a spec, wherever they came from
// (the command line, an installed package's metadata).
func (s Spec) check() error {
	switch s.Eco {
	case EcoR:
		if !rNameRE.MatchString(s.Name) {
			return fmt.Errorf("%q is not an R package name", s.Name)
		}
	case EcoPyPI:
		if !pypiNameRE.MatchString(s.Name) || len(s.Name) > 200 {
			return fmt.Errorf("%q is not a PyPI project name", s.Name)
		}
	case EcoNPM:
		if !npmNameRE.MatchString(s.Name) || len(s.Name) > 214 {
			return fmt.Errorf("%q is not an npm package name", s.Name)
		}
	case EcoGitHub:
		owner, repo, ok := strings.Cut(strings.TrimSuffix(s.Name, ".git"), "/")
		if !ok || !ghOwnerRE.MatchString(owner) || !ghRepoRE.MatchString(repo) || repo == "." || repo == ".." {
			return fmt.Errorf("%q is not a GitHub repository (owner/repo)", s.Name)
		}
		if s.Version != "" && (!ghRefRE.MatchString(s.Version) || strings.Contains(s.Version, "..") ||
			strings.HasPrefix(s.Version, "-") || strings.HasPrefix(s.Version, "/") || strings.HasSuffix(s.Version, "/")) {
			return fmt.Errorf("%q is not a git ref", s.Version)
		}
		if s.Subdir != "" && !cleanRel(s.Subdir) {
			return fmt.Errorf("%q is not a directory inside a repository", s.Subdir)
		}
		return nil
	default:
		return fmt.Errorf("unknown ecosystem %q", s.Eco)
	}
	if s.Version != "" {
		return CheckVersion(s.Version)
	}
	return nil
}

// CheckVersion validates a version that becomes a directory name.
func CheckVersion(v string) error {
	if !versionRE.MatchString(v) || strings.Contains(v, "..") {
		return fmt.Errorf("%q is not a version", v)
	}
	return nil
}

func cleanRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\\") || len(p) > 1024 {
		return false
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return true
}

// NormalizePyPI is a project name as PEP 503 compares it.
func NormalizePyPI(name string) string {
	return strings.ToLower(pypiNormRE.ReplaceAllString(name, "-"))
}

// pathSegments escapes each segment of a slash-separated ref for a URL.
func pathSegments(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// ErrNotFound means the registry has no such package or version.
var ErrNotFound = errors.New("not found")
