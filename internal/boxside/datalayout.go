// This file describes a project's data layout (the data root and its
// zones) and the names devc makes in .devc: where a path lies, what may be
// sealed, savepoint and trash IDs.

package boxside

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/mb706/devc/internal/names"
)

// Zone kinds of a project's data directory (spec 14.1).
const (
	ZoneInputs = "inputs"
	ZoneWork   = "work"
	ZoneSealed = "sealed"
)

// DataLayout is a project's data directory as the configuration defines
// it: the data root (one name beside main/) and the zone paths relative to
// it. The host sends it to every data worker, which checks it again.
type DataLayout struct {
	Root   string   `json:"root"`
	Inputs []string `json:"inputs"`
	Work   []string `json:"work"`
	Sealed []string `json:"sealed"`
}

// Zone is one zone directory.
type Zone struct {
	Kind string
	Path string
}

// Zones lists every zone, inputs first, then work, then sealed, each in
// configuration order.
func (l DataLayout) Zones() []Zone {
	var out []Zone
	for _, z := range []struct {
		kind  string
		paths []string
	}{{ZoneInputs, l.Inputs}, {ZoneWork, l.Work}, {ZoneSealed, l.Sealed}} {
		for _, p := range z.paths {
			out = append(out, Zone{Kind: z.kind, Path: p})
		}
	}
	return out
}

// Validate checks what the configuration loader checks, again: the data
// root is a valid name other than main, .repo and .devc; zone paths are
// clean, relative, without ".." or components starting with "-", and no
// zone contains another.
func (l DataLayout) Validate() error {
	if !names.Valid(l.Root) || l.Root == "main" || strings.HasPrefix(l.Root, ".") {
		return fmt.Errorf("invalid data root %q", l.Root)
	}
	zs := l.Zones()
	if len(zs) == 0 {
		return errors.New("the project defines no data zones")
	}
	for i, z := range zs {
		if err := CheckDataPath(z.Path); err != nil {
			return fmt.Errorf("zone %q: %w", z.Path, err)
		}
		for _, c := range strings.Split(z.Path, "/") {
			if strings.HasPrefix(c, "-") {
				return fmt.Errorf("zone %q: a component starts with '-'", z.Path)
			}
		}
		for _, o := range zs[:i] {
			if within(o.Path, z.Path) || within(z.Path, o.Path) {
				return fmt.Errorf("zones %q and %q overlap", o.Path, z.Path)
			}
		}
	}
	return nil
}

// CheckDataPath accepts a clean relative path below the data root: not
// empty, not ".", no leading "/", no ".." component, no NUL, no empty
// component.
func CheckDataPath(p string) error {
	switch {
	case p == "" || p == ".":
		return errors.New("empty path")
	case strings.HasPrefix(p, "/"):
		return errors.New("absolute path")
	case strings.ContainsRune(p, 0):
		return errors.New("NUL in path")
	case len(p) > 4096:
		return errors.New("path too long")
	case path.Clean(p) != p:
		return fmt.Errorf("path is not clean (write %q)", path.Clean(p))
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return errors.New("\"..\" component")
		}
	}
	return nil
}

// within reports whether p equals dir or lies below it.
func within(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// Place says where a path below the data root lies.
type Place struct {
	// Kind is the kind of the zone containing the path, "" outside zones.
	Kind string
	// Zone is the zone's path; Rest the path below it ("" for the zone
	// directory itself).
	Zone string
	Rest string
	// Ancestor is set when the path is a directory above one or more
	// zones (the zones' parents up to the data root).
	Ancestor bool
}

// IsZone reports whether the path is a zone directory itself.
func (p Place) IsZone() bool { return p.Kind != "" && p.Rest == "" }

// Structural reports whether the path is part of the layout that setup
// maintains: a zone directory or one of its ancestors.
func (p Place) Structural() bool { return p.IsZone() || p.Ancestor }

// Classify says where rel (a path accepted by CheckDataPath) lies.
func (l DataLayout) Classify(rel string) Place {
	for _, z := range l.Zones() {
		switch {
		case rel == z.Path:
			return Place{Kind: z.Kind, Zone: z.Path}
		case strings.HasPrefix(rel, z.Path+"/"):
			return Place{Kind: z.Kind, Zone: z.Path, Rest: strings.TrimPrefix(rel, z.Path+"/")}
		}
	}
	for _, z := range l.Zones() {
		if strings.HasPrefix(z.Path, rel+"/") {
			return Place{Ancestor: true}
		}
	}
	return Place{}
}

// SealTarget checks that rel names a direct child of a sealed zone and
// returns the zone and the child's name.
func (l DataLayout) SealTarget(rel string) (zone, name string, err error) {
	if err := CheckDataPath(rel); err != nil {
		return "", "", err
	}
	pl := l.Classify(rel)
	switch {
	case pl.Kind != ZoneSealed && pl.Kind != "":
		return "", "", fmt.Errorf("%s lies in the %s zone %s; only direct children of a sealed zone (%s) can be sealed", rel, pl.Kind, pl.Zone, l.sealedList())
	case pl.Kind == "":
		return "", "", fmt.Errorf("%s is not in a sealed zone; only direct children of a sealed zone (%s) can be sealed", rel, l.sealedList())
	case pl.Rest == "":
		return "", "", fmt.Errorf("%s is a sealed zone itself; seal one of its entries", rel)
	case strings.Contains(pl.Rest, "/"):
		return "", "", fmt.Errorf("%s is nested inside %s/%s; only direct children of a sealed zone can be sealed", rel, pl.Zone, strings.SplitN(pl.Rest, "/", 2)[0])
	}
	return pl.Zone, pl.Rest, nil
}

func (l DataLayout) sealedList() string {
	if len(l.Sealed) == 0 {
		return "the project has none"
	}
	return strings.Join(l.Sealed, ", ")
}

// FirstWork is the first work zone, "" when there is none.
func (l DataLayout) FirstWork() string {
	if len(l.Work) == 0 {
		return ""
	}
	return l.Work[0]
}

// IDs of savepoints and trash entries are used as directory names in .devc;
// they are made by devc and validated wherever they are read back.
var (
	savepointIDRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(-[a-z0-9][a-z0-9._-]{0,62})?$`)
	trashIDRE     = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-z0-9][a-z0-9._-]{0,80}$`)
)

// StampFormat is the UTC timestamp at the start of savepoint and trash IDs.
const StampFormat = "20060102T150405Z"

// ValidSavepointID reports whether id is a savepoint ID: a UTC timestamp,
// optionally followed by "-" and a name.
func ValidSavepointID(id string) bool { return savepointIDRE.MatchString(id) }

// ValidTrashID reports whether id is a trash entry ID: a UTC timestamp, "-"
// and a sanitised name.
func ValidTrashID(id string) bool { return trashIDRE.MatchString(id) }

// TrashLabel turns an entry's name into the label part of a trash ID:
// lower case, other characters replaced by "-", at most 48 characters.
func TrashLabel(name string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(name) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= 48 {
			break
		}
	}
	s := strings.TrimLeft(b.String(), "._-")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		return "entry"
	}
	return s
}

// sortedUnique returns the sorted distinct elements of ss.
func sortedUnique(ss []string) []string {
	out := slices.Clone(ss)
	slices.Sort(out)
	return slices.Compact(out)
}
