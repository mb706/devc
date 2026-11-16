// This file connects the data directory to the box's life cycle: input
// zones mounted read-only, the layout set up before a start, data links
// after it.

package data

import (
	"context"
	"fmt"
	"path"
	"path/filepath"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
)

// BoxHooks connect the data directory to the box lifecycle: input zones are
// mounted read-only over the project mount (Contributor), the layout is set
// up by the setup worker before the box starts (Preparer), and worktrees
// get their data links once it runs (StartHook).
//
// A project mounted read-write into another box ([[mounts]] mode = "rw")
// gets its input zones mounted read-only there too, and its layout is set
// up as well: otherwise the other box could write the inputs, or swap a
// zone directory for a symlink while this project's box is being started.
type BoxHooks struct {
	Service *Service
}

var (
	_ box.Contributor = (*BoxHooks)(nil)
	_ box.Preparer    = (*BoxHooks)(nil)
	_ box.StartHook   = (*BoxHooks)(nil)
)

// dataProjects returns the project itself (when it has data) and every
// project with data that it mounts read-write.
func (h *BoxHooks) dataProjects(r *config.Resolved) ([]*Project, error) {
	var out []*Project
	if r.Data.Enabled {
		p, err := ProjectOf(h.Service.Config.Paths, r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	for _, m := range r.Mounts {
		if m.Mode != "rw" {
			continue
		}
		ref := names.ProjectRef{Realm: r.Realm, Project: m.Project}
		if err := ref.Validate(); err != nil {
			return nil, err
		}
		or, err := h.Service.Config.Resolved(ref)
		if err != nil {
			return nil, err
		}
		if !or.Data.Enabled {
			continue
		}
		p, err := ProjectOf(h.Service.Config.Paths, or)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Contribute implements box.Contributor: one read-only bind mount per input
// zone, nested in the project mount. They are part of the configuration
// hash like every other argument.
func (h *BoxHooks) Contribute(in *box.Inputs, s *box.Spec) error {
	ps, err := h.dataProjects(in.Resolved)
	if err != nil {
		return err
	}
	for _, p := range ps {
		for _, z := range p.Layout.Inputs {
			m := box.Mount{
				Source:   filepath.Join(p.HostRoot(), filepath.FromSlash(z)),
				Target:   path.Join(p.InBoxRoot(), z),
				ReadOnly: true,
			}
			if err := s.AddMount(m); err != nil {
				return fmt.Errorf("mounting the input zone %s of %s: %w", z, p.Ref, err)
			}
		}
	}
	return nil
}

// Prepare implements box.Preparer: the setup worker runs for every data
// project the box mounts before the box starts.
func (h *BoxHooks) Prepare(ctx context.Context, in *box.Inputs, _ *box.Spec) error {
	ps, err := h.dataProjects(in.Resolved)
	if err != nil {
		return err
	}
	for _, p := range ps {
		res, err := h.Service.Setup(ctx, p)
		if err != nil {
			return fmt.Errorf("setting up the data directory of %s: %w", p.Ref, err)
		}
		if len(res.Created) > 0 {
			h.Service.printf("devc: %s: created %d data director%s\n", p.Ref, len(res.Created), map[bool]string{true: "y", false: "ies"}[len(res.Created) == 1])
		}
	}
	return nil
}

// AfterStart implements box.StartHook: every worktree gets its data link.
func (h *BoxHooks) AfterStart(ctx context.Context, ref names.ProjectRef, container string) error {
	r, err := h.Service.Config.Resolved(ref)
	if err != nil || !r.Data.Enabled {
		return err
	}
	p, err := ProjectOf(h.Service.Config.Paths, r)
	if err != nil {
		return err
	}
	rep, err := h.Service.Links(ctx, p, container)
	if err != nil {
		return err
	}
	for _, sk := range rep.Skipped {
		h.Service.printf("devc: %s: no data link in %s\n", ref, sk)
	}
	return nil
}
