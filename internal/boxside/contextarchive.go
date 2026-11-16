package boxside

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/safefs"
)

// The context repository in the archive (work order C17): the context
// worker makes bundles of it for the hubs and exports, and restores it
// from bundles. Both run in the context container like every other git
// operation on it (ctxrepo); the host hands files in and out through a
// directory of its own mounted at ContextIOMount.

// ContextIOMount is where the context container sees the host's exchange
// directory for bundles.
const ContextIOMount = "/io"

// Names in the exchange directory.
const (
	ContextBundleFile   = "context.bundle"
	ContextExcludesFile = "excludes"
	ContextRefsFile     = "refs.json"
)

var (
	ctxOIDRE     = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	ctxRefRE     = regexp.MustCompile(`^refs/(heads|tags)/[A-Za-z0-9._/-]{1,200}$`)
	ctxBundleRE  = regexp.MustCompile(`^[0-9]{8}\.bundle$`)
	restoreSpace = "refs/devc/restore/"
)

// ValidContextRef accepts the refs the archive carries for the context
// repository: branches and tags with plain names.
func ValidContextRef(r string) bool {
	return ctxRefRE.MatchString(r) && !strings.Contains(r, "..") && !strings.Contains(r, "//") && !strings.HasSuffix(r, "/") && !strings.HasSuffix(r, ".lock")
}

// ValidOID reports whether s is a full object name.
func ValidOID(s string) bool { return ctxOIDRE.MatchString(s) }

// ContextBundleResult is what Bundle prints: the refs of the repository
// now, and whether a bundle of the objects not reachable from the
// excluded commits was written.
type ContextBundleResult struct {
	Refs   map[string]string `json:"refs"`
	Bundle bool              `json:"bundle"`
}

// ContextRestoreResult is what Restore prints.
type ContextRestoreResult struct {
	Created   []string `json:"created"`
	Conflicts []string `json:"conflicts"`
	Files     int      `json:"files"`
	Skipped   []string `json:"skipped"`
}

// refs lists the branches and tags.
func (w *ContextWorker) refs() (map[string]string, error) {
	out, err := w.gitOut("for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		oid, name, ok := strings.Cut(l, " ")
		if ok && ValidOID(oid) && ValidContextRef(name) {
			m[name] = oid
		}
	}
	return m, nil
}

func (w *ContextWorker) hasObject(oid string) bool {
	return w.git(nil, io.Discard, "cat-file", "-e", oid+"^{commit}") == nil
}

// Bundle writes the objects reachable from the branches and tags but not
// from the commits listed in io/excludes to io/context.bundle and prints
// a ContextBundleResult. With nothing new it writes no bundle.
func (w *ContextWorker) Bundle(ioDir string) error {
	if err := w.ensureRepo(); err != nil {
		return err
	}
	refs, err := w.refs()
	if err != nil {
		return err
	}
	res := ContextBundleResult{Refs: refs}
	var excl []string
	if b, err := os.ReadFile(filepath.Join(ioDir, ContextExcludesFile)); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); ValidOID(l) && w.hasObject(l) {
				excl = append(excl, l)
			}
		}
	}
	if len(refs) > 0 {
		var in bytes.Buffer
		names := make([]string, 0, len(refs))
		for r := range refs {
			names = append(names, r)
		}
		sort.Strings(names)
		for _, r := range names {
			in.WriteString(r + "\n")
		}
		for _, e := range excl {
			in.WriteString("^" + e + "\n")
		}
		out := filepath.Join(ioDir, ContextBundleFile)
		err := w.git(&in, io.Discard, "bundle", "create", "--quiet", out, "--stdin")
		switch {
		case err == nil:
			res.Bundle = true
		case strings.Contains(err.Error(), "empty bundle"):
			_ = os.Remove(out)
		default:
			return err
		}
	}
	return json.NewEncoder(w.Stdout).Encode(res)
}

// Restore fetches the bundles io/<seq>.bundle in order into the
// repository, then creates the refs of io/refs.json that are missing
// (existing refs with other values are reported, never moved). When it
// created main, it writes main's files that are missing from the working
// tree, through safefs (no symlink followed, nothing replaced).
func (w *ContextWorker) Restore(ioDir string) error {
	if err := w.ensureRepo(); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(ioDir, ContextRefsFile))
	if err != nil {
		return err
	}
	var want map[string]string
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&want); err != nil {
		return fmt.Errorf("invalid ref list: %w", err)
	}
	for r, oid := range want {
		if !ValidContextRef(r) || !ValidOID(oid) {
			return fmt.Errorf("invalid ref %q", r)
		}
	}
	des, err := os.ReadDir(ioDir)
	if err != nil {
		return err
	}
	var bundles []string
	for _, de := range des {
		if ctxBundleRE.MatchString(de.Name()) {
			bundles = append(bundles, de.Name())
		}
	}
	sort.Strings(bundles)
	for _, name := range bundles {
		if err := w.git(nil, io.Discard, "-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true",
			"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-gc", filepath.Join(ioDir, name), "+refs/*:"+restoreSpace+"*"); err != nil {
			return fmt.Errorf("fetching %s: %w", name, err)
		}
	}
	defer w.dropRestoreRefs()
	have, err := w.refs()
	if err != nil {
		return err
	}
	res := ContextRestoreResult{Created: []string{}, Conflicts: []string{}, Skipped: []string{}}
	names := make([]string, 0, len(want))
	for r := range want {
		names = append(names, r)
	}
	sort.Strings(names)
	createdMain := false
	for _, r := range names {
		oid := want[r]
		switch cur, ok := have[r]; {
		case ok && cur == oid:
		case ok:
			res.Conflicts = append(res.Conflicts, r)
		case w.git(nil, io.Discard, "cat-file", "-e", oid) != nil:
			res.Conflicts = append(res.Conflicts, r+" (objects missing from the bundles)")
		default:
			if err := w.git(nil, io.Discard, "update-ref", "-m", "devc state restore", r, oid, strings.Repeat("0", len(oid))); err != nil {
				return err
			}
			res.Created = append(res.Created, r)
			createdMain = createdMain || r == mainRef
		}
	}
	if createdMain {
		if err := w.checkoutMissing(&res); err != nil {
			return err
		}
	}
	return json.NewEncoder(w.Stdout).Encode(res)
}

func (w *ContextWorker) dropRestoreRefs() {
	out, err := w.gitOut("for-each-ref", "--format=%(refname)", restoreSpace)
	if err != nil {
		return
	}
	for _, r := range strings.Split(out, "\n") {
		if strings.HasPrefix(r, restoreSpace) {
			_ = w.git(nil, io.Discard, "update-ref", "-d", r)
		}
	}
}

// checkoutMissing writes the regular files of main's tree that do not
// exist in the working tree.
func (w *ContextWorker) checkoutMissing(res *ContextRestoreResult) error {
	var out bytes.Buffer
	if err := w.git(nil, &out, "ls-tree", "-r", "-z", "--full-tree", mainRef); err != nil {
		return err
	}
	root, err := safefs.OpenDir(w.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	entries := bytes.Split(out.Bytes(), []byte{0})
	if len(entries) > contextMaxFiles {
		return fmt.Errorf("the restored context has more than %d files", contextMaxFiles)
	}
	for _, e := range entries {
		meta, p, ok := strings.Cut(string(e), "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 || f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") || !ValidOID(f[2]) {
			res.Skipped = append(res.Skipped, p)
			continue
		}
		if !safeContextPath(p) {
			res.Skipped = append(res.Skipped, p)
			continue
		}
		dir, name := path.Split(p)
		d := root
		if dir != "" {
			sub, _, err := root.MkdirAll(strings.TrimSuffix(dir, "/"), 0o755)
			if err != nil {
				res.Skipped = append(res.Skipped, p)
				continue
			}
			d = sub
		}
		mode := uint32(0o644)
		if f[0] == "100755" {
			mode = 0o755
		}
		fh, err := d.OpenFile(name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, mode)
		if err == nil {
			bw := bufio.NewWriter(fh)
			err = w.git(nil, &limitWriter{w: bw, n: contextMaxFileSize}, "cat-file", "blob", f[2])
			if ferr := bw.Flush(); err == nil {
				err = ferr
			}
			fh.Close()
			if err != nil {
				return fmt.Errorf("writing %s: %w", p, err)
			}
			res.Files++
		}
		if d != root {
			d.Close()
		}
	}
	return nil
}

// safeContextPath accepts a restored path: relative, clean, no git
// metadata or ".." anywhere, bounded.
func safeContextPath(p string) bool {
	if p == "" || len(p) > contextMaxPathBytes || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00") {
		return false
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." || c == "." || IsDotGit(c) {
			return false
		}
	}
	return true
}

type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errors.New("file larger than the context limit")
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}
