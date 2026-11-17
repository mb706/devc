//go:build integration

// This file installs the fake harness's manifests and the static tmux into
// a test environment.

package itest

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FakeAgent is the path of bin/fakeagent, built by ./dev itest.
func FakeAgent(t *testing.T) string {
	t.Helper()
	p := os.Getenv("DEVC_FAKEAGENT")
	if p == "" {
		t.Fatal("DEVC_FAKEAGENT is not set; run the integration tests with ./dev itest")
	}
	return p
}

// InstallFakeManifests copies the fake harness manifests and templates
// into the configuration, pointing install.path at bin/fakeagent.
func (e *Env) InstallFakeManifests() {
	e.t.Helper()
	src := filepath.Join(e.Repo, "test", "fakeharness", "manifests")
	des, err := os.ReadDir(src)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, de := range des {
		b, err := os.ReadFile(filepath.Join(src, de.Name()))
		if err != nil {
			e.t.Fatal(err)
		}
		e.WriteConfig(filepath.Join("harness", de.Name()), strings.ReplaceAll(string(b), "@FAKEAGENT@", FakeAgent(e.t)))
	}
}

// CacheDir is where tests keep what they may reuse between runs.
func CacheDir() string {
	if d := os.Getenv("DEVC_ITEST_CACHE"); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "devc-dev", "itest")
	}
	return filepath.Join(os.TempDir(), "devc-itest-cache")
}

type toolboxInfo struct {
	Tmux       string `json:"tmux"`
	TmuxPinned string `json:"tmux_pinned"`
	TmuxRecipe string `json:"tmux_recipe"`
}

// EnsureTmux puts the pinned static tmux into the environment's toolbox.
// A build made by 'devc toolbox build tmux' in an earlier run is reused
// from the cache; otherwise (or with DEVC_TEST_REBUILD_TMUX=1) the command
// runs and its result is cached. It returns whether it built.
func EnsureTmux(t *testing.T, e *Env) bool {
	t.Helper()
	e.MustRun("toolbox", "sync")
	var info toolboxInfo
	res := e.MustRun("toolbox", "ls", "--json")
	if err := json.Unmarshal([]byte(res.Stdout), &info); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(CacheDir(), "tmux-"+info.TmuxPinned+"-"+info.TmuxRecipe)
	toolbox := filepath.Join(e.StateDir, "toolbox")
	if os.Getenv("DEVC_TEST_REBUILD_TMUX") != "1" {
		if _, err := os.Stat(filepath.Join(cache, "complete")); err == nil {
			for _, d := range []string{"tmux", "terminfo"} {
				if err := copyTreeLinks(filepath.Join(cache, d), filepath.Join(toolbox, d)); err != nil {
					t.Fatal(err)
				}
			}
			_ = os.Remove(filepath.Join(toolbox, "bin", "tmux"))
			if err := os.Symlink("../tmux/current/tmux", filepath.Join(toolbox, "bin", "tmux")); err != nil {
				t.Fatal(err)
			}
			res := e.MustRun("toolbox", "ls", "--json")
			if json.Unmarshal([]byte(res.Stdout), &info) == nil && info.Tmux == info.TmuxPinned {
				return false
			}
			t.Logf("cached tmux in %s is not usable; rebuilding", cache)
		}
	}
	old := e.Timeout
	e.Timeout = 30 * time.Minute
	e.MustRun("toolbox", "build", "tmux")
	e.Timeout = old
	tmp := cache + ".new"
	_ = os.RemoveAll(tmp)
	for _, d := range []string{"tmux", "terminfo"} {
		if err := copyTreeLinks(filepath.Join(toolbox, d), filepath.Join(tmp, d)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(cache)
	if err := os.Rename(tmp, cache); err != nil {
		t.Fatal(err)
	}
	return true
}

// copyTreeLinks copies a directory tree, recreating symlinks as symlinks.
func copyTreeLinks(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && rel != "." && (d.Name() == ".lock" || strings.HasPrefix(d.Name(), ".build")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, fi.Mode().Perm())
		}
		return fmt.Errorf("unexpected file type at %s", p)
	})
}
