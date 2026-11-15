package doctor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/engine"
)

// DefaultStatxTimeout is how long the overlay-statx probe may take.
const DefaultStatxTimeout = 10 * time.Second

// checkOverlayStatx runs the helper container with a worker that asks
// statx() for the birth time of a file of its root filesystem. Some
// fuse-overlayfs versions (seen with 1.17 and libfuse 3.18.2) never answer
// that request for files in a lower layer, and programs that ask for it
// (Debian trixie's apt through sqv, Claude Code's installer, `stat FILE`)
// then hang for ever. The probe is a container of its own that devc
// watches and removes: a hung worker cannot be killed while the request is
// pending, so on a timeout the container's own fuse-overlayfs process is
// ended first, which fails the request.
func checkOverlayStatx(ctx context.Context, h *Host, es *engineState) (c Check) {
	var done bool
	c, done = engineSkip(Check{ID: "overlay-statx", Title: "statx() birth time on image layers"}, es)
	if done {
		return c
	}
	if h.HelperRootfs == "" {
		c.Status, c.Detail = Skip, "no toolbox to run the probe from"
		return c
	}
	if fi, err := os.Stat(filepath.Join(h.HelperRootfs, "bin", "devc")); err != nil || !fi.Mode().IsRegular() {
		c.Status, c.Detail = Skip, "the toolbox is not synced yet; run 'devc doctor --fix' or 'devc toolbox sync' first"
		return c
	}
	timeout := h.StatxTimeout
	if timeout <= 0 {
		timeout = DefaultStatxTimeout
	}
	name := "devc-doctor-statx-" + randomHex(4)
	args, err := engine.RunOptions{
		Rootfs: h.HelperRootfs, Network: "none",
		Labels:       map[string]string{"devc.role": "aux", "devc.aux": "doctor"},
		SecurityOpts: h.AuxSecurityOpts, CapDropAll: true,
		Tmpfs: []string{"/target"}, Env: []string{"DEVC_HELPER=1"},
		Cmd: []string{"/bin/devc", "box", "statx-worker"},
	}.CreateArgs(name)
	if err != nil {
		c.Status, c.Detail = Warn, "cannot build the probe: "+err.Error()
		return c
	}
	if _, err := h.Engine.Create(ctx, args); err != nil {
		c.Status, c.Detail = Warn, "cannot create the probe container: "+err.Error()
		return c
	}
	// Whatever happens, the container goes away again.
	defer func() {
		if msg := cleanupProbe(context.WithoutCancel(ctx), h, name); msg != "" {
			c.Detail += "; " + msg
			if c.Status == OK {
				c.Status = Warn
			}
		}
	}()
	if err := h.Engine.Start(ctx, name); err != nil {
		c.Status, c.Detail = Warn, "cannot start the probe container: "+err.Error()
		return c
	}
	start := time.Now()
	for {
		ci, err := h.Engine.Inspect(ctx, name)
		switch {
		case err != nil:
			c.Status, c.Detail = Warn, "cannot follow the probe container: "+err.Error()
			return c
		case !ci.State.Running && ci.State.Status != "created" && ci.State.Status != "initialized":
			if ci.State.ExitCode == 0 {
				c.Status, c.Detail = OK, fmt.Sprintf("statx() with the birth time returned in %s", time.Since(start).Round(10*time.Millisecond))
			} else {
				c.Status, c.Detail = Warn, fmt.Sprintf("the probe failed with exit status %d (see '%s logs' while it runs)", ci.State.ExitCode, h.Engine.Binary())
			}
			return c
		case time.Since(start) > timeout:
			c.Status = Fail
			c.Detail = fmt.Sprintf("statx() asking for the birth time of a file on an image layer did not return within %s: the storage's fuse-overlayfs hangs on it", timeout)
			c.Hint = "programs that ask for a file's birth time hang in boxes (Debian trixie's apt, Claude Code's installer, stat).\n" +
				"Use native overlay storage (kernel 5.13 or newer; in ~/.config/containers/storage.conf leave\n" +
				"mount_program unset under [storage.options.overlay], then 'podman system reset', which deletes\n" +
				"local images and containers), or install a fuse-overlayfs without the bug. See docs/host-setup.md."
			return c
		}
		select {
		case <-ctx.Done():
			c.Status, c.Detail = Warn, "interrupted: "+ctx.Err().Error()
			return c
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// cleanupProbe removes the probe container. A worker stuck in a fuse
// request survives SIGKILL, so when the container is still running the
// fuse-overlayfs process serving its root filesystem is ended first. It
// returns a message when the container could not be removed.
func cleanupProbe(ctx context.Context, h *Host, name string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ci, err := h.Engine.Inspect(ctx, name)
	if errors.Is(err, engine.ErrNoSuchContainer) {
		return ""
	}
	if err == nil && ci.State.Running && h.KillFuse != nil {
		_ = h.KillFuse(ci.ID, ci.GraphDriver.Data["UpperDir"])
	}
	if err := h.Engine.Remove(ctx, name); err != nil && !errors.Is(err, engine.ErrNoSuchContainer) {
		return fmt.Sprintf("the probe container %s could not be removed (%v): remove it with '%s rm -f %s'", name, err, h.Engine.Binary(), name)
	}
	return ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// KillFuseOverlayfs ends the fuse-overlayfs process that serves the root
// filesystem of one container: a process of this user named
// fuse-overlayfs whose arguments name the container's ID (the mount point
// and upper directory of a --rootfs overlay lie below
// overlay-containers/<id>/) or its upper directory (image containers).
func KillFuseOverlayfs(id, upperDir string) error {
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return fmt.Errorf("bad container id %q", id)
	}
	des, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	uid := os.Getuid()
	var killed []int
	for _, de := range des {
		pid, err := strconv.Atoi(de.Name())
		if err != nil || pid <= 1 {
			continue
		}
		dir := filepath.Join("/proc", de.Name())
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "fuse-overlayfs" {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || !fuseServes(strings.Split(string(cmdline), "\x00"), id, upperDir) {
			continue
		}
		if syscall.Kill(pid, syscall.SIGTERM) == nil {
			killed = append(killed, pid)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, pid := range killed {
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}

// fuseServes reports whether fuse-overlayfs arguments belong to the
// container with this ID or upper directory.
func fuseServes(args []string, id, upperDir string) bool {
	for _, a := range args {
		if strings.Contains(a, "/"+id+"/") {
			return true
		}
		if upperDir == "" {
			continue
		}
		for _, opt := range strings.Split(a, ",") {
			if opt == "upperdir="+upperDir {
				return true
			}
		}
	}
	return false
}
