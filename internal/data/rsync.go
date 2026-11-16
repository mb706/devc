// This file builds rsync's command line for data pull and push and reads
// the listing of a dry run.

package data

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/proto"
)

// Rsync describes one rsync run of `devc data pull` or `push` (work order
// C14). RsyncArgs is the only place that turns it into a command line.
type Rsync struct {
	// Push sends Local to the remote; otherwise the remote's paths are
	// fetched into Local.
	Push   bool
	Remote config.Remote
	// Local is the host staging directory: the destination of a pull (a
	// directory under the state directory), the source of a push (the
	// vault-owned push staging directory). Never the data root.
	Local string
	// Paths are relative to the data root on both sides.
	Paths []string
	// DryRun lists what would be transferred, one ItemFormat line each.
	DryRun bool
	// BackupDir (push only) is where the remote keeps what the push
	// overwrites, relative to the remote's data path.
	BackupDir string
	// Shell is the remote shell as words (gate.Client.RsyncShell); nil
	// leaves rsync's default.
	Shell []string
	// Progress shows rsync's overall progress (for a terminal).
	Progress bool
}

// ItemFormat is the --out-format of dry runs: the itemized change, the
// length and the name (which rsync escapes when it is not printable).
const ItemFormat = "%i %l %n"

var (
	backupDirRE = regexp.MustCompile(`^\` + boxside.BackupDirName + `/[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}$`)
	backupIDRE  = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// BackupDir is the remote backup directory of a push stamped stamp with
// the operation ID id (12 hex digits): every push has its own, so two
// pushes in the same second never overwrite each other's backups
// (finding D2-10).
func BackupDir(stamp, id string) string { return boxside.BackupDirName + "/" + stamp + "-" + id }

// RsyncArgs builds rsync's arguments. Fixed choices, for every case:
//
//   - never --delete: nothing is ever removed on the receiving side;
//   - --partial, so an interrupted transfer keeps what it received;
//   - symlinks are copied as links and never followed (--links, no
//     --copy-links), devices, special files, owners and groups are not
//     transferred;
//   - --relative with "<base>/./<path>", so every path keeps its place
//     below the data root on the other side;
//   - --protect-args: the remote shell never parses a path (rsync expands
//     wildcards itself, which is why paths may not contain any);
//   - a pull keeps the remote's permission bits (the helper container
//     strips set-ID bits when it moves the files in); a push sends no
//     directory permissions (the staging directories are made readable for
//     the host) and files keep theirs, limited by the remote's umask;
//   - a push keeps what it overwrites in BackupDir on the remote.
//
// Every operand starts with "/" or with a validated host alias and ":",
// so none can pass for an option; remote paths and aliases come from the
// configuration (config.CheckRemote), never from a box.
func RsyncArgs(r Rsync) ([]string, error) {
	if err := config.CheckRemote(r.Remote); err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	if !filepath.IsAbs(r.Local) || filepath.Clean(r.Local) != r.Local || strings.ContainsAny(r.Local, "\x00\n\r") {
		return nil, fmt.Errorf("the staging directory %q must be a clean absolute path", r.Local)
	}
	if len(r.Paths) == 0 {
		return nil, errors.New("no paths to transfer")
	}
	for _, p := range r.Paths {
		if err := boxside.CheckSyncPath(p); err != nil {
			return nil, err
		}
	}
	switch {
	case r.Push && !backupDirRE.MatchString(r.BackupDir):
		return nil, fmt.Errorf("invalid backup directory %q", r.BackupDir)
	case !r.Push && r.BackupDir != "":
		return nil, errors.New("a pull has no backup directory")
	}
	args := []string{"--recursive", "--links", "--times", "--no-owner", "--no-group", "--no-devices", "--no-specials",
		"--partial", "--relative", "--protect-args"}
	if r.Push {
		args = append(args, "--no-perms", "--executability", "--chmod=D0777", "--backup", "--backup-dir="+r.BackupDir)
	} else {
		args = append(args, "--perms")
	}
	if r.DryRun {
		args = append(args, "--dry-run", "--out-format="+ItemFormat)
	} else if r.Progress {
		args = append(args, "--info=progress2")
	}
	if len(r.Shell) > 0 {
		sh, err := rshCommand(r.Shell)
		if err != nil {
			return nil, err
		}
		args = append(args, "--rsh="+sh)
	}
	base := strings.TrimSuffix(r.Remote.Path, "/")
	remote := func(p string) string {
		if r.Remote.Host == "" {
			return p
		}
		return r.Remote.Host + ":" + p
	}
	if r.Push {
		for _, p := range r.Paths {
			args = append(args, r.Local+"/./"+p)
		}
		return append(args, remote(base+"/")), nil
	}
	for _, p := range r.Paths {
		args = append(args, remote(base+"/./"+p))
	}
	return append(args, r.Local+"/"), nil
}

// rshCommand joins the remote shell's words for --rsh, which rsync splits
// at spaces itself; words with blanks or quotes are single-quoted (rsync
// reads a doubled quote inside quotes as one).
func rshCommand(words []string) (string, error) {
	out := make([]string, 0, len(words))
	for _, w := range words {
		switch {
		case w == "" || strings.ContainsAny(w, "\x00\n\r"):
			return "", fmt.Errorf("invalid remote shell word %q", w)
		case strings.ContainsAny(w, " \t'\""):
			out = append(out, "'"+strings.ReplaceAll(w, "'", "''")+"'")
		default:
			out = append(out, w)
		}
	}
	return strings.Join(out, " "), nil
}

// Item is one line of a dry run.
type Item struct {
	// Change is rsync's itemized change (%i), such as ">f+++++++++".
	Change string `json:"change"`
	Size   int64  `json:"size"`
	// Name is the path (directories end in "/"), made printable.
	Name string `json:"name"`
}

// File reports whether the item transfers a regular file.
func (it Item) File() bool {
	return len(it.Change) >= 2 && (it.Change[0] == '<' || it.Change[0] == '>') && it.Change[1] == 'f'
}

// New reports whether the item does not exist on the receiving side yet.
func (it Item) New() bool { return strings.HasSuffix(it.Change, "+++++++++") }

// itemRE is rsync's itemized change (%i, protocol 30 and later): the
// update type, the file type and nine attribute flags.
var itemRE = regexp.MustCompile(`^[<>ch.*][fdLDS][a-zA-Z.+ ?]{9}$`)

// infoLines are the only other lines rsync prints on standard output in a
// dry run with ItemFormat.
var infoLines = []string{"created directory ", "skipping non-regular file "}

// ErrPreview means a dry run's output could not be read completely, so
// what a transfer would do is unknown (finding D2-06).
var ErrPreview = errors.New("the preview of the transfer cannot be read completely")

// ParseItems parses the ItemFormat lines of a dry run. A line that is
// neither an item nor one of rsync's known messages, or an item whose size
// does not parse, is an error: a preview that cannot be read whole cannot
// be counted against a limit.
func ParseItems(out []byte) ([]Item, error) {
	var items []Item
	for i, line := range strings.Split(string(out), "\n") {
		if line == "" || slices.ContainsFunc(infoLines, func(p string) bool { return strings.HasPrefix(line, p) }) {
			continue
		}
		// The change code may hold spaces; it is always 11 characters.
		var f []string
		var n int64
		err := errors.New("not an item")
		if len(line) > 12 && line[11] == ' ' && itemRE.MatchString(line[:11]) {
			if f = strings.SplitN(line[12:], " ", 2); len(f) == 2 {
				n, err = strconv.ParseInt(f[0], 10, 64)
			}
		}
		if err != nil || len(f) != 2 || n < 0 || f[1] == "" {
			return nil, fmt.Errorf("%w: line %d is not one rsync prints (%.80q)", ErrPreview, i+1, proto.Printable(line))
		}
		items = append(items, Item{Change: line[:11], Size: n, Name: proto.Printable(f[1])})
	}
	return items, nil
}

// Totals counts the files of a dry run and their bytes; a total beyond
// what an int64 holds is an error, never a wrapped number.
func Totals(items []Item) (files, size, updated int64, err error) {
	for _, it := range items {
		if it.File() {
			if it.Size > math.MaxInt64-size {
				return 0, 0, 0, fmt.Errorf("%w: the sizes add up to more than %d bytes", ErrPreview, int64(math.MaxInt64))
			}
			files++
			size += it.Size
			if !it.New() {
				updated++
			}
		}
	}
	return files, size, updated, nil
}
