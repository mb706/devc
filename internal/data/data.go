// Package data implements a project's data directory on the host side
// (spec 14, work order C06): zones and their ownership, sealing, the trash,
// savepoints (reflink and restic drivers) and restores, and the box hooks
// that set the layout up at `devc up`, mount input zones read-only and give
// worktrees their data link.
//
// Security rules: the project tree is box-writable, so this package never
// walks it, reads it or changes it from the host (README rule 2). Every
// such operation is a request to a worker in the helper container (plan
// 3.3; internal/boxside), which sees only the project directory (and a
// staging directory), runs with the box's user-namespace mapping, no
// network and only the capabilities it needs, and resolves every path below
// the mount without following symlinks. Worker output is untrusted: it is
// decoded strictly with a size cap and validated, and text from it is made
// printable before it reaches a terminal. restic runs in the helper
// container as the box user without capabilities, with the data root
// mounted read-only. Unsealing, trash deletion and restoring are host-only
// commands; nothing here can be triggered from a box. The savepoint index
// and the restic repository live in the state directory, which no box can
// reach.
package data

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/toolbox"
)

// Service runs the data operations of one configuration.
type Service struct {
	Config  *config.Config
	Engine  engine.Engine
	Toolbox *toolbox.Toolbox
	// Executable and Version are the devc build synced into the toolbox
	// before the first helper container runs.
	Executable, Version string
	// Out receives progress messages and warnings.
	Out io.Writer
	// Commit returns the relay's tip of a project's default branch, or ""
	// when there is none.
	Commit func(ctx context.Context, ref names.ProjectRef) string
	// Now is the clock for records and IDs.
	Now func() time.Time
	// HostRoot prefixes host paths probed for facts ("/" normally).
	HostRoot string

	syncOnce sync.Once
	syncErr  error
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) printf(format string, args ...any) {
	if s.Out != nil {
		fmt.Fprintf(s.Out, format, args...)
	}
}

func (s *Service) commit(ctx context.Context, ref names.ProjectRef) string {
	if s.Commit == nil {
		return ""
	}
	return s.Commit(ctx, ref)
}

// Project is a project with data zones.
type Project struct {
	Ref      names.ProjectRef
	Resolved *config.Resolved
	Layout   boxside.DataLayout
	// HostDir is the box-writable project directory on the host.
	HostDir string
}

// ErrNoData is returned for a project that defines no data zones.
var ErrNoData = errors.New("the project defines no data zones")

// Project returns the data view of a project.
func (s *Service) Project(ref names.ProjectRef) (*Project, error) {
	r, err := s.Config.Resolved(ref)
	if err != nil {
		return nil, err
	}
	return ProjectOf(s.Config.Paths, r)
}

// ProjectOf returns the data view of a resolved project.
func ProjectOf(p *paths.Paths, r *config.Resolved) (*Project, error) {
	ref := names.ProjectRef{Realm: r.Realm, Project: r.Project}
	if !r.Data.Enabled {
		return nil, fmt.Errorf("%s: %w: define zones under [data] in its project.toml", ref, ErrNoData)
	}
	l := LayoutOf(r)
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	return &Project{Ref: ref, Resolved: r, Layout: l, HostDir: p.HostProjectDir(ref)}, nil
}

// LayoutOf is the data layout of a resolved project.
func LayoutOf(r *config.Resolved) boxside.DataLayout {
	return boxside.DataLayout{
		Root:   r.Data.Root,
		Inputs: append([]string{}, r.Data.Inputs...),
		Work:   append([]string{}, r.Data.Work...),
		Sealed: append([]string{}, r.Data.Sealed...),
	}
}

// InBoxRoot is the data root in the box.
func (p *Project) InBoxRoot() string {
	return path.Join(paths.InBoxProjectDir(p.Ref), p.Layout.Root)
}

// HostRoot is the data root on the host.
func (p *Project) HostRoot() string { return filepath.Join(p.HostDir, p.Layout.Root) }

// Normalize turns a path argument into a path relative to the data root.
// Relative arguments are relative to the data root; absolute ones must lie
// inside the data root, either as the box sees it (/w/<realm>/<project>/
// <root>/…) or on the host. ".." is refused anywhere in the argument, even
// where it would stay inside.
func Normalize(p *Project, projectsRoot, arg string) (string, error) {
	if arg == "" {
		return "", errors.New("empty path")
	}
	if strings.ContainsRune(arg, 0) {
		return "", errors.New("the path contains a NUL byte")
	}
	for _, c := range strings.Split(arg, "/") {
		if c == ".." {
			return "", fmt.Errorf("%q: \"..\" is not allowed in data paths", arg)
		}
	}
	rel := arg
	if strings.HasPrefix(arg, "/") {
		roots := []string{p.InBoxRoot(), p.HostRoot()}
		if real, err := filepath.EvalSymlinks(projectsRoot); err == nil {
			// The projects root is the user's own directory; resolving it
			// (and only it) is safe.
			roots = append(roots, filepath.Join(real, p.Ref.Realm, p.Ref.Project, p.Layout.Root))
		}
		clean := path.Clean(arg)
		rel = ""
		for _, r := range roots {
			if clean == r {
				return "", fmt.Errorf("%s is the data root itself; name an entry inside it", arg)
			}
			if after, ok := strings.CutPrefix(clean, r+"/"); ok {
				rel = after
				break
			}
		}
		if rel == "" {
			return "", fmt.Errorf("%s is not inside the data root of %s (%s in the box, %s on the host)", arg, p.Ref, p.InBoxRoot(), p.HostRoot())
		}
	}
	rel = strings.TrimSuffix(path.Clean(rel), "/")
	rel = strings.TrimPrefix(rel, "./")
	if err := boxside.CheckDataPath(rel); err != nil {
		return "", fmt.Errorf("%q: %w", arg, err)
	}
	return rel, nil
}
