// Package archive holds what the machine and the hub share about the
// archive of durable state (work order C17): the names of archived files,
// bundles and ref logs, the wire types of the hub's archive and secrets
// endpoints, the format of encrypted secret bundles, and the one place
// credential files are excluded. The machine side lives in
// internal/fleet, the hub side in internal/hub.
//
// What the archive carries of a realm (durable/realms/<realm>/): the
// host-written session journals, request, network and gate logs and the
// savepoint index; the shared harness state (transcripts and shared
// homes), which boxes write; and the relays and the context repository as
// git bundles in sequence with a log of their refs. Each file is stored in
// generations: a file that changes other than by growing gets a new one.
// Secrets never travel in the archive, and neither does anything ValidRel
// does not name (service logs, for example).
//
// Files: paths.go names archived files and checks them, and holds the
// credential and transcript patterns; wire.go the endpoints, classes and
// answers; sealed.go the encrypted secrets bundles.
//
// Security rules: every archive path is validated by ParsePath before it
// names a file on a hub or a machine; it accepts only the kinds of durable
// state above, with names a box cannot use to escape (no "..", no
// separators in components, a fixed character set). Credential files that
// harness manifests list (home.secret_files) are excluded by Excluded on
// every path by which a machine puts files into the archive or takes them
// out; the hub, which has no manifests, does not check for them.
package archive

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/mb706/devc/internal/names"
)

// Top-level directories of a machine's realm archive (on a hub, below
// archive/<machine>/<realm>/, and in an export).
const (
	FilesDir   = "files"
	BundlesDir = "bundles"
)

// Kinds of archive paths.
const (
	KindFile   = "file"   // files/<rel>[~<gen>]: one generation of a durable file
	KindBundle = "bundle" // bundles/<repo>/<seq>-<id>.bundle
	KindRefs   = "refs"   // bundles/<repo>/refs.jsonl: the refs after each bundle
)

// RepoContext names the context repository among the repositories
// ("relay/<project>" for relays).
const RepoContext = "context"

// RelayRepo is the repository name of a project's relay.
func RelayRepo(project string) string { return "relay/" + project }

// Limits of archive paths.
const (
	maxComponents = 24
	maxComponent  = 255
	maxPathLen    = 1024
	// MaxFileName bounds the last component of a file's path: the hub
	// stores generation g of a file as <name>~<g> with a record
	// <name>~<g>~meta, written through <name>~<g>~meta.tmp, and every one
	// of those names must fit the 255 bytes a file system allows
	// (finding E1-02). The longest suffix is "~1000000~meta.tmp".
	MaxFileName = maxComponent - len("~1000000") - len("~meta.tmp")
	// MaxGen bounds file generations.
	MaxGen = 1_000_000
	// MaxSeq bounds bundle sequence numbers.
	MaxSeq = 99_999_999
)

// componentRE is what a path component may hold. Box-made names outside
// it (spaces, control characters, non-ASCII, '~') are skipped and counted:
// '~' is reserved for generation suffixes.
var componentRE = regexp.MustCompile(`^[A-Za-z0-9._+=,@%-]+$`)

// bundleRE is <seq>-<id>.bundle.
var bundleRE = regexp.MustCompile(`^([0-9]{8})-([0-9a-f]{16})\.bundle$`)

// ValidComponent reports whether s may be a component of an archive path.
func ValidComponent(s string) bool {
	return s != "." && s != ".." && len(s) <= maxComponent && componentRE.MatchString(s)
}

// Path is a parsed archive path.
type Path struct {
	Kind string
	// Rel is a file's path below the realm's durable directory (KindFile)
	// and Gen its generation (0 is the first upload).
	Rel string
	Gen int
	// Repo is "relay/<project>" or "context" (KindBundle, KindRefs); Seq
	// and ID name a bundle.
	Repo string
	Seq  int
	ID   string
}

// String is the archive path.
func (p Path) String() string {
	switch p.Kind {
	case KindFile:
		return FilePath(p.Rel, p.Gen)
	case KindBundle:
		return BundlePath(p.Repo, p.Seq, p.ID)
	case KindRefs:
		return RefsPath(p.Repo)
	}
	return ""
}

// FilePath is the archive path of generation gen of a durable file.
func FilePath(rel string, gen int) string {
	if gen == 0 {
		return FilesDir + "/" + rel
	}
	return FilesDir + "/" + rel + "~" + strconv.Itoa(gen)
}

// BundlePath is the archive path of bundle seq (attempt id) of a
// repository.
func BundlePath(repo string, seq int, id string) string {
	return fmt.Sprintf("%s/%s/%08d-%s.bundle", BundlesDir, repo, seq, id)
}

// RefsPath is the archive path of a repository's ref log.
func RefsPath(repo string) string { return BundlesDir + "/" + repo + "/refs.jsonl" }

// ValidRepo checks a repository name.
func ValidRepo(repo string) error {
	if repo == RepoContext {
		return nil
	}
	if p, ok := strings.CutPrefix(repo, "relay/"); ok && names.Valid(p) {
		return nil
	}
	return fmt.Errorf("invalid repository %q", repo)
}

// ValidRel checks the path of a durable file below the realm's durable
// directory: only the kinds of durable state the archive carries, with
// valid names. Secrets, relays and the context repository never travel as
// files.
func ValidRel(rel string) error {
	if len(rel) > maxPathLen-len(FilesDir)-16 {
		return errors.New("path too long")
	}
	cs := strings.Split(rel, "/")
	if len(cs) > maxComponents {
		return errors.New("path too deep")
	}
	for _, c := range cs {
		if !ValidComponent(c) {
			return fmt.Errorf("invalid path component %q", c)
		}
	}
	if len(cs[len(cs)-1]) > MaxFileName {
		return fmt.Errorf("file name longer than %d bytes", MaxFileName)
	}
	ok := false
	switch cs[0] {
	case "journal":
		ok = len(cs) == 3 && names.Valid(cs[1]) && strings.HasSuffix(cs[2], ".jsonl") && names.ValidSessionID(strings.TrimSuffix(cs[2], ".jsonl"))
	case "requests", "savepoints":
		ok = len(cs) == 2 && strings.HasSuffix(cs[1], ".jsonl") && names.Valid(strings.TrimSuffix(cs[1], ".jsonl"))
	case "netlog":
		base := strings.TrimSuffix(strings.TrimSuffix(cs[len(cs)-1], ".1"), ".jsonl")
		ok = len(cs) == 2 && (strings.HasSuffix(cs[1], ".jsonl") || strings.HasSuffix(cs[1], ".jsonl.1")) && names.Valid(base)
	case "gate-log.jsonl", "gate-log.jsonl.1":
		ok = len(cs) == 1
	case "harness":
		ok = len(cs) >= 3 && names.Valid(cs[1])
	}
	if !ok {
		return fmt.Errorf("%q is not durable state the archive carries", rel)
	}
	return nil
}

// ParsePath validates an archive path and returns its parts. It accepts
// exactly what FilePath, BundlePath and RefsPath produce for valid inputs.
func ParsePath(p string) (Path, error) {
	if p == "" || len(p) > maxPathLen || strings.ContainsRune(p, 0) || path.Clean(p) != p || strings.HasPrefix(p, "/") {
		return Path{}, fmt.Errorf("invalid archive path %q", trunc(p))
	}
	top, rest, _ := strings.Cut(p, "/")
	switch top {
	case FilesDir:
		rel, gen := rest, 0
		if i := strings.LastIndexByte(rest, '~'); i >= 0 {
			n, err := strconv.Atoi(rest[i+1:])
			if err != nil || n < 1 || n > MaxGen || strconv.Itoa(n) != rest[i+1:] {
				return Path{}, fmt.Errorf("invalid generation in %q", trunc(p))
			}
			rel, gen = rest[:i], n
		}
		if err := ValidRel(rel); err != nil {
			return Path{}, err
		}
		return Path{Kind: KindFile, Rel: rel, Gen: gen}, nil
	case BundlesDir:
		i := strings.LastIndexByte(rest, '/')
		if i < 0 {
			break
		}
		repo, name := rest[:i], rest[i+1:]
		if err := ValidRepo(repo); err != nil {
			return Path{}, err
		}
		if name == "refs.jsonl" {
			return Path{Kind: KindRefs, Repo: repo}, nil
		}
		m := bundleRE.FindStringSubmatch(name)
		if m == nil {
			break
		}
		seq, _ := strconv.Atoi(m[1])
		return Path{Kind: KindBundle, Repo: repo, Seq: seq, ID: m[2]}, nil
	}
	return Path{}, fmt.Errorf("invalid archive path %q", trunc(p))
}

func trunc(s string) string {
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return strconv.Quote(s)
}

// Excluded reports whether rel (a path below a harness's state directory
// harness/<id>/) is a credential file by patterns (from
// config.Harness.SecretStatePatterns): it matches a pattern, or lies
// below a directory that matches one. This is the one place credential
// files are kept out of the archive, exports and restores.
func Excluded(patterns []string, rel string) bool {
	cs := strings.Split(rel, "/")
	for _, p := range patterns {
		if p == "." {
			return true
		}
		ps := strings.Split(p, "/")
		if len(ps) > len(cs) {
			continue
		}
		match := true
		for i, pc := range ps {
			if ok, err := path.Match(pc, cs[i]); err != nil || !ok {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// MatchGlob reports whether rel matches pattern component by component
// (path.Match within a component), where a component "**" matches any
// number of components, none included. Patterns come from harness
// manifests (transcript globs mapped to the state directory).
func MatchGlob(pattern, rel string) bool {
	return matchParts(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

func matchParts(ps, cs []string) bool {
	for len(ps) > 0 {
		if ps[0] == "**" {
			for i := 0; i <= len(cs); i++ {
				if matchParts(ps[1:], cs[i:]) {
					return true
				}
			}
			return false
		}
		if len(cs) == 0 {
			return false
		}
		if ok, err := path.Match(ps[0], cs[0]); err != nil || !ok {
			return false
		}
		ps, cs = ps[1:], cs[1:]
	}
	return len(cs) == 0
}

// MutableAllowed reports whether the file rel (below the realm's durable
// directory) may be archived as mutable: only shared harness state may
// (finding E1-04). Journals, request, network and gate logs are
// append-only whatever a machine says, and so are bundles and ref logs.
func MutableAllowed(rel string) bool { return strings.HasPrefix(rel, "harness/") }

// ClassOf is the class of a file below a harness's state directory: append
// when it matches one of the harness's transcript patterns
// (config.Harness.TranscriptStatePatterns), mutable otherwise.
func ClassOf(transcripts []string, within string) string {
	for _, p := range transcripts {
		if MatchGlob(p, within) {
			return ClassAppend
		}
	}
	return ClassMutable
}
