package toolbox

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRestic is "#!/bin/sh\necho fake restic\n" compressed with bzip2.
const (
	fakeResticBz2 = "QlpoOTFBWSZTWe30Xp4AAALRgAAQaAC7aZwAIAAxTTIxMTEKAeo0zUeoGLDALtAI7HikmQd9/F3JFOFCQ7fReng="
	fakeResticSum = "f097d79ccbe866eb4f9ee84714e90bde64102de7a53d91e2c80c642b4b418d7c"
)

func TestInstallRestic(t *testing.T) {
	tb := New(filepath.Join(t.TempDir(), "toolbox"))
	cache := filepath.Join(t.TempDir(), "downloads")
	blob, _ := base64.StdEncoding.DecodeString(fakeResticBz2)
	calls := 0
	get := func(_ context.Context, url string, w io.Writer) error {
		calls++
		if url != "https://example.org/restic.bz2" {
			t.Errorf("url %q", url)
		}
		_, err := w.Write(blob)
		return err
	}
	if v, _ := tb.InstalledRestic(); v != "" {
		t.Errorf("restic installed before installing: %q", v)
	}
	rel, err := tb.installRestic(context.Background(), "https://example.org/restic.bz2", fakeResticSum, cache, get, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rel != filepath.Join("restic", ResticVersion, "restic") {
		t.Errorf("rel %q", rel)
	}
	b, err := os.ReadFile(filepath.Join(tb.Dir, rel))
	if err != nil || string(b) != "#!/bin/sh\necho fake restic\n" {
		t.Errorf("binary %q %v", b, err)
	}
	if v, r := tb.InstalledRestic(); v != ResticVersion || r != rel {
		t.Errorf("installed %q %q", v, r)
	}
	if _, err := os.Stat(filepath.Join(cache, fakeResticSum)); err != nil {
		t.Errorf("not cached: %v", err)
	}
	// A second installation uses the cache.
	if _, err := tb.installRestic(context.Background(), "https://example.org/restic.bz2", fakeResticSum, cache, get, io.Discard); err != nil || calls != 1 {
		t.Errorf("reinstall: %d downloads, %v", calls, err)
	}
	// A wrong checksum is refused and nothing is cached.
	other := filepath.Join(t.TempDir(), "dl")
	_, err = tb.installRestic(context.Background(), "https://example.org/restic.bz2", strings.Repeat("0", 64), other, get, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("bad checksum: %v", err)
	}
	if ents, _ := os.ReadDir(other); len(ents) != 0 {
		t.Errorf("cache after a bad download: %v", ents)
	}
	// A failed download is an error.
	fail := func(context.Context, string, io.Writer) error { return errors.New("offline") }
	if _, err := tb.installRestic(context.Background(), "https://example.org/restic.bz2", strings.Repeat("1", 64), other, fail, io.Discard); err == nil {
		t.Error("a failed download succeeded")
	}
	if _, _, err := ResticRelease("mips"); err == nil {
		t.Error("an unpinned architecture was accepted")
	}
	if u, s, err := ResticRelease("amd64"); err != nil || !strings.HasSuffix(u, "restic_"+ResticVersion+"_linux_amd64.bz2") || len(s) != 64 {
		t.Errorf("release %q %q %v", u, s, err)
	}
	if got := DownloadCacheDir(func(string) string { return "" }, "/home/u"); got != "/home/u/.cache/devc/downloads" {
		t.Errorf("cache dir %q", got)
	}
}
