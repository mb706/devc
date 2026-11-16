package boxside

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/safefs"
)

// The context worker keeps a realm's context repository (work order C04
// section 6). It runs in a throwaway container from the installer image
// (which has git) with no network, the realm's context directory at
// ContextMount and the toolbox read-only. The working tree is box-written,
// so git never looks at it: the worker walks it itself through safefs (no
// symlink is ever followed), and hands the regular files it accepts to
// git fast-import as one commit whose tree is exactly those files. The
// repository is bare, at ContextMount/.git, which no box ever sees.
const (
	ContextMount = "/ctx"
	contextGit   = ContextMount + "/.git"
	// pendingRef holds a freshly imported tree until it is compared with
	// the branch.
	pendingRef = "refs/devc/pending"
	mainRef    = "refs/heads/main"
	emptyTree  = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// Limits of one context snapshot. A tree beyond them is refused rather
// than committed in part, because a partial tree would record deletions.
const (
	contextMaxFiles     = 50000
	contextMaxFileSize  = 10 << 20
	contextMaxTotal     = 512 << 20
	contextMaxDirEntry  = 100000
	contextMaxPathBytes = 4096
)

// RequireContextWorker refuses to run the worker anywhere but in its
// container: DEVC_CONTEXT_WORKER=1, the context mount, and not in a box.
func RequireContextWorker(getenv func(string) string, root string) error {
	if getenv("DEVC_CONTEXT_WORKER") != "1" {
		return errors.New("this command runs only in devc's context container (DEVC_CONTEXT_WORKER is not 1)")
	}
	if _, err := os.Stat(filepath.Join(root, InfoFile)); err == nil {
		return errors.New("this command does not run in a box")
	}
	if fi, err := os.Lstat(filepath.Join(root, ContextMount)); err != nil || !fi.IsDir() {
		return fmt.Errorf("this command runs only in devc's context container (%s is not a directory)", ContextMount)
	}
	return nil
}

// ContextWorker runs the git operations on the context repository.
type ContextWorker struct {
	Dir    string // ContextMount
	Git    string // the git executable
	Stdout io.Writer
	Stderr io.Writer
	Now    func() time.Time
}

func (w *ContextWorker) gitDir() string { return filepath.Join(w.Dir, ".git") }

// git runs git against the bare repository with a configuration that
// neither the box nor the user's global files influence.
func (w *ContextWorker) git(stdin io.Reader, stdout io.Writer, args ...string) error {
	full := append([]string{
		"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "core.pager=cat",
		"-c", "color.ui=false", "-c", "core.quotePath=true", "-c", "core.fsmonitor=false",
	}, args...)
	cmd := exec.Command(w.Git, full...)
	cmd.Env = []string{"GIT_DIR=" + w.gitDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1",
		"HOME=/tmp", "PATH=/usr/local/bin:/usr/bin:/bin", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	cmd.Dir = "/"
	cmd.Stdin = stdin
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return nil
}

func (w *ContextWorker) gitOut(args ...string) (string, error) {
	var out bytes.Buffer
	err := w.git(nil, &out, args...)
	return strings.TrimSpace(out.String()), err
}

// ensureRepo creates the bare repository on first use.
func (w *ContextWorker) ensureRepo() error {
	fi, err := os.Lstat(w.gitDir())
	if err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", w.gitDir())
		}
		return nil
	}
	cmd := exec.Command(w.Git, "init", "--bare", "--template=", "-q", "--initial-branch=main", w.gitDir())
	cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "HOME=/tmp", "PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}
	cmd.Dir = "/"
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, kv := range [][2]string{{"gc.auto", "0"}, {"core.logAllRefUpdates", "always"}} {
		if err := w.git(nil, io.Discard, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// revParse returns the object a revision names, or "" when it does not
// exist.
func (w *ContextWorker) revParse(rev string) string {
	out, err := w.gitOut("rev-parse", "--verify", "-q", rev)
	if err != nil {
		return ""
	}
	return out
}

// ContextFile is a regular file accepted into a snapshot.
type ContextFile struct {
	Path string // relative to the context directory, slash-separated
	Exec bool
}

// ContextSkip is an entry left out of a snapshot, and why.
type ContextSkip struct {
	Path   string
	Reason string
}

// IsDotGit reports whether a path component would be git metadata on some
// filesystem: ".git" in any case, with trailing dots or spaces (NTFS), the
// short name "git~1", or with characters HFS+ ignores.
func IsDotGit(name string) bool {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
			continue
		}
		b.WriteRune(r)
	}
	n := strings.ToLower(strings.TrimRight(b.String(), ". "))
	return n == ".git" || n == "git~1"
}

// ScanContext walks the context directory below root and returns the
// regular files to commit and what it left out: nested git metadata (the
// ".git" entry only, so a box cannot hide its siblings by planting one),
// symlinks, special files, unreadable directories and files too large.
func ScanContext(root *safefs.Root) ([]ContextFile, []ContextSkip, error) {
	var files []ContextFile
	var skips []ContextSkip
	var total int64
	var walk func(rel string) error
	walk = func(rel string) error {
		ents, err := root.List(rel, contextMaxDirEntry)
		if errors.Is(err, safefs.ErrTooLarge) {
			return fmt.Errorf("%s has more than %d entries", rel, contextMaxDirEntry)
		}
		if err != nil {
			skips = append(skips, ContextSkip{rel, "unreadable directory"})
			return nil
		}
		for _, e := range ents {
			p := e.Name
			if rel != "." {
				p = rel + "/" + e.Name
			}
			switch {
			case rel == "." && e.Name == ".git":
				continue // the repository itself
			case IsDotGit(e.Name):
				skips = append(skips, ContextSkip{p, "nested git metadata"})
			case len(p) > contextMaxPathBytes:
				skips = append(skips, ContextSkip{p[:200] + "…", "path too long"})
			case e.IsDir():
				if err := walk(p); err != nil {
					return err
				}
			case e.Type&os.ModeSymlink != 0:
				skips = append(skips, ContextSkip{p, "symlink"})
			case !e.IsRegular():
				skips = append(skips, ContextSkip{p, "not a regular file"})
			default:
				f, err := root.Open(p)
				if err != nil {
					skips = append(skips, ContextSkip{p, "unreadable"})
					continue
				}
				fi, err := f.Stat()
				f.Close()
				switch {
				case err != nil:
					skips = append(skips, ContextSkip{p, "unreadable"})
					continue
				case fi.Size() > contextMaxFileSize:
					skips = append(skips, ContextSkip{p, fmt.Sprintf("larger than %d MiB", contextMaxFileSize>>20)})
					continue
				}
				total += fi.Size()
				files = append(files, ContextFile{Path: p, Exec: fi.Mode().Perm()&0o111 != 0})
				if len(files) > contextMaxFiles || total > contextMaxTotal {
					return fmt.Errorf("the context holds more than %d files or %d MiB; refusing to snapshot it", contextMaxFiles, contextMaxTotal>>20)
				}
			}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, nil, err
	}
	return files, skips, nil
}

// QuotePath quotes a path for git fast-import (C style), escaping
// backslashes, double quotes and every control byte, so any file name the
// box chose is passed as data.
func QuotePath(p string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// snapshot imports the current tree into pendingRef, on top of main when
// it exists, and returns main's commit ("" when there is none).
func (w *ContextWorker) snapshot(message string) (string, []ContextSkip, error) {
	if err := w.ensureRepo(); err != nil {
		return "", nil, err
	}
	root, err := safefs.OpenRoot(w.Dir)
	if err != nil {
		return "", nil, err
	}
	defer root.Close()
	files, skips, err := ScanContext(root)
	if err != nil {
		return "", nil, err
	}
	main := w.revParse(mainRef + "^{commit}")
	_ = w.git(nil, io.Discard, "update-ref", "-d", pendingRef)

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- w.git(pr, io.Discard, "fast-import", "--quiet", "--done") }()
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	bw := bufio.NewWriter(pw)
	werr := func() error {
		fmt.Fprintf(bw, "reset %s\ncommit %s\ncommitter devc <devc@localhost> %d +0000\ndata %d\n%s\n", pendingRef, pendingRef, now().Unix(), len(message), message)
		if main != "" {
			fmt.Fprintf(bw, "from %s\n", main)
		}
		bw.WriteString("deleteall\n")
		for _, f := range files {
			data, err := readContextFile(root, f.Path)
			if err != nil {
				skips = append(skips, ContextSkip{f.Path, "changed while reading"})
				continue
			}
			mode := "100644"
			if f.Exec {
				mode = "100755"
			}
			fmt.Fprintf(bw, "M %s inline %s\ndata %d\n", mode, QuotePath(f.Path), len(data))
			bw.Write(data)
			bw.WriteByte('\n')
		}
		bw.WriteString("done\n")
		return bw.Flush()
	}()
	pw.CloseWithError(werr)
	if err := <-done; err != nil {
		return "", skips, err
	}
	if werr != nil {
		return "", skips, werr
	}
	return main, skips, nil
}

// readContextFile reads a file found by the scan again, through safefs,
// refusing anything that is no longer a small regular file.
func readContextFile(root *safefs.Root, rel string) ([]byte, error) {
	return root.ReadFile(rel, contextMaxFileSize)
}

func (w *ContextWorker) reportSkips(skips []ContextSkip) {
	for _, s := range skips {
		fmt.Fprintf(w.Stderr, "skipped %s: %s\n", strconv.Quote(s.Path), s.Reason)
	}
}

// Commit records the context directory as a new commit on main when it
// differs from main's tree.
func (w *ContextWorker) Commit(message string) error {
	main, skips, err := w.snapshot(message)
	w.reportSkips(skips)
	if err != nil {
		return err
	}
	defer w.git(nil, io.Discard, "update-ref", "-d", pendingRef)
	pending := w.revParse(pendingRef + "^{commit}")
	if pending == "" {
		return errors.New("the snapshot produced no commit")
	}
	tree := w.revParse(pending + "^{tree}")
	if (main != "" && w.revParse(main+"^{tree}") == tree) || (main == "" && tree == emptyTree) {
		fmt.Fprintln(w.Stdout, "nothing to commit")
		return nil
	}
	old := main
	if old == "" {
		old = strings.Repeat("0", len(pending))
	}
	if err := w.git(nil, io.Discard, "update-ref", "-m", "devc context commit", mainRef, pending, old); err != nil {
		return err
	}
	fmt.Fprintf(w.Stdout, "committed %s\n", pending[:12])
	return nil
}

// Diff shows what a commit would record: a summary, then the patch.
func (w *ContextWorker) Diff() error {
	main, skips, err := w.snapshot("uncommitted changes")
	w.reportSkips(skips)
	if err != nil {
		return err
	}
	defer w.git(nil, io.Discard, "update-ref", "-d", pendingRef)
	base := main
	if base == "" {
		base = emptyTree
	}
	for _, extra := range [][]string{{"--stat"}, {}} {
		args := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv"}, extra...)
		if err := w.git(nil, w.Stdout, append(args, base, pendingRef)...); err != nil {
			return err
		}
	}
	return nil
}

// Log shows the last n commits with the files they changed.
func (w *ContextWorker) Log(n int) error {
	if err := w.ensureRepo(); err != nil {
		return err
	}
	if w.revParse(mainRef+"^{commit}") == "" {
		fmt.Fprintln(w.Stdout, "no commits yet")
		return nil
	}
	return w.git(nil, w.Stdout, "log", "--no-color", "--stat", "--date=iso", "-n", strconv.Itoa(n), mainRef)
}
