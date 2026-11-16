// Package shares publishes directories from a project's relay as shares:
// read-only trees mounted at /opt/shares/<name> into the boxes that list
// them, possibly in other realms (work order C16). A share puts text in
// front of other agents, so publishing is a deliberate act on the host.
//
// Layout (paths.Paths):
//
//	durable/shares/<name>/<version>/   one published version (a UTC stamp)
//	durable/shares/<name>/current      -> <version>
//	durable/shares/.staging/           versions while they are extracted
//	durable/share-log/<name>.jsonl     the published versions and their sources
//
// Boxes mount durable/shares/<name> whole (read-only), and /opt/shares/<name>
// is a link to its current entry: a new version is visible in running
// boxes at once (box.go).
//
// Security rules: the content comes only from the relay, a host-owned bare
// repository, through host git (internal/gitx: --git-dir, "/" as working
// directory, no hooks, no configuration of the user's), and only committed
// content can be published: never the project directory, which the box
// writes (README rule 2). Before anything is extracted the change is shown
// as git's file list and diff from the relay, made printable. The tar
// stream of git archive is unpacked by internal/extract, which writes
// nothing outside the new version directory and refuses symlinks leaving
// it, special files and oversized trees. Share names are validated before
// they become paths (rule 4).
package shares

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
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/extract"
	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// SkillFile marks a share as a skill: box init links shares whose root
// holds it into the harnesses' skill directories.
const SkillFile = "SKILL.md"

// Version is one line of a share's log.
type Version struct {
	V       int       `json:"v"`
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
	// The source: project, the ref as asked (or the default branch), the
	// commit it resolved to, the directory and its tree object.
	Realm   string `json:"realm"`
	Project string `json:"project"`
	Ref     string `json:"ref"`
	Commit  string `json:"commit"`
	Path    string `json:"path"`
	Tree    string `json:"tree"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
	Skipped int    `json:"skipped"`
	Skill   bool   `json:"skill"`
}

// Source names the project a version came from.
func (v *Version) Source() string { return v.Realm + "/" + v.Project }

// Service publishes and manages shares.
type Service struct {
	Paths *paths.Paths
	Git   *gitx.Host
	Now   func() time.Time
	// Limits bound one share (extract.ShareLimits when zero).
	Limits extract.Limits
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) limits() extract.Limits {
	if s.Limits.MaxEntries == 0 {
		return extract.ShareLimits
	}
	return s.Limits
}

// CheckName validates a share name for publishing.
func CheckName(name string) error {
	if err := names.Check(names.KindShare, name); err != nil {
		return err
	}
	if name == config.BuiltinShare {
		return fmt.Errorf("%q is the built-in skill every box has; choose another name", name)
	}
	return nil
}

// CheckPath validates the directory of a publish: a clean path relative to
// the repository root ("." for the root itself).
func CheckPath(p string) error {
	switch {
	case p == "":
		return errors.New("the path is empty")
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%q must be relative to the repository root", p)
	case strings.ContainsAny(p, "\x00\n\r"):
		return fmt.Errorf("%q contains a control character", p)
	case strings.HasPrefix(p, "-"):
		return fmt.Errorf("%q must not start with '-'", p)
	case path.Clean(p) != p:
		return fmt.Errorf("%q is not clean; write %q", p, path.Clean(p))
	case slices.Contains(strings.Split(p, "/"), ".."):
		return fmt.Errorf("%q must not contain ..", p)
	}
	return nil
}

var hexRE = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// Plan is what a publish would do, prepared before asking the user.
type Plan struct {
	Name    string
	Source  names.ProjectRef
	Relay   string
	Ref     string
	Commit  string
	Path    string
	Tree    string
	Files   int
	Bytes   int64
	Current *Version
	// Unchanged: the current version has the same tree from the same
	// project.
	Unchanged bool
	// Stat and Diff compare the current version with the new tree in the
	// relay (made printable); empty when there is no current version or
	// its tree is not in this relay, in which case List names the files.
	Stat, Diff string
	List       []string
	// DiffTruncated is set when Diff was cut at maxDiff bytes.
	DiffTruncated bool
}

// maxDiff bounds the diff shown before a publish.
const maxDiff = 1 << 20

// Prepare resolves what publishing dir of src's relay at ref as share name
// would publish and compares it with the current version. ref empty means
// the relay's default branch (its HEAD).
func (s *Service) Prepare(ctx context.Context, name string, src names.ProjectRef, dir, ref string) (*Plan, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	if err := src.Validate(); err != nil {
		return nil, err
	}
	if err := CheckPath(dir); err != nil {
		return nil, fmt.Errorf("--path: %w", err)
	}
	relay := s.Paths.RelayRepo(src)
	if fi, err := os.Stat(relay); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s has no relay: shares are published from committed content in the relay; run 'devc project init %s' (and 'devc bank %s') first", src, src, src)
	}
	p := &Plan{Name: name, Source: src, Relay: relay, Path: dir}
	commit, shown, err := s.resolve(ctx, relay, ref)
	if err != nil {
		return nil, err
	}
	p.Commit, p.Ref = commit, shown
	obj := commit + ":" + dir
	if dir == "." {
		obj = commit + "^{tree}"
	}
	tree, err := s.git(ctx, relay, "rev-parse", "--verify", "--quiet", obj)
	if err != nil || tree == "" {
		return nil, fmt.Errorf("%s does not exist at %s in the relay of %s (only committed and banked content can be published)", dir, shown, src)
	}
	if typ, err := s.git(ctx, relay, "cat-file", "-t", tree); err != nil || typ != "tree" {
		return nil, fmt.Errorf("%s at %s is not a directory", dir, shown)
	}
	p.Tree = tree
	if err := s.measure(ctx, p); err != nil {
		return nil, err
	}
	cur, err := s.Current(name)
	if err != nil {
		return nil, err
	}
	p.Current = cur
	switch {
	case cur == nil:
	case cur.Tree == tree:
		p.Unchanged = true
	case s.hasObject(ctx, relay, cur.Tree):
		if err := s.diff(ctx, p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// resolve turns ref (a branch name or a commit ID) into a commit of the
// relay and the name to show for it.
func (s *Service) resolve(ctx context.Context, relay, ref string) (string, string, error) {
	var rev, shown string
	switch {
	case ref == "":
		head, err := s.git(ctx, relay, "symbolic-ref", "--quiet", "HEAD")
		b, ok := strings.CutPrefix(head, "refs/heads/")
		if err != nil || !ok || names.CheckRef(b) != nil {
			return "", "", errors.New("the relay has no default branch: name one with --ref")
		}
		rev, shown = head, b
	case hexRE.MatchString(ref):
		rev, shown = ref, ref
	default:
		b := strings.TrimPrefix(ref, "refs/heads/")
		if err := names.CheckRef(b); err != nil {
			return "", "", fmt.Errorf("--ref %q is neither a branch name nor a commit ID: %w", ref, err)
		}
		rev, shown = "refs/heads/"+b, b
	}
	commit, err := s.git(ctx, relay, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil || !hexRE.MatchString(commit) {
		return "", "", fmt.Errorf("%s is not a branch or commit of the relay", shown)
	}
	return commit, shown, nil
}

// measure counts the files of the tree and their size, refusing a tree
// beyond the limits before anything is extracted.
func (s *Service) measure(ctx context.Context, p *Plan) error {
	lim := s.limits()
	res, err := s.Git.Run(ctx, gitx.Call{Relay: p.Relay, Mode: gitx.Internal, MaxOutput: 64 << 20,
		Args: []string{"ls-tree", "-r", "-t", "-l", "-z", "--full-tree", p.Tree}})
	if err != nil {
		return fmt.Errorf("listing %s: %w", p.Path, err)
	}
	if res.Truncated {
		return fmt.Errorf("%s has too many files for a share (more than %d)", p.Path, lim.MaxEntries)
	}
	for _, rec := range bytes.Split(res.Stdout, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(rec, []byte{'\t'})
		f := strings.Fields(string(meta))
		if !ok || len(f) != 4 {
			return fmt.Errorf("unexpected output of git ls-tree: %q", rec)
		}
		p.Files++
		if f[1] == "blob" {
			n, err := strconv.ParseInt(f[3], 10, 64)
			if err != nil {
				return fmt.Errorf("unexpected size in git ls-tree: %q", f[3])
			}
			p.Bytes += n
			if n > lim.MaxFile {
				return fmt.Errorf("%s/%s is larger than %d bytes, the limit for a file in a share", p.Path, gitx.Sanitize(name), lim.MaxFile)
			}
		}
		if len(p.List) < 10000 && f[1] != "tree" {
			p.List = append(p.List, gitx.Sanitize(name))
		}
	}
	switch {
	case p.Files > lim.MaxEntries:
		return fmt.Errorf("%s has %d entries, more than the %d a share may have", p.Path, p.Files, lim.MaxEntries)
	case p.Bytes > lim.MaxBytes:
		return fmt.Errorf("%s holds %d bytes, more than the %d a share may hold", p.Path, p.Bytes, lim.MaxBytes)
	}
	return nil
}

func (s *Service) hasObject(ctx context.Context, relay, oid string) bool {
	if !hexRE.MatchString(oid) {
		return false
	}
	typ, err := s.git(ctx, relay, "cat-file", "-t", oid)
	return err == nil && typ == "tree"
}

// diff compares the current version's tree with the new one, in the relay.
func (s *Service) diff(ctx context.Context, p *Plan) error {
	res, err := s.Git.Run(ctx, gitx.Call{Relay: p.Relay, Mode: gitx.Display,
		Args: []string{"diff", "--stat=120", "--summary", p.Current.Tree, p.Tree}})
	if err != nil {
		return fmt.Errorf("comparing with the current version: %w", err)
	}
	p.Stat = gitx.Sanitize(res.Stdout)
	res, err = s.Git.Run(ctx, gitx.Call{Relay: p.Relay, Mode: gitx.Display, MaxOutput: maxDiff,
		Args: []string{"diff", p.Current.Tree, p.Tree}})
	if err != nil {
		return fmt.Errorf("comparing with the current version: %w", err)
	}
	p.Diff, p.DiffTruncated = gitx.Sanitize(res.Stdout), res.Truncated
	return nil
}

// git runs one internal git command on a relay and returns its trimmed
// output.
func (s *Service) git(ctx context.Context, relay string, args ...string) (string, error) {
	res, err := s.Git.Run(ctx, gitx.Call{Relay: relay, Mode: gitx.Internal, Args: args})
	return strings.TrimSpace(string(res.Stdout)), err
}

// lock serialises the changes to one share.
func (s *Service) lock(ctx context.Context, name string) (func(), error) {
	dir := filepath.Join(s.Paths.LocalDir(), "locks")
	if err := s.Paths.Ensure(dir); err != nil {
		return nil, err
	}
	return flock.Acquire(ctx, filepath.Join(dir, "share-"+name+".lock"), flock.Options{Timeout: 5 * time.Minute})
}

var stampRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(-[0-9]{1,4})?$`)

// ValidVersion reports whether v names a version directory.
func ValidVersion(v string) bool { return stampRE.MatchString(v) }

// Publish extracts the planned tree as a new version and makes it current.
func (s *Service) Publish(ctx context.Context, p *Plan) (*Version, *extract.Report, error) {
	unlock, err := s.lock(ctx, p.Name)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	dir := s.Paths.ShareDir(p.Name)
	staging := s.Paths.ShareStagingDir()
	for _, d := range []string{dir, staging} {
		if err := s.Paths.Ensure(d); err != nil {
			return nil, nil, err
		}
	}
	tmp, err := os.MkdirTemp(staging, p.Name+"-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	tree := filepath.Join(tmp, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		return nil, nil, err
	}
	rep, err := s.archive(ctx, p, tree)
	if err != nil {
		return nil, rep, err
	}
	fi, err := os.Lstat(filepath.Join(tree, SkillFile))
	skill := err == nil && fi.Mode().IsRegular()

	now := s.now().UTC()
	base := now.Format("20060102T150405Z")
	stamp := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, stamp)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		stamp = fmt.Sprintf("%s-%d", base, i)
	}
	if err := os.Rename(tree, filepath.Join(dir, stamp)); err != nil {
		return nil, rep, err
	}
	v := &Version{V: 1, Version: stamp, Time: now, Realm: p.Source.Realm, Project: p.Source.Project, Ref: p.Ref,
		Commit: p.Commit, Path: p.Path, Tree: p.Tree, Files: rep.Files, Bytes: rep.Bytes, Skipped: rep.SkippedCount, Skill: skill}
	if err := s.appendLog(p.Name, v); err != nil {
		_ = os.RemoveAll(filepath.Join(dir, stamp))
		return nil, rep, err
	}
	if err := symlinkAtomic(stamp, filepath.Join(dir, "current")); err != nil {
		return nil, rep, err
	}
	return v, rep, nil
}

// archive streams git archive of the tree into the extractor.
func (s *Service) archive(ctx context.Context, p *Plan, dir string) (*extract.Report, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		code, err := s.Git.Serve(ctx, gitx.Call{Relay: p.Relay, Mode: gitx.Internal, Timeout: 30 * time.Minute,
			Args: []string{"archive", "--format=tar", p.Tree}}, pw, &limitedBuffer{b: &stderr, max: 64 << 10})
		if err == nil && code != 0 {
			err = fmt.Errorf("git archive: exit status %d: %s", code, gitx.Sanitize(bytes.TrimSpace(stderr.Bytes())))
		}
		pw.CloseWithError(err)
		done <- err
	}()
	rep, err := extract.Tar(pr, dir, extract.Options{Limits: s.limits()})
	if err != nil {
		cancel()
		pr.CloseWithError(err)
		<-done
		return rep, fmt.Errorf("unpacking %s: %w", p.Path, err)
	}
	// Drain what git still writes (the tar trailer) so it exits normally.
	_, _ = io.Copy(io.Discard, io.LimitReader(pr, 1<<20))
	if gerr := <-done; gerr != nil {
		return rep, gerr
	}
	return rep, nil
}

type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

const noFollow = unix.O_NOFOLLOW

// maxLog bounds a share's log when it is read.
const maxLog = 8 << 20

func (s *Service) appendLog(name string, v *Version) error {
	file := s.Paths.ShareLogFile(name)
	if err := s.Paths.Ensure(filepath.Dir(file)); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_SYNC|noFollow, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Log returns the published versions of a share, oldest first.
func (s *Service) Log(name string) ([]Version, error) {
	if err := names.Check(names.KindShare, name); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.Paths.ShareLogFile(name), os.O_RDONLY|noFollow, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Version
	sc := bufio.NewScanner(io.LimitReader(f, maxLog))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var v Version
		if json.Unmarshal(sc.Bytes(), &v) != nil || v.V != 1 || !ValidVersion(v.Version) {
			continue
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// CurrentVersion returns the version current points at ("" when none).
func (s *Service) CurrentVersion(name string) string {
	v, err := os.Readlink(filepath.Join(s.Paths.ShareDir(name), "current"))
	if err != nil || !ValidVersion(v) {
		return ""
	}
	if fi, err := os.Lstat(filepath.Join(s.Paths.ShareDir(name), v)); err != nil || !fi.IsDir() {
		return ""
	}
	return v
}

// Current returns the log record of the current version, nil when the
// share has none. A current version without a record (a log lost by hand)
// is described by its name alone.
func (s *Service) Current(name string) (*Version, error) {
	cur := s.CurrentVersion(name)
	if cur == "" {
		return nil, nil
	}
	log, err := s.Log(name)
	if err != nil {
		return nil, err
	}
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].Version == cur {
			return &log[i], nil
		}
	}
	return &Version{V: 1, Version: cur}, nil
}

// Info describes one published share.
type Info struct {
	Name     string   `json:"name"`
	Current  *Version `json:"current"`
	Versions int      `json:"versions"`
}

// List returns the shares that have a current version, sorted by name.
func (s *Service) List() ([]Info, error) {
	des, err := os.ReadDir(s.Paths.SharesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Info{}
	for _, de := range des {
		if !de.IsDir() || !names.ValidKind(names.KindShare, de.Name()) {
			continue
		}
		cur, err := s.Current(de.Name())
		if err != nil {
			return nil, err
		}
		if cur == nil {
			continue
		}
		n := 0
		if vs, err := os.ReadDir(s.Paths.ShareDir(de.Name())); err == nil {
			for _, v := range vs {
				if v.IsDir() && ValidVersion(v.Name()) {
					n++
				}
			}
		}
		out = append(out, Info{Name: de.Name(), Current: cur, Versions: n})
	}
	return out, nil
}

// Remove deletes every version of a share and its log. The share's
// directory itself stays, empty: running boxes that mount it keep a valid
// (empty) mount and see a later publish under the same name.
func (s *Service) Remove(ctx context.Context, name string) (int, error) {
	if err := names.Check(names.KindShare, name); err != nil {
		return 0, err
	}
	dir := s.Paths.ShareDir(name)
	if s.CurrentVersion(name) == "" {
		if _, err := os.Lstat(s.Paths.ShareLogFile(name)); err != nil {
			return 0, fmt.Errorf("there is no share %q", name)
		}
	}
	unlock, err := s.lock(ctx, name)
	if err != nil {
		return 0, err
	}
	defer unlock()
	// current goes first, so no box follows it into a version being deleted.
	if err := os.Remove(filepath.Join(dir, "current")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	des, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	n := 0
	for _, de := range des {
		if err := os.RemoveAll(filepath.Join(dir, de.Name())); err != nil {
			return n, err
		}
		if ValidVersion(de.Name()) {
			n++
		}
	}
	if err := os.Remove(s.Paths.ShareLogFile(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return n, err
	}
	return n, nil
}

// symlinkAtomic makes p a symlink to target through a uniquely named
// temporary link and a rename.
func symlinkAtomic(target, p string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(p), ".link-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	l := filepath.Join(tmp, "l")
	if err := os.Symlink(target, l); err != nil {
		return err
	}
	return os.Rename(l, p)
}
