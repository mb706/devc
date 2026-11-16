// This file installs the pinned Node.js release into the toolbox.

package toolbox

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"

	"github.com/mb706/devc/internal/extract"
	"github.com/mb706/devc/internal/paths"
)

// The Node.js of the toolbox (work order C16), for harnesses whose
// manifest says install.needs = ["node"] (pi): an official build for the
// host architecture, pinned and verified like restic (restic.go):
//
//	node/<version>/bin/node, lib/, …   one release, unpacked
//	node/current -> <version>
//
// The release tarball is downloaded once, checked against the SHA-256
// pinned here (from the release's SHASUMS256.txt) and cached by checksum.
// It is unpacked with the safe extractor; old versions are kept, because
// launch descriptions name the version directory a session runs with.
const NodeVersion = "24.22.3"

// nodeSHA256 are the checksums of node-v<version>-linux-<arch>.tar.gz.
var nodeSHA256 = map[string]string{
	"amd64": "061ec3e3062115705b56b51fa9cc330ab50d34b6969387d4e4b1a50c6809acc2",
	"arm64": "261af2af250dfd7e9f25cf151d1b2713a0adf8f10a9ad116c4947f68627affe8",
}

// nodeArch maps Go's architecture names to Node's.
var nodeArch = map[string]string{"amd64": "x64", "arm64": "arm64"}

// nodeLimits bound the unpacked release (about 200 MB in some 6,000
// files).
var nodeLimits = extract.Limits{MaxEntries: 50000, MaxFile: 256 << 20, MaxBytes: 1 << 30, MaxInput: 2 << 30}

// NodeDir holds the Node.js installations.
func (t *Toolbox) NodeDir() string { return filepath.Join(t.Dir, "node") }

// NodeRelease returns the download URL and checksum for arch.
func NodeRelease(arch string) (url, sum string, err error) {
	sum, ok := nodeSHA256[arch]
	if !ok {
		return "", "", fmt.Errorf("no Node.js release is pinned for the architecture %s", arch)
	}
	url = fmt.Sprintf("https://nodejs.org/dist/v%s/node-v%s-linux-%s.tar.gz", NodeVersion, NodeVersion, nodeArch[arch])
	return url, sum, nil
}

// InstalledNode returns the installed Node.js version ("" when none).
func (t *Toolbox) InstalledNode() string {
	v, err := os.Readlink(filepath.Join(t.NodeDir(), "current"))
	if err != nil || v == "" || filepath.Base(v) != v || !versionRE.MatchString(v) {
		return ""
	}
	fi, err := os.Stat(filepath.Join(t.NodeDir(), v, "bin", "node"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return v
}

// ComponentBin returns the in-box bin directory of the installed toolbox
// component name (the concrete version directory), its host directory and
// whether it is installed.
func (t *Toolbox) ComponentBin(name string) (inBox, host string, ok bool) {
	switch name {
	case "node":
		v := t.InstalledNode()
		if v == "" {
			return "", "", false
		}
		return path.Join(paths.InBoxTools, "node", v, "bin"), filepath.Join(t.NodeDir(), v), true
	}
	return "", "", false
}

// InstallNode installs the pinned Node.js for the host architecture from
// the download cache (downloading it once) and points current at it.
func (t *Toolbox) InstallNode(ctx context.Context, cache string, get Downloader, out io.Writer) (string, error) {
	url, sum, err := NodeRelease(runtime.GOARCH)
	if err != nil {
		return "", err
	}
	return t.installNode(ctx, NodeVersion, url, sum, cache, get, out)
}

func (t *Toolbox) installNode(ctx context.Context, version, url, sum, cache string, get Downloader, out io.Writer) (string, error) {
	if err := mkdirs(t.Dir, t.NodeDir()); err != nil {
		return "", err
	}
	unlock, err := lock(filepath.Join(t.NodeDir(), ".lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	verDir := filepath.Join(t.NodeDir(), version)
	if _, err := os.Lstat(filepath.Join(verDir, "bin", "node")); err != nil {
		src, err := fetchCached(ctx, cache, url, sum, get, out)
		if err != nil {
			return "", err
		}
		tmp, err := os.MkdirTemp(t.NodeDir(), ".unpack-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		tree := filepath.Join(tmp, "tree")
		if err := os.Mkdir(tree, 0o755); err != nil {
			return "", err
		}
		if _, err := extract.File(src, tree, extract.Options{Limits: nodeLimits, Strip: 1}); err != nil {
			return "", fmt.Errorf("unpacking Node.js: %w", err)
		}
		if fi, err := os.Lstat(filepath.Join(tree, "bin", "node")); err != nil || !fi.Mode().IsRegular() {
			return "", fmt.Errorf("the Node.js release has no bin/node")
		}
		// The toolbox must be world-readable (package comment).
		if err := filepath.WalkDir(tree, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			return os.Chmod(p, 0o755)
		}); err != nil {
			return "", err
		}
		_ = os.RemoveAll(verDir) // a partial directory from an interrupted install
		if err := os.Rename(tree, verDir); err != nil {
			return "", err
		}
	}
	if _, err := symlinkAtomic(version, filepath.Join(t.NodeDir(), "current")); err != nil {
		return "", err
	}
	return filepath.Join("node", version, "bin", "node"), nil
}
