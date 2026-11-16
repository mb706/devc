package toolbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/depsrc/depsrctest"
)

func TestInstallNode(t *testing.T) {
	tb := New(filepath.Join(t.TempDir(), "toolbox"))
	cache := filepath.Join(t.TempDir(), "downloads")
	blob := depsrctest.Tarball("node-v9.9.9-linux-x64", map[string]string{"bin/node": "#!/bin/sh\necho node\n", "lib/x.js": "x"}, true)
	sum := sha256.Sum256(blob)
	calls := 0
	get := func(_ context.Context, url string, w io.Writer) error {
		calls++
		_, err := w.Write(blob)
		return err
	}
	if tb.InstalledNode() != "" {
		t.Error("node installed before installing")
	}
	m := &config.Harness{ID: "pi", Install: config.HarnessInstall{Needs: []string{"node"}}}
	if _, _, err := tb.NeedsFor(m); err == nil || !strings.Contains(err.Error(), "devc toolbox install node") {
		t.Errorf("needs without node: %v", err)
	}
	rel, err := tb.installNode(context.Background(), "9.9.9", "https://example.org/node.tar.gz", hex.EncodeToString(sum[:]), cache, get, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// The extractor left out the archive's hostile entries; bin/node is
	// not executable in the fake (mode 0644), so make it so for the check.
	if rel != filepath.Join("node", "9.9.9", "bin", "node") {
		t.Errorf("rel %q", rel)
	}
	if _, err := os.Lstat(filepath.Join(tb.NodeDir(), "9.9.9", "escape")); err == nil {
		t.Error("an escaping link was installed")
	}
	os.Chmod(filepath.Join(tb.Dir, rel), 0o755)
	if tb.InstalledNode() != "9.9.9" {
		t.Errorf("installed %q", tb.InstalledNode())
	}
	mounts, env, err := tb.NeedsFor(m)
	if err != nil || len(mounts) != 1 || mounts[0].Target != "/opt/devc/node/9.9.9" || !mounts[0].ReadOnly ||
		mounts[0].Source != filepath.Join(tb.NodeDir(), "9.9.9") || env[0] != "PATH=/opt/devc/node/9.9.9/bin:"+SystemPATH {
		t.Errorf("needs: %+v %v %v", mounts, env, err)
	}
	if dirs, missing := tb.NeedsPath(m); missing != "" || len(dirs) != 1 || dirs[0] != "/opt/devc/node/9.9.9/bin" {
		t.Errorf("needs path: %v %q", dirs, missing)
	}
	// Installed already: nothing downloaded again.
	if _, err := tb.installNode(context.Background(), "9.9.9", "https://example.org/node.tar.gz", hex.EncodeToString(sum[:]), cache, get, io.Discard); err != nil || calls != 1 {
		t.Errorf("reinstall: %d downloads, %v", calls, err)
	}
	if u, s, err := NodeRelease("amd64"); err != nil || !strings.HasSuffix(u, "/v"+NodeVersion+"/node-v"+NodeVersion+"-linux-x64.tar.gz") || len(s) != 64 {
		t.Errorf("release %q %q %v", u, s, err)
	}
	if _, _, err := NodeRelease("riscv64"); err == nil {
		t.Error("an unpinned architecture was accepted")
	}
}

func TestSyncSkills(t *testing.T) {
	tb := New(filepath.Join(t.TempDir(), "toolbox"))
	os.MkdirAll(filepath.Join(tb.SkillsDir(), "stale"), 0o755)
	changed, err := tb.syncSkills()
	if err != nil || !changed {
		t.Fatalf("sync: %v %v", changed, err)
	}
	b, err := os.ReadFile(filepath.Join(tb.SkillsDir(), "dep-source", "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "name: dep-source") || !strings.Contains(string(b), "devc src") {
		t.Errorf("dep-source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tb.SkillsDir(), "stale")); err == nil {
		t.Error("a stale skill was kept")
	}
	if changed, err := tb.syncSkills(); err != nil || changed {
		t.Errorf("second sync: %v %v", changed, err)
	}
}
