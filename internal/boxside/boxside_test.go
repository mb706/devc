package boxside

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRewritePasswd(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no uid 1000",
			"root:x:0:0:root:/root:/bin/bash\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n",
			"root:x:0:0:root:/root:/bin/bash\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n" +
				"dev:x:1000:1000:dev:/home/dev:/bin/bash\nvault:x:2000:2000:vault:/nonexistent:/usr/sbin/nologin\n"},
		{"other name at 1000, kept in place",
			"root:x:0:0:root:/root:/bin/sh\nnode:x:1000:1000::/home/node:/bin/sh\n# comment\n+@nis\n",
			"root:x:0:0:root:/root:/bin/sh\ndev:x:1000:1000:dev:/home/dev:/bin/bash\n# comment\n+@nis\n" +
				"vault:x:2000:2000:vault:/nonexistent:/usr/sbin/nologin\n"},
		{"name dev elsewhere and a second 1000",
			"dev:x:1001:1001::/home/dev:/bin/sh\nuser:x:1000:1000::/home/user:/bin/sh\nsvc:x:2000:2000::/srv:/bin/false\n",
			"dev:x:1000:1000:dev:/home/dev:/bin/bash\nvault:x:2000:2000:vault:/nonexistent:/usr/sbin/nologin\n"},
		{"no trailing newline", "root:x:0:0:root:/root:/bin/sh",
			"root:x:0:0:root:/root:/bin/sh\ndev:x:1000:1000:dev:/home/dev:/bin/bash\nvault:x:2000:2000:vault:/nonexistent:/usr/sbin/nologin\n"},
		{"empty", "", "dev:x:1000:1000:dev:/home/dev:/bin/bash\nvault:x:2000:2000:vault:/nonexistent:/usr/sbin/nologin\n"},
	}
	for _, tc := range cases {
		got := string(RewritePasswd([]byte(tc.in), "/bin/bash", "/usr/sbin/nologin"))
		if got != tc.want {
			t.Errorf("%s:\ngot\n%s\nwant\n%s", tc.name, got, tc.want)
		}
		if again := string(RewritePasswd([]byte(got), "/bin/bash", "/usr/sbin/nologin")); again != got {
			t.Errorf("%s: not idempotent:\n%s", tc.name, again)
		}
	}
}

func TestRewriteGroup(t *testing.T) {
	in := "root:x:0:\nnode:x:1000:node,other\nusers:x:100:\n"
	want := "root:x:0:\ndev:x:1000:node,other\nusers:x:100:\nvault:x:2000:\n"
	got := string(RewriteGroup([]byte(in)))
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if string(RewriteGroup([]byte(got))) != got {
		t.Error("not idempotent")
	}
}

func TestFixIdentityFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.MkdirAll(filepath.Join(root, "usr", "sbin"), 0o755)
	os.WriteFile(filepath.Join(root, "usr", "sbin", "nologin"), nil, 0o755)
	os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o644)
	os.WriteFile(filepath.Join(root, "etc", "group"), []byte("root:x:0:\n"), 0o644)
	changed, err := FixIdentity(root, "/bin/sh")
	if err != nil || !changed {
		t.Fatalf("FixIdentity = %v, %v", changed, err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "etc", "passwd"))
	if !strings.Contains(string(b), "dev:x:1000:1000:dev:/home/dev:/bin/sh\n") || !strings.Contains(string(b), "vault:x:2000:2000:vault:/nonexistent:/usr/sbin/nologin") {
		t.Errorf("passwd:\n%s", b)
	}
	if fi, _ := os.Stat(filepath.Join(root, "etc", "passwd")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode())
	}
	changed, err = FixIdentity(root, "/bin/sh")
	if err != nil || changed {
		t.Errorf("second run = %v, %v", changed, err)
	}
	if des, _ := os.ReadDir(filepath.Join(root, "etc")); len(des) != 2 {
		t.Errorf("leftover files in etc: %v", des)
	}
}

func TestChooseShell(t *testing.T) {
	root := t.TempDir()
	mk := func(p string, mode os.FileMode) {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), nil, mode)
	}
	mk("/bin/sh", 0o755)
	if got := ChooseShell(root, ""); got != "/bin/sh" {
		t.Errorf("only sh: %s", got)
	}
	mk("/usr/bin/bash", 0o755)
	mk("/usr/bin/zsh", 0o644) // not executable
	if got := ChooseShell(root, ""); got != "/usr/bin/bash" {
		t.Errorf("bash: %s", got)
	}
	mk("/usr/local/bin/fish", 0o755)
	if got := ChooseShell(root, "fish"); got != "/usr/local/bin/fish" {
		t.Errorf("configured fish: %s", got)
	}
	if got := ChooseShell(root, "/usr/local/bin/fish"); got != "/usr/local/bin/fish" {
		t.Errorf("configured absolute: %s", got)
	}
	if got := ChooseShell(root, "nushell"); got != "/usr/bin/bash" {
		t.Errorf("missing configured shell: %s", got)
	}
	if got := ChooseShell(t.TempDir(), ""); got != "/bin/sh" {
		t.Errorf("empty root: %s", got)
	}
}

func TestPrepareHome(t *testing.T) {
	base := t.TempDir()
	home, dots, skel, run := filepath.Join(base, "home"), filepath.Join(base, "dotfiles"), filepath.Join(base, "skel"), filepath.Join(base, "run")
	for _, d := range []string{filepath.Join(home, ".cache"), dots, filepath.Join(skel, ".config"), run} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(dots, ".bashrc"), []byte("dot"), 0o644)
	os.WriteFile(filepath.Join(dots, ".gitconfig"), []byte("dot"), 0o644)
	os.WriteFile(filepath.Join(skel, ".bashrc"), []byte("skel"), 0o644)
	os.WriteFile(filepath.Join(skel, ".profile"), []byte("skel"), 0o644)
	os.WriteFile(filepath.Join(skel, ".config", "x"), []byte("skel"), 0o600)
	o := InitOptions{Home: home, Dotfiles: dots, Skel: skel, Run: run, MountPoints: []string{".cache"}}

	if errs := PrepareHome(o); len(errs) > 0 {
		t.Fatal(errs)
	}
	if l, err := os.Readlink(filepath.Join(home, ".bashrc")); err != nil || l != filepath.Join(dots, ".bashrc") {
		t.Errorf(".bashrc -> %q, %v (dotfile must win over skel)", l, err)
	}
	for _, p := range []string{".profile", ".config/x", ".local/bin"} {
		if _, err := os.Lstat(filepath.Join(home, p)); err != nil {
			t.Errorf("%s missing: %v", p, err)
		}
	}

	// A used home gets no skeleton, the user's own file is kept, removed
	// dotfiles lose their links.
	os.Remove(filepath.Join(home, ".profile"))
	os.Remove(filepath.Join(home, ".gitconfig"))
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("mine"), 0o644)
	os.Remove(filepath.Join(dots, ".bashrc"))
	os.WriteFile(filepath.Join(dots, ".zshrc"), []byte("dot"), 0o644)
	if errs := PrepareHome(o); len(errs) > 0 {
		t.Fatal(errs)
	}
	if _, err := os.Lstat(filepath.Join(home, ".profile")); err == nil {
		t.Error("skeleton copied into a used home")
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".gitconfig")); string(b) != "mine" {
		t.Error("the user's own file was replaced")
	}
	if _, err := os.Lstat(filepath.Join(home, ".bashrc")); err == nil {
		t.Error("stale dotfile link kept")
	}
	if _, err := os.Readlink(filepath.Join(home, ".zshrc")); err != nil {
		t.Error("new dotfile not linked")
	}
}

func TestInitWritesReadyAndWaits(t *testing.T) {
	base := t.TempDir()
	o := InitOptions{Home: filepath.Join(base, "home"), Dotfiles: filepath.Join(base, "none"), Run: filepath.Join(base, "run")}
	os.MkdirAll(o.Home, 0o700)
	os.MkdirAll(o.Run, 0o700)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Init(ctx, o) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(o.Run, ReadyFile)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ready file not written")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fi, err := os.Stat(filepath.Join(o.Run, SpoolDirName)); err != nil || !fi.IsDir() {
		t.Errorf("spool: %v", err)
	}
	select {
	case <-done:
		t.Fatal("Init returned before it was told to stop")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Init = %v", err)
	}
}

func TestReadInfo(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, EtcDir), 0o755)
	os.WriteFile(filepath.Join(root, InfoFile), []byte(`{"schema":1,"realm":"demo","project":"hello","project_dir":"/w/demo/hello"}`), 0o644)
	in, err := ReadInfo(root)
	if err != nil || in.ProjectDir != "/w/demo/hello" {
		t.Errorf("ReadInfo = %+v, %v", in, err)
	}
	os.WriteFile(filepath.Join(root, InfoFile), []byte(`{"schema":2}`), 0o644)
	if _, err := ReadInfo(root); err == nil {
		t.Error("unknown schema accepted")
	}
}
