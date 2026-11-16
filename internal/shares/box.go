// This file puts shares into boxes: the mounts, and /opt/shares rendered
// at every start.

package shares

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// BoxHooks put a project's shares into its box (work order C16):
//
//	/opt/shares            read-only, rendered per box (BoxSharesDir)
//	/opt/shares/<name>     -> .v/<name>/current
//	/opt/shares/.v/<name>  durable/shares/<name>, read-only
//	/opt/shares/dep-source -> /opt/devc/skills/dep-source (built in)
//
// The mounts depend only on the configured list, never on what is
// published, so publishing does not change the box definition; a listed
// share that is not published yet is an empty directory whose link
// dangles until it is. current is followed inside the box, through the
// mounted directory, so a new version is visible at once in running boxes.
type BoxHooks struct {
	Paths *paths.Paths
	Out   io.Writer
}

var (
	_ box.Contributor = (*BoxHooks)(nil)
	_ box.Preparer    = (*BoxHooks)(nil)
)

// Contribute implements box.Contributor.
func (h *BoxHooks) Contribute(in *box.Inputs, s *box.Spec) error {
	r := in.Resolved
	ref := names.ProjectRef{Realm: r.Realm, Project: r.Project}
	if err := s.AddMount(box.Mount{Source: h.Paths.BoxSharesDir(ref), Target: paths.InBoxShares, ReadOnly: true}); err != nil {
		return err
	}
	for _, name := range r.Shares {
		if !names.ValidKind(names.KindShare, name) || name == config.BuiltinShare {
			return fmt.Errorf("invalid share %q", name)
		}
		if err := s.AddMount(box.Mount{Source: h.Paths.ShareDir(name), Target: paths.InBoxShareMount(name), ReadOnly: true}); err != nil {
			return err
		}
	}
	return nil
}

// Prepare implements box.Preparer: it renders /opt/shares for the box.
func (h *BoxHooks) Prepare(_ context.Context, in *box.Inputs, _ *box.Spec) error {
	r := in.Resolved
	ref := names.ProjectRef{Realm: r.Realm, Project: r.Project}
	parent := h.Paths.BoxLocalDir(ref)
	if err := h.Paths.Ensure(parent); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".shares-new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if err := os.Symlink(path.Join(paths.InBoxSkills, config.BuiltinShare), filepath.Join(stage, config.BuiltinShare)); err != nil {
		return err
	}
	if len(r.Shares) > 0 {
		if err := os.Mkdir(filepath.Join(stage, ".v"), 0o755); err != nil {
			return err
		}
	}
	svc := &Service{Paths: h.Paths}
	for _, name := range r.Shares {
		if err := os.Mkdir(filepath.Join(stage, ".v", name), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(path.Join(".v", name, "current"), filepath.Join(stage, name)); err != nil {
			return err
		}
		if svc.CurrentVersion(name) == "" && h.Out != nil {
			fmt.Fprintf(h.Out, "devc: warning: the share %s is listed for %s but not published yet; it appears at %s once it is ('devc share publish %s --from <realm>/<project> --path <dir>')\n",
				name, ref, path.Join(paths.InBoxShares, name), name)
		}
	}
	return box.ReplaceDir(stage, h.Paths.BoxSharesDir(ref))
}
