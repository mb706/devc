package toolbox

import (
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The restic of the toolbox (work order C06 section 5), a pinned static
// release used by the restic savepoint driver in the helper container:
//
//	restic/<version>/restic
//	restic/current -> <version>
//
// The release is downloaded once, verified against the SHA-256 pinned here
// (taken from the release's SHA256SUMS) and cached by checksum in the
// download cache, so reinstalling never downloads again.
const ResticVersion = "0.19.1"

// resticSHA256 are the checksums of restic_<version>_linux_<arch>.bz2.
var resticSHA256 = map[string]string{
	"amd64": "f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c",
	"arm64": "a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465",
}

// EnvDownloadCache overrides the download cache directory.
const EnvDownloadCache = "DEVC_DOWNLOAD_CACHE"

// DownloadCacheDir is $DEVC_DOWNLOAD_CACHE, else ~/.cache/devc/downloads.
func DownloadCacheDir(getenv func(string) string, home string) string {
	if d := getenv(EnvDownloadCache); d != "" {
		return d
	}
	return filepath.Join(home, ".cache", "devc", "downloads")
}

// ResticDir holds the restic installations.
func (t *Toolbox) ResticDir() string { return filepath.Join(t.Dir, "restic") }

// ResticRelease returns the download URL and checksum for arch.
func ResticRelease(arch string) (url, sum string, err error) {
	sum, ok := resticSHA256[arch]
	if !ok {
		return "", "", fmt.Errorf("no restic release is pinned for the architecture %s", arch)
	}
	url = fmt.Sprintf("https://github.com/restic/restic/releases/download/v%s/restic_%s_linux_%s.bz2", ResticVersion, ResticVersion, arch)
	return url, sum, nil
}

// InstalledRestic returns the installed restic version ("" when none) and
// the binary's path relative to the toolbox.
func (t *Toolbox) InstalledRestic() (string, string) {
	v, err := os.Readlink(filepath.Join(t.ResticDir(), "current"))
	if err != nil || v == "" || filepath.Base(v) != v {
		return "", ""
	}
	rel := filepath.Join("restic", v, "restic")
	if fi, err := os.Stat(filepath.Join(t.Dir, rel)); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return "", ""
	}
	return v, rel
}

// Downloader fetches url into w.
type Downloader func(ctx context.Context, url string, w io.Writer) error

// HTTPDownload is the production Downloader: HTTPS only, the environment's
// proxy settings, a 10-minute limit.
func HTTPDownload(ctx context.Context, url string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" {
			return errors.New("refusing a redirect away from https")
		}
		if len(via) > 10 {
			return errors.New("too many redirects")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// Releases are a few tens of megabytes; cap the transfer generously.
	_, err = io.Copy(w, io.LimitReader(resp.Body, 512<<20))
	return err
}

// fetchCached returns the path of the file with checksum sum in cache,
// downloading url when it is not there. The file is named by its checksum
// and verified whenever it is used.
func fetchCached(ctx context.Context, cache, url, sum string, get Downloader, out io.Writer) (string, error) {
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return "", fmt.Errorf("creating the download cache: %w", err)
	}
	file := filepath.Join(cache, sum)
	if got, err := fileSHA256(file); err == nil && got == sum {
		return file, nil
	}
	if out != nil {
		fmt.Fprintf(out, "devc: downloading %s\n", url)
	}
	f, err := os.CreateTemp(cache, ".download-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	h := sha256.New()
	err = get(ctx, url, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return "", fmt.Errorf("the download of %s has SHA-256 %s, expected %s: refusing it", url, got, sum)
	}
	if err := os.Rename(tmp, file); err != nil {
		return "", err
	}
	return file, nil
}

func fileSHA256(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// InstallRestic installs the pinned restic for the host architecture from
// the download cache (downloading it once) and points current at it. It
// returns the binary's path relative to the toolbox.
func (t *Toolbox) InstallRestic(ctx context.Context, cache string, get Downloader, out io.Writer) (string, error) {
	url, sum, err := ResticRelease(runtime.GOARCH)
	if err != nil {
		return "", err
	}
	return t.installRestic(ctx, url, sum, cache, get, out)
}

func (t *Toolbox) installRestic(ctx context.Context, url, sum, cache string, get Downloader, out io.Writer) (string, error) {
	if err := mkdirs(t.Dir, t.ResticDir()); err != nil {
		return "", err
	}
	unlock, err := lock(filepath.Join(t.ResticDir(), ".lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	src, err := fetchCached(ctx, cache, url, sum, get, out)
	if err != nil {
		return "", err
	}
	verDir := filepath.Join(t.ResticDir(), ResticVersion)
	if err := mkdirs(verDir); err != nil {
		return "", err
	}
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tmp, err := os.CreateTemp(verDir, ".restic-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	// The compressed file is verified; the decompressed binary is capped
	// all the same.
	if _, err := io.Copy(tmp, io.LimitReader(bzip2.NewReader(f), 512<<20)); err != nil {
		tmp.Close()
		return "", fmt.Errorf("decompressing restic: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	bin := filepath.Join(verDir, "restic")
	if err := os.Rename(tmp.Name(), bin); err != nil {
		return "", err
	}
	if _, err := symlinkAtomic(ResticVersion, filepath.Join(t.ResticDir(), "current")); err != nil {
		return "", err
	}
	return filepath.Join("restic", ResticVersion, "restic"), nil
}
