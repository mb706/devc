package depsrc

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mb706/devc/internal/extract"
)

// Base URLs of the registries, and the variables that override them (for
// tests and mirrors).
var bases = map[string]struct{ env, def, preset string }{
	EcoR:      {"DEVC_SRC_CRAN", "https://cloud.r-project.org", "cran"},
	EcoPyPI:   {"DEVC_SRC_PYPI", "https://pypi.org", "pypi"},
	EcoNPM:    {"DEVC_SRC_NPM", "https://registry.npmjs.org", "npm"},
	EcoGitHub: {"DEVC_SRC_GITHUB_API", "https://api.github.com", "github"},
}

// Limits of downloads: metadata documents and archives.
const (
	maxDocument = 64 << 20
	maxArchive  = 2 << 30
)

// Options configure Fetch.
type Options struct {
	// Root is the cache (/src in a box).
	Root string
	// Getenv reads base URL overrides and GitHub tokens.
	Getenv func(string) string
	// Client makes the requests (default: one with the environment's proxy
	// settings, which carry the host's proxy at level allowlist).
	Client *http.Client
	// Network is the box's network level; at "allowlist" failed downloads
	// name the preset that allows the registry.
	Network string
	// Out receives progress notes (nil: none).
	Out io.Writer
}

func (o Options) getenv(k string) string {
	if o.Getenv == nil {
		return ""
	}
	return o.Getenv(k)
}

func (o Options) base(eco string) string {
	b := bases[eco]
	if v := strings.TrimRight(o.getenv(b.env), "/"); v != "" {
		return v
	}
	return b.def
}

func (o Options) notef(format string, args ...any) {
	if o.Out != nil {
		fmt.Fprintf(o.Out, "devc src: "+format+"\n", args...)
	}
}

// Result describes a fetched (or cached) source tree.
type Result struct {
	Path string `json:"path"`
	Spec Spec   `json:"spec"`
	// Source is the archive's URL (empty when the cache answered).
	Source string `json:"source,omitempty"`
	Cached bool   `json:"cached"`
	// InstalledFrom is the metadata file that gave the version.
	InstalledFrom string `json:"installed_from,omitempty"`
	// Skipped counts archive entries the extractor left out.
	Skipped int `json:"skipped,omitempty"`
	// Note says how the version was chosen.
	Note string `json:"note,omitempty"`
}

// candidate is one place an archive may be downloaded from, with the
// checksum the registry publishes for it (algorithm and expected digest;
// empty when none).
type candidate struct {
	url       string
	algo, sum string
}

// Fetch puts the source of s into the cache and returns where it is.
func Fetch(ctx context.Context, s Spec, e Env, o Options) (*Result, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	if o.Root == "" || !filepath.IsAbs(o.Root) {
		return nil, errors.New("depsrc: the cache directory is not set")
	}
	res := &Result{}
	if s.Version == "" {
		in, err := Discover(ctx, s, e)
		if err != nil {
			return nil, err
		}
		switch {
		case in == nil:
			if s.Eco != EcoGitHub {
				res.Note = "not installed here; using the current release"
			}
		case in.GitHub != nil:
			res.InstalledFrom = in.Where
			res.Note = fmt.Sprintf("%s %s was installed from GitHub (%s at %s); fetching that commit", s.Name, in.Version, in.GitHub.Name, in.GitHub.Version[:12])
			s = *in.GitHub
		default:
			res.InstalledFrom = in.Where
			res.Note = "the installed version"
			s.Version = in.Version
		}
	}
	var cands []candidate
	var err error
	switch s.Eco {
	case EcoR:
		s.Version, cands, err = o.cran(ctx, s)
	case EcoPyPI:
		s.Version, cands, err = o.pypi(ctx, s)
	case EcoNPM:
		s.Version, cands, err = o.npm(ctx, s)
	case EcoGitHub:
		s.Version, cands, err = o.github(ctx, s)
	}
	if err != nil {
		return nil, o.hint(s.Eco, err)
	}
	if err := s.check(); err != nil || s.Version == "" {
		return nil, fmt.Errorf("the registry answered with an unusable version for %s: %v", s.Name, err)
	}
	res.Spec = s
	parent := filepath.Join(o.Root, s.Eco, filepath.FromSlash(s.Name))
	dir := filepath.Join(parent, s.Version)
	res.Path = dir
	if s.Subdir != "" {
		res.Path = filepath.Join(dir, filepath.FromSlash(s.Subdir))
	}
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		res.Cached = true
		return res, nil
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("creating the cache directory: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-"+s.Version+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "archive")
	var used string
	for i, c := range cands {
		err = o.download(ctx, s.Eco, c, archive)
		if err == nil {
			used = c.url
			break
		}
		if !errors.Is(err, ErrNotFound) || i == len(cands)-1 {
			return nil, o.hint(s.Eco, err)
		}
	}
	res.Source = used
	o.notef("downloaded %s", used)
	tree := filepath.Join(tmp, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		return nil, err
	}
	rep, err := extract.File(archive, tree, extract.Options{Limits: extract.SourceLimits})
	if err != nil {
		return nil, fmt.Errorf("unpacking %s: %w", used, err)
	}
	res.Skipped = rep.SkippedCount
	if rep.SkippedCount > 0 {
		o.notef("left out %d archive entries (links leaving the tree, special files or unsafe names)", rep.SkippedCount)
	}
	top := tree
	// Release archives hold one top directory (pkg/, package/,
	// name-version/, owner-repo-sha/); its content becomes the version.
	if des, err := os.ReadDir(tree); err == nil && len(des) == 1 && des[0].IsDir() {
		top = filepath.Join(tree, des[0].Name())
	}
	if err := os.Rename(top, dir); err != nil {
		if fi, serr := os.Lstat(dir); serr == nil && fi.IsDir() {
			res.Cached = true // fetched meanwhile by someone else
			return res, nil
		}
		return nil, fmt.Errorf("moving the source into the cache: %w", err)
	}
	return res, nil
}

// hint adds, at network level allowlist, which preset allows the registry.
func (o Options) hint(eco string, err error) error {
	if errors.Is(err, ErrNotFound) {
		return err
	}
	if o.Network == "none" {
		return fmt.Errorf("%w\nthis box has no network (level none), so devc src cannot download anything", err)
	}
	if o.Network != "allowlist" {
		return err
	}
	host := o.base(eco)
	if u, perr := url.Parse(host); perr == nil {
		host = u.Host
	}
	return fmt.Errorf("%w\nthis box reaches only the hosts on its allowlist: ask the user to add the preset %q to [network.allowlist] presets "+
		"(in project.toml or realm.toml) and restart the box, or run 'devc request net %s:443' and wait", err, bases[eco].preset, host)
}

// client returns the HTTP client, refusing redirects to plain http except
// to the configured base's own host (a local test server).
func (o Options) client(eco string) *http.Client {
	c := o.Client
	if c == nil {
		c = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 30 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute}}
	}
	cc := *c
	cc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return errors.New("too many redirects")
		}
		if !o.allowedURL(eco, r.URL) {
			return fmt.Errorf("refusing a redirect to %s", r.URL.Redacted())
		}
		return nil
	}
	return &cc
}

// allowedURL accepts https, and plain http only on the configured base's
// scheme and host.
func (o Options) allowedURL(eco string, u *url.URL) bool {
	if u.User != nil || u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	b, err := url.Parse(o.base(eco))
	return err == nil && u.Scheme == b.Scheme && u.Host == b.Host
}

// get fetches u into w (at most max bytes). A 404 is ErrNotFound.
func (o Options) get(ctx context.Context, eco, rawURL string, header http.Header, w io.Writer, max int64) error {
	u, err := url.Parse(rawURL)
	if err != nil || !o.allowedURL(eco, u) {
		return fmt.Errorf("refusing to download %q: only https URLs are fetched", rawURL)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("User-Agent", "devc-src")
	resp, err := o.client(eco).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("GET %s: %w", u.Redacted(), ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("GET %s: %s %s", u.Redacted(), resp.Status, strings.TrimSpace(printable(string(body))))
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err == nil && n > max {
		err = fmt.Errorf("GET %s: larger than %d bytes", u.Redacted(), max)
	}
	return err
}

func (o Options) getJSON(ctx context.Context, eco, u string, header http.Header, v any) error {
	var buf bytes.Buffer
	if err := o.get(ctx, eco, u, header, &buf, maxDocument); err != nil {
		return err
	}
	if err := json.Unmarshal(buf.Bytes(), v); err != nil {
		return fmt.Errorf("reading %s: %w", u, err)
	}
	return nil
}

// download fetches a candidate into file, verifying its checksum.
func (o Options) download(ctx context.Context, eco string, c candidate, file string) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	var h hash.Hash
	switch c.algo {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	case "sha1":
		h = sha1.New()
	case "md5":
		h = md5.New()
	}
	var w io.Writer = f
	if h != nil {
		w = io.MultiWriter(f, h)
	}
	err = o.get(ctx, eco, c.url, o.headers(eco), w, maxArchive)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if h != nil {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, c.sum) {
			return fmt.Errorf("the download of %s has %s %s, but the registry says %s: refusing it", c.url, c.algo, got, c.sum)
		}
	}
	return nil
}

// headers are sent to a registry: GitHub's API version, and the user's
// token (GH_TOKEN, GITHUB_TOKEN) only to GitHub itself.
func (o Options) headers(eco string) http.Header {
	h := http.Header{}
	if eco != EcoGitHub {
		return h
	}
	h.Set("Accept", "application/vnd.github+json")
	h.Set("X-GitHub-Api-Version", "2022-11-28")
	if o.base(eco) == bases[EcoGitHub].def {
		for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
			if t := o.getenv(k); t != "" && !strings.ContainsAny(t, " \r\n") {
				h.Set("Authorization", "Bearer "+t)
				break
			}
		}
	}
	return h
}

// cran: the current release (with PACKAGES' MD5) or a named version, which
// is in src/contrib while current and in the archive afterwards.
func (o Options) cran(ctx context.Context, s Spec) (string, []candidate, error) {
	base := o.base(EcoR)
	cur := ""
	var md5sum string
	var buf bytes.Buffer
	if err := o.get(ctx, EcoR, base+"/src/contrib/PACKAGES", nil, &buf, maxDocument); err == nil {
		cur, md5sum = cranCurrent(buf.Bytes(), s.Name)
	} else if s.Version == "" {
		return "", nil, fmt.Errorf("reading CRAN's package list: %w", err)
	}
	v := s.Version
	if v == "" {
		if cur == "" {
			return "", nil, fmt.Errorf("CRAN has no package %s", s.Name)
		}
		v = cur
	}
	if CheckVersion(v) != nil {
		return "", nil, fmt.Errorf("CRAN lists an unusable version %q for %s", v, s.Name)
	}
	file := url.PathEscape(s.Name + "_" + v + ".tar.gz")
	current := candidate{url: base + "/src/contrib/" + file}
	if v == cur && md5sum != "" {
		current.algo, current.sum = "md5", md5sum
	}
	archive := candidate{url: base + "/src/contrib/Archive/" + url.PathEscape(s.Name) + "/" + file}
	return v, []candidate{current, archive}, nil
}

// cranCurrent finds a package's stanza in PACKAGES.
func cranCurrent(b []byte, name string) (version, md5sum string) {
	for _, stanza := range bytes.Split(b, []byte("\n\n")) {
		d := ParseDCF(stanza)
		if d["Package"] == name {
			return d["Version"], d["MD5sum"]
		}
	}
	return "", ""
}

// pypi: the source distribution of a release, from the JSON API.
func (o Options) pypi(ctx context.Context, s Spec) (string, []candidate, error) {
	base := o.base(EcoPyPI)
	u := base + "/pypi/" + url.PathEscape(s.Name) + "/json"
	if s.Version != "" {
		u = base + "/pypi/" + url.PathEscape(s.Name) + "/" + url.PathEscape(s.Version) + "/json"
	}
	var doc struct {
		Info struct {
			Version     string            `json:"version"`
			ProjectURLs map[string]string `json:"project_urls"`
		} `json:"info"`
		URLs []struct {
			PackageType string            `json:"packagetype"`
			URL         string            `json:"url"`
			Digests     map[string]string `json:"digests"`
		} `json:"urls"`
	}
	if err := o.getJSON(ctx, EcoPyPI, u, nil, &doc); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil, fmt.Errorf("PyPI has no %s: %w", s, err)
		}
		return "", nil, err
	}
	v := doc.Info.Version
	if CheckVersion(v) != nil {
		return "", nil, fmt.Errorf("PyPI answered with an unusable version %q", v)
	}
	for _, f := range doc.URLs {
		if f.PackageType == "sdist" {
			c := candidate{url: f.URL}
			if sum := f.Digests["sha256"]; len(sum) == 64 {
				c.algo, c.sum = "sha256", sum
			}
			return v, []candidate{c}, nil
		}
	}
	msg := fmt.Sprintf("%s %s has no source distribution on PyPI (only wheels)", s.Name, v)
	for _, k := range []string{"Source", "Source Code", "Repository", "Homepage"} {
		if l := doc.Info.ProjectURLs[k]; strings.HasPrefix(l, "https://github.com/") {
			msg += "; its repository is " + printable(l) + ": try github:<owner>/<repo>@<tag>"
			break
		}
	}
	return "", nil, errors.New(msg)
}

// npm: the registry's tarball of a version (latest when none), verified
// with its integrity string.
func (o Options) npm(ctx context.Context, s Spec) (string, []candidate, error) {
	name := url.PathEscape(s.Name) // a scope's "/" becomes %2F
	v := s.Version
	if v == "" {
		v = "latest"
	}
	var doc struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dist    struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
			Shasum    string `json:"shasum"`
		} `json:"dist"`
	}
	if err := o.getJSON(ctx, EcoNPM, o.base(EcoNPM)+"/"+name+"/"+url.PathEscape(v), nil, &doc); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil, fmt.Errorf("the npm registry has no %s: %w", s, err)
		}
		return "", nil, err
	}
	if doc.Name != s.Name || CheckVersion(doc.Version) != nil {
		return "", nil, fmt.Errorf("the npm registry answered for %q %q instead of %s", printable(doc.Name), printable(doc.Version), s.Name)
	}
	c := candidate{url: doc.Dist.Tarball}
	if algo, b64, ok := strings.Cut(doc.Dist.Integrity, "-"); ok && algo == "sha512" {
		if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
			c.algo, c.sum = "sha512", hex.EncodeToString(raw)
		}
	}
	if c.algo == "" && len(doc.Dist.Shasum) == 40 {
		c.algo, c.sum = "sha1", doc.Dist.Shasum
	}
	return doc.Version, []candidate{c}, nil
}

// github: the commit a ref names (the default branch when none), then the
// tarball of that commit.
func (o Options) github(ctx context.Context, s Spec) (string, []candidate, error) {
	base := o.base(EcoGitHub)
	ref := s.Version
	if ref == "" {
		ref = "HEAD"
	}
	sha := ref
	if !shaRE.MatchString(ref) {
		h := o.headers(EcoGitHub)
		h.Set("Accept", "application/vnd.github.sha")
		var buf bytes.Buffer
		if err := o.get(ctx, EcoGitHub, base+"/repos/"+s.Name+"/commits/"+pathSegments(ref), h, &buf, 4096); err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", nil, fmt.Errorf("GitHub has no %s (or it is private): %w", s, err)
			}
			return "", nil, err
		}
		sha = strings.TrimSpace(buf.String())
		if !shaRE.MatchString(sha) {
			return "", nil, fmt.Errorf("GitHub answered %q for the commit of %s", printable(proto64(sha)), s)
		}
		o.notef("%s is commit %s", s, sha)
	}
	return sha, []candidate{{url: base + "/repos/" + s.Name + "/tarball/" + sha}}, nil
}

func proto64(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// printable replaces control characters in text from a registry.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}
