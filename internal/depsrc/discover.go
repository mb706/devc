// This file finds the installed version of a package in the box: R's
// DESCRIPTION files, Python's distribution metadata, npm's package.json.

package depsrc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Env is what discovery reads from the box: the environment, the working
// directory and the commands it may ask (R and Python know their own
// library paths best). Tests replace all of it.
type Env struct {
	Getenv func(string) string
	Getwd  func() (string, error)
	// Command runs a program and returns its standard output; nil, or an
	// error, means it is not available.
	Command func(ctx context.Context, name string, args ...string) ([]byte, error)
	// DefaultRLibs are R's usual library directories (DefaultRLibs when
	// nil).
	DefaultRLibs []string
}

// DefaultRLibs are the library directories of R on Debian, Ubuntu and
// Rocker images.
var DefaultRLibs = []string{"/usr/local/lib/R/site-library", "/usr/lib/R/site-library", "/usr/lib/R/library", "/usr/local/lib/R/library"}

// ExecCommand runs a program found in PATH with a short timeout.
func ExecCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = io.Discard
	return cmd.Output()
}

// Installed is what discovery found about an installed package.
type Installed struct {
	Version string
	// Where is the metadata file that named it.
	Where string
	// GitHub is set for an R package installed from GitHub: fetch that
	// commit instead of CRAN.
	GitHub *Spec
}

// Discover finds the installed version of s's package; nil when it is not
// installed (or the ecosystem has no notion of installed, GitHub).
func Discover(ctx context.Context, s Spec, e Env) (*Installed, error) {
	switch s.Eco {
	case EcoR:
		return discoverR(ctx, s.Name, e)
	case EcoPyPI:
		return discoverPython(ctx, s.Name, e)
	case EcoNPM:
		return discoverNPM(s.Name, e)
	}
	return nil, nil
}

func (e Env) getenv(k string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(k)
}

func (e Env) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	if e.Command == nil {
		return nil, errors.New("no commands")
	}
	return e.Command(ctx, name, args...)
}

// splitList splits a PATH-like variable, keeping absolute entries.
func splitList(v string) []string {
	var out []string
	for _, p := range filepath.SplitList(v) {
		if filepath.IsAbs(p) && !strings.Contains(p, "%") {
			out = append(out, filepath.Clean(p))
		}
	}
	return out
}

// RLibPaths lists the R library directories to search, in R's order: the
// environment's R_LIBS, R_LIBS_USER and R_LIBS_SITE, what R itself reports
// (.libPaths(), with the project's own setup such as renv), the user
// library below ~/R, and R's usual directories.
func RLibPaths(ctx context.Context, e Env) []string {
	var out []string
	add := func(ps ...string) {
		for _, p := range ps {
			if p != "" && !contains(out, p) {
				out = append(out, p)
			}
		}
	}
	for _, k := range []string{"R_LIBS", "R_LIBS_USER", "R_LIBS_SITE"} {
		add(splitList(e.getenv(k))...)
	}
	if b, err := e.command(ctx, "Rscript", "-e", `cat(.libPaths(), sep = "\n")`); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); filepath.IsAbs(l) {
				add(filepath.Clean(l))
			}
		}
	}
	if home := e.getenv("HOME"); filepath.IsAbs(home) {
		ms, _ := filepath.Glob(filepath.Join(home, "R", "*-library", "*"))
		add(ms...)
	}
	libs := e.DefaultRLibs
	if libs == nil {
		libs = DefaultRLibs
	}
	add(libs...)
	return out
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// maxMetadata bounds a metadata file read during discovery.
const maxMetadata = 1 << 20

func readSmall(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxMetadata))
}

// maxDCFLine bounds one line of a Debian control file.
const maxDCFLine = 1 << 20

// ParseDCFAll reads a whole Debian control file (R's DESCRIPTION, CRAN's
// PACKAGES) strictly: LF or CRLF line endings, stanzas separated by blank
// lines, continuation lines joined to their field. A line longer than
// maxDCFLine, a line that is neither a field nor a continuation, a
// continuation without a field, a field without a name or one given twice
// in a stanza is an error, never a partial result (finding E2-03).
func ParseDCFAll(b []byte) ([]map[string]string, error) {
	var out []map[string]string
	var cur map[string]string
	var key string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxDCFLine)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text() // ScanLines drops the CR of a CRLF
		switch {
		case strings.TrimSpace(line) == "":
			if cur != nil {
				out = append(out, cur)
			}
			cur, key = nil, ""
		case line[0] == ' ' || line[0] == '\t':
			if key == "" {
				return nil, fmt.Errorf("line %d continues no field", n)
			}
			cur[key] += " " + strings.TrimSpace(line)
		default:
			k, v, ok := strings.Cut(line, ":")
			k = strings.TrimSpace(k)
			if !ok || k == "" || strings.ContainsAny(k, " \t") {
				return nil, fmt.Errorf("line %d is not a field", n)
			}
			if cur == nil {
				cur = map[string]string{}
			}
			if _, dup := cur[k]; dup {
				return nil, fmt.Errorf("line %d repeats the field %s", n, printable(proto64(k)))
			}
			key = k
			cur[key] = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("after line %d: %w", n, err)
	}
	if cur != nil {
		out = append(out, cur)
	}
	return out, nil
}

// ParseDCF reads a file of one stanza (R's DESCRIPTION) strictly.
func ParseDCF(b []byte) (map[string]string, error) {
	st, err := ParseDCFAll(b)
	switch {
	case err != nil:
		return nil, err
	case len(st) != 1:
		return nil, fmt.Errorf("%d stanzas where one belongs", len(st))
	}
	return st[0], nil
}

// discoverR takes the version from the first DESCRIPTION of the package in
// R's library paths, and the GitHub commit when it was installed from
// there.
func discoverR(ctx context.Context, name string, e Env) (*Installed, error) {
	for _, lib := range RLibPaths(ctx, e) {
		file := filepath.Join(lib, name, "DESCRIPTION")
		b, err := readSmall(file)
		if err != nil {
			continue
		}
		// A DESCRIPTION that cannot be read completely names no version:
		// a part of it could hide the GitHub remote it was installed from.
		if len(b) >= maxMetadata {
			return nil, fmt.Errorf("%s is larger than %d bytes", file, maxMetadata)
		}
		d, err := ParseDCF(b)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}
		if d["Package"] != name || CheckVersion(d["Version"]) != nil {
			continue
		}
		in := &Installed{Version: d["Version"], Where: file}
		in.GitHub = rGitHubRemote(d)
		return in, nil
	}
	return nil, nil
}

// rGitHubRemote returns the GitHub commit an R package was installed from
// (remotes, pak and devtools record it in DESCRIPTION), or nil.
func rGitHubRemote(d map[string]string) *Spec {
	var user, repo, sha, sub string
	switch {
	case strings.EqualFold(d["RemoteType"], "github"):
		if h := d["RemoteHost"]; h != "" && h != "api.github.com" && h != "https://api.github.com" {
			return nil // GitHub Enterprise: not supported, use CRAN
		}
		user, repo, sha, sub = d["RemoteUsername"], d["RemoteRepo"], d["RemoteSha"], d["RemoteSubdir"]
	case d["GithubRepo"] != "":
		user, repo, sha, sub = d["GithubUsername"], d["GithubRepo"], d["GithubSHA1"], d["GithubSubdir"]
	default:
		return nil
	}
	s := &Spec{Eco: EcoGitHub, Name: user + "/" + repo, Version: sha, Subdir: sub}
	if !shaRE.MatchString(sha) || s.check() != nil {
		return nil
	}
	return s
}

// pythonSitePaths lists the directories to search for installed
// distributions: the active virtual or conda environment, then the
// interpreter's own sys.path.
func pythonSitePaths(ctx context.Context, e Env) []string {
	var out []string
	add := func(ps ...string) {
		for _, p := range ps {
			if filepath.IsAbs(p) && !contains(out, filepath.Clean(p)) {
				out = append(out, filepath.Clean(p))
			}
		}
	}
	for _, k := range []string{"VIRTUAL_ENV", "CONDA_PREFIX"} {
		if pre := e.getenv(k); filepath.IsAbs(pre) {
			ms, _ := filepath.Glob(filepath.Join(pre, "lib", "python*", "site-packages"))
			add(ms...)
		}
	}
	for _, py := range []string{"python3", "python"} {
		b, err := e.command(ctx, py, "-c", "import json, sys; print(json.dumps(sys.path))")
		if err != nil {
			continue
		}
		var ps []string
		if json.Unmarshal(bytes.TrimSpace(b), &ps) == nil {
			add(ps...)
			break
		}
	}
	return out
}

// discoverPython takes the version from the first *.dist-info/METADATA or
// *.egg-info/PKG-INFO of the project (names compared as PEP 503 does).
func discoverPython(ctx context.Context, name string, e Env) (*Installed, error) {
	want := NormalizePyPI(name)
	for _, dir := range pythonSitePaths(ctx, e) {
		des, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, de := range des {
			n := de.Name()
			var meta string
			switch {
			case strings.HasSuffix(n, ".dist-info"):
				meta = "METADATA"
			case strings.HasSuffix(n, ".egg-info"):
				meta = "PKG-INFO"
			default:
				continue
			}
			// <name>-<version>.dist-info; the name part is a cheap filter,
			// the metadata decides.
			base := strings.TrimSuffix(strings.TrimSuffix(n, ".dist-info"), ".egg-info")
			i := strings.Index(base, "-")
			if i <= 0 || NormalizePyPI(base[:i]) != want {
				continue
			}
			file := filepath.Join(dir, n, meta)
			b, err := readSmall(file)
			if err != nil {
				continue
			}
			h := parseHeaders(b)
			if NormalizePyPI(h["Name"]) != want || CheckVersion(h["Version"]) != nil {
				continue
			}
			return &Installed{Version: h["Version"], Where: file}, nil
		}
	}
	return nil, nil
}

// parseHeaders reads the RFC 822 header block of a Python metadata file.
func parseHeaders(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(k, " ") {
			if _, seen := out[k]; !seen {
				out[k] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

// discoverNPM takes the version from the first node_modules/<name>/
// package.json in the working directory or one of its parents, then in
// NODE_PATH.
func discoverNPM(name string, e Env) (*Installed, error) {
	var dirs []string
	if e.Getwd != nil {
		if wd, err := e.Getwd(); err == nil && filepath.IsAbs(wd) {
			for d := filepath.Clean(wd); ; d = filepath.Dir(d) {
				dirs = append(dirs, filepath.Join(d, "node_modules"))
				if d == "/" {
					break
				}
			}
		}
	}
	dirs = append(dirs, splitList(e.getenv("NODE_PATH"))...)
	for _, d := range dirs {
		file := filepath.Join(d, filepath.FromSlash(name), "package.json")
		b, err := readSmall(file)
		if errors.Is(err, fs.ErrNotExist) || err != nil {
			continue
		}
		var pj struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if json.Unmarshal(b, &pj) != nil || pj.Name != name || CheckVersion(pj.Version) != nil {
			continue
		}
		return &Installed{Version: pj.Version, Where: file}, nil
	}
	return nil, nil
}
