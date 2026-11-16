// This file builds the static tmux of the toolbox in a throwaway Alpine
// container.

package toolbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/mb706/devc/internal/engine"
)

// The static tmux of the toolbox (work order C03 section 1), built from a
// pinned release in a throwaway Alpine container:
//
//	tmux/<version>/tmux
//	tmux/current -> <version>
//	bin/tmux -> ../tmux/current/tmux
//	terminfo/              Alpine's compiled terminfo database (ncurses-terminfo
//	                       and kitty-terminfo)
const (
	TmuxVersion = "3.8"
	TmuxURL     = "https://github.com/tmux/tmux/releases/download/" + TmuxVersion + "/tmux-" + TmuxVersion + ".tar.gz"
	TmuxSHA256  = "e79c699c7e949dccd0a4a125e17b8d1261e311b16979dbc8eb34542f3966d82e"
	// TmuxBuildImage is pinned by tag. The statx() birth-time hang seen
	// with fuse-overlayfs on the development host does not affect this
	// build: musl's stat and busybox do not ask for the birth time.
	TmuxBuildImage = "docker.io/library/alpine:3.24.4"
)

// tmuxScript runs in the build container. It is a constant; the release
// arrives in environment variables. The tarball is verified before it is
// unpacked.
const tmuxScript = `set -eu
apk add --no-cache build-base libevent-dev libevent-static ncurses-dev ncurses-static ncurses-terminfo kitty-terminfo bison curl linux-headers pkgconf >/dev/null
cd /tmp
curl -fsSL --proto '=https' --retry 3 -o tmux.tar.gz "$TMUX_URL"
echo "$TMUX_SHA256  tmux.tar.gz" | sha256sum -c -
tar xzf tmux.tar.gz
cd "tmux-$TMUX_VERSION"
./configure --enable-static --disable-utf8proc >/dev/null
make -j"$(nproc)" >/dev/null
strip tmux
./tmux -V
mkdir -p /out/terminfo
cp tmux /out/tmux
cp -R /usr/share/terminfo/. /out/terminfo/
cp -R /etc/terminfo/. /out/terminfo/
chmod -R a+rX /out
`

// TmuxRecipe identifies the build recipe (image, release, script), so a
// cached build can be recognised as current.
func TmuxRecipe() string {
	sum := sha256.Sum256([]byte(TmuxBuildImage + "\x00" + TmuxURL + "\x00" + TmuxSHA256 + "\x00" + tmuxScript))
	return hex.EncodeToString(sum[:])[:12]
}

// TmuxDir is the directory of the tmux builds.
func (t *Toolbox) TmuxDir() string { return filepath.Join(t.Dir, "tmux") }

// TerminfoDir is the terminfo database (/opt/devc/terminfo in boxes).
func (t *Toolbox) TerminfoDir() string { return filepath.Join(t.Dir, "terminfo") }

// InstalledTmux returns the version tmux/current points at, or "".
func (t *Toolbox) InstalledTmux() string {
	v, err := os.Readlink(filepath.Join(t.TmuxDir(), "current"))
	if err != nil {
		return ""
	}
	if fi, err := os.Stat(filepath.Join(t.TmuxDir(), v, "tmux")); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	if target, err := os.Readlink(filepath.Join(t.BinDir(), "tmux")); err != nil || target != "../tmux/current/tmux" {
		return ""
	}
	return v
}

// BuildTmux builds the pinned tmux and installs it with the terminfo
// database. securityOpts are those of auxiliary containers.
func (t *Toolbox) BuildTmux(ctx context.Context, eng engine.Engine, securityOpts []string, out io.Writer) error {
	if err := mkdirs(t.Dir, t.TmuxDir(), t.BinDir()); err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(t.TmuxDir(), ".lock"))
	if err != nil {
		return err
	}
	defer unlock()

	build, err := os.MkdirTemp(t.TmuxDir(), ".build-")
	if err != nil {
		return err
	}
	defer removeTree(build)
	if err := os.Chmod(build, 0o755); err != nil {
		return err
	}
	if out != nil {
		fmt.Fprintf(out, "devc: building tmux %s in %s\n", TmuxVersion, TmuxBuildImage)
	}
	code, err := eng.RunStream(ctx, engine.RunOptions{
		Image: TmuxBuildImage, Labels: map[string]string{LabelRole: RoleAux, labelAux: "tmux-build"},
		SecurityOpts: securityOpts,
		Mounts:       []engine.RunMount{{Source: build, Target: "/out"}},
		Env:          []string{"TMUX_URL=" + TmuxURL, "TMUX_SHA256=" + TmuxSHA256, "TMUX_VERSION=" + TmuxVersion},
		Cmd:          []string{"sh", "-c", tmuxScript},
		Stdout:       out, Stderr: out, Timeout: 30 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("building tmux: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("building tmux failed with exit status %d", code)
	}
	bin := filepath.Join(build, "tmux")
	if fi, err := os.Lstat(bin); err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return fmt.Errorf("the tmux build produced no binary")
	}
	if fi, err := os.Lstat(filepath.Join(build, "terminfo")); err != nil || !fi.IsDir() {
		return fmt.Errorf("the tmux build produced no terminfo database")
	}

	verDir := filepath.Join(t.TmuxDir(), TmuxVersion)
	staged := filepath.Join(build, "version")
	if err := mkdirs(staged); err != nil {
		return err
	}
	if err := os.Rename(bin, filepath.Join(staged, "tmux")); err != nil {
		return err
	}
	if err := replaceDir(staged, verDir); err != nil {
		return err
	}
	if err := replaceDir(filepath.Join(build, "terminfo"), t.TerminfoDir()); err != nil {
		return err
	}
	if _, err := symlinkAtomic(TmuxVersion, filepath.Join(t.TmuxDir(), "current")); err != nil {
		return err
	}
	if _, err := symlinkAtomic("../tmux/current/tmux", filepath.Join(t.BinDir(), "tmux")); err != nil {
		return err
	}
	if out != nil {
		fmt.Fprintf(out, "devc: installed tmux %s\n", TmuxVersion)
	}
	return nil
}

// replaceDir moves src to dst, replacing an existing dst. Running boxes
// keep files they have open; tmux reads terminfo entries at attach time.
func replaceDir(src, dst string) error {
	old := dst + ".old"
	if err := removeTree(old); err != nil {
		return err
	}
	if err := os.Rename(dst, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	return removeTree(old)
}
