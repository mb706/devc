// Package flock takes advisory file locks (flock(2)) with a bounded wait.
// devc uses them where two devc processes could act on the same thing at
// once: a box's life cycle, a harness's publication, a realm's context
// repository, the ingestion of a box's spool, a share, a data remote, a
// service login, the SSH gate's host key, the archive's upload state, and
// to let only one daemon run per runtime directory.
//
// Security rules: lock files live only in devc's own state and runtime
// directories; they are opened without following a symlink in the last
// component and hold no data.
package flock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ErrTimeout is returned (wrapped) when the lock was not free in time.
var ErrTimeout = errors.New("timed out waiting for a lock")

// Options tune Acquire.
type Options struct {
	// Shared takes a shared lock instead of an exclusive one.
	Shared bool
	// Timeout bounds the wait; zero means one minute.
	Timeout time.Duration
	// OnWait is called once when the lock is busy, before waiting.
	OnWait func()
}

// pollInterval is how often a busy lock is tried again.
const pollInterval = 50 * time.Millisecond

// Acquire locks path (creating it with mode 0600) and returns the function
// that releases the lock. It gives up when ctx ends or the timeout passes.
func Acquire(ctx context.Context, path string, o Options) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the lock file %s: %w", path, err)
	}
	how := unix.LOCK_EX
	if o.Shared {
		how = unix.LOCK_SH
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	deadline := time.Now().Add(timeout)
	waited := false
	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if !waited && o.OnWait != nil {
			o.OnWait()
		}
		waited = true
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%s: %w (%s)", path, ErrTimeout, timeout)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
