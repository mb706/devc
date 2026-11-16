// This file keeps a running box's network exceptions in its kernel rules
// in step with the project's exception record.

package box

// Network exceptions in the kernel (work order C13 section 2). The record
// of a project's exceptions (netpolicy.ReadExceptions) is the source of
// truth; for a running box of level internet its exception sets are made
// to match it, from the host, in the box's network namespace, after the
// namespace has been verified as the one devc bound the box's readiness to
// (finding B1). The elements carry timeouts, so they expire in the kernel
// without any process; they vanish with the namespace when the box stops,
// and the start hook puts back those still valid after the box has passed
// its readiness checks and the canary.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
)

// Lock takes the box lock (LockBox with the manager's timeout); callers
// that change a box's exceptions hold it, as lifecycle commands do.
func (m *Manager) Lock(ctx context.Context, ref names.ProjectRef) (func(), error) {
	return m.lockBox(ctx, ref)
}

// RunningLevel is the network level of ref's running box: the level it was
// started with when its capability record belongs to the running
// container, else the configured one. running is false when no box runs.
func (m *Manager) RunningLevel(ctx context.Context, ref names.ProjectRef) (level string, running bool, err error) {
	ci, err := m.Find(ctx, ref)
	if err != nil || ci == nil || !ci.State.Running {
		return "", false, err
	}
	if c, err := ReadCapabilities(m.Config.Paths, ref); err == nil && c.ContainerID == ci.ID && c.Network != "" {
		return c.Network, true, nil
	}
	r, err := m.Config.Resolved(ref)
	if err != nil {
		return "", true, err
	}
	return r.Network, true, nil
}

// SyncExceptions makes the exception sets of ref's running box match its
// record, if the box runs at level internet; applied says whether it did.
// The caller holds the box lock.
func (m *Manager) SyncExceptions(ctx context.Context, ref names.ProjectRef) (applied bool, err error) {
	level, running, err := m.RunningLevel(ctx, ref)
	if err != nil || !running || level != netpolicy.Internet {
		return false, err
	}
	ci, err := m.Find(ctx, ref)
	if err != nil || ci == nil || !ci.State.Running {
		return false, err
	}
	if err := m.verifyRunning(ctx, ref, ci); err != nil {
		return false, fmt.Errorf("%s runs without devc's network rules bound to it (%v): restart it with 'devc up %s'", names.BoxName(ref), err, ref)
	}
	list, err := netpolicy.ReadExceptions(m.Config.Paths.BoxNetExceptionsFile(ref))
	if err != nil {
		return false, err
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	if err := m.installer().ApplyExceptions(ctx, ci.NetworkSettings.SandboxKey, list, now()); err != nil {
		return false, err
	}
	return true, nil
}

// removeExceptions drops a project's exceptions (devc rm).
func (m *Manager) removeExceptions(ref names.ProjectRef) error {
	if err := os.Remove(m.Config.Paths.BoxNetExceptionsFile(ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the network exceptions: %w", err)
	}
	return nil
}

// NetExceptionHook puts the exceptions still valid back into a box that
// has just started (a StartHook: after the readiness checks and the
// canary, which therefore always run against the rules alone).
type NetExceptionHook struct{ Manager *Manager }

// AfterStart implements StartHook.
func (h NetExceptionHook) AfterStart(ctx context.Context, ref names.ProjectRef, _ string) error {
	if _, err := h.Manager.SyncExceptions(ctx, ref); err != nil {
		return fmt.Errorf("re-applying the network exceptions of %s: %w", ref, err)
	}
	return nil
}
