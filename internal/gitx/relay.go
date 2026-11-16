// This file implements the relay repository: creation, the configuration
// that keeps everything it ever received, ref listings and update-ref
// transactions with the old value checked.

package gitx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mb706/devc/internal/execx"
)

// Relay is one project's relay repository.
type Relay struct {
	Host *Host
	Dir  string
}

// relayConfig is applied to every relay (work order section 2): git never
// collects garbage or runs maintenance on its own, every ref change is
// logged, and every object that arrives is checked.
var relayConfig = [][2]string{
	{"core.bare", "true"},
	{"gc.auto", "0"},
	{"maintenance.auto", "false"},
	{"core.logAllRefUpdates", "always"},
	{"fetch.fsckObjects", "true"},
	{"receive.fsckObjects", "true"},
	{"transfer.fsckObjects", "true"},
	{"fetch.recurseSubmodules", "false"},
	// Incoming objects stay in the pack they came in (finding D1-05): a
	// small pack of deltas cannot expand into loose objects many times
	// its size. Bases a thin pack refers to are added once.
	{"transfer.unpackLimit", "1"},
	{"receive.unpackLimit", "1"},
	{"fetch.unpackLimit", "1"},
}

func (r *Relay) run(ctx context.Context, args ...string) (execx.Result, error) {
	return r.Host.Run(ctx, Call{Relay: r.Dir, Args: args})
}

// out runs an internal command and returns its trimmed standard output.
func (r *Relay) out(ctx context.Context, args ...string) (string, error) {
	res, err := r.run(ctx, args...)
	return strings.TrimSpace(string(res.Stdout)), err
}

// Exists reports whether the relay has been created.
func (r *Relay) Exists() bool {
	fi, err := os.Lstat(filepath.Join(r.Dir, "HEAD"))
	return err == nil && fi.Mode().IsRegular()
}

// Create makes the relay if it does not exist: the repository is set up in
// a temporary directory beside it and renamed into place, so a relay that
// exists is always fully configured. It reports whether it created one.
func (r *Relay) Create(ctx context.Context) (bool, error) {
	if r.Exists() {
		return false, r.Configure(ctx)
	}
	if _, err := os.Lstat(r.Dir); err == nil {
		return false, fmt.Errorf("%s exists but is not a git repository: move it away and run 'devc project init' again", r.Dir)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return false, err
	}
	tmp := &Relay{Host: r.Host, Dir: r.Dir + ".tmp-" + hex.EncodeToString(b)}
	cleanup := func() { _ = os.RemoveAll(tmp.Dir) }
	if _, err := tmp.run(ctx, "init", "--bare", "--quiet"); err != nil {
		cleanup()
		return false, fmt.Errorf("creating the relay: %w", err)
	}
	if err := tmp.Configure(ctx); err != nil {
		cleanup()
		return false, err
	}
	// Hooks shipped by git init's template are samples; nothing runs them
	// (core.hooksPath), but removing the directory says so plainly.
	if err := os.RemoveAll(filepath.Join(tmp.Dir, "hooks")); err != nil {
		cleanup()
		return false, err
	}
	if err := os.Rename(tmp.Dir, r.Dir); err != nil {
		cleanup()
		if errors.Is(err, fs.ErrExist) || r.Exists() {
			return false, r.Configure(ctx)
		}
		return false, fmt.Errorf("creating the relay: %w", err)
	}
	return true, nil
}

// Configure applies the relay configuration (idempotent).
func (r *Relay) Configure(ctx context.Context) error {
	for _, kv := range relayConfig {
		if _, err := r.run(ctx, "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("configuring the relay: %w", err)
		}
	}
	return nil
}

// SetUpstream points the remote "github" at url, or removes it when url is
// empty. It runs before every upstream operation, so the relay always uses
// the URL from the project definition.
func (r *Relay) SetUpstream(ctx context.Context, url string) error {
	if url == "" {
		// git exits non-zero when there is no such section.
		_, err := r.run(ctx, "config", "--remove-section", "remote."+UpstreamRemote)
		var ee *execx.ExitError
		if err != nil && !errors.As(err, &ee) {
			return err
		}
		return nil
	}
	if strings.HasPrefix(url, "-") || strings.Contains(url, "::") || strings.ContainsAny(url, "\x00\n\r") {
		return fmt.Errorf("refusing upstream URL %q", url)
	}
	for _, kv := range [][2]string{
		{"remote." + UpstreamRemote + ".url", url},
		{"remote." + UpstreamRemote + ".fetch", "+refs/heads/*:" + GithubPrefix + "*"},
		{"remote." + UpstreamRemote + ".tagOpt", "--tags"},
	} {
		if _, err := r.run(ctx, "config", "--replace-all", kv[0], kv[1]); err != nil {
			return fmt.Errorf("configuring the upstream remote: %w", err)
		}
	}
	return nil
}

var oidRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ValidOID reports whether s is a full object name.
func ValidOID(s string) bool { return oidRE.MatchString(s) }

// Refs lists the refs below the given prefixes (all refs without one).
func (r *Relay) Refs(ctx context.Context, prefixes ...string) ([]Ref, error) {
	args := append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, prefixes...)
	res, err := r.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("listing the relay's refs: %w", err)
	}
	return parseRefList(string(res.Stdout))
}

// parseRefList parses "<oid> <refname>" lines; ref names cannot contain
// spaces or newlines.
func parseRefList(s string) ([]Ref, error) {
	var out []Ref
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		oid, name, ok := strings.Cut(line, " ")
		if !ok || !ValidOID(oid) || !strings.HasPrefix(name, "refs/") || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("unexpected ref line %q", line)
		}
		out = append(out, Ref{Name: name, OID: oid})
	}
	return out, nil
}

// RefSet lists and sorts the refs a bank or pull works with.
func (r *Relay) RefSet(ctx context.Context) (RefSet, error) {
	refs, err := r.Refs(ctx, "refs/heads/", "refs/remotes/github/", "refs/tags/", "refs/devc/")
	if err != nil {
		return RefSet{}, err
	}
	return SortRefs(refs), nil
}

// RefUpdate is one line of an update-ref transaction. New empty deletes
// the ref; Old empty means no check, ZeroOID that the ref must not exist.
type RefUpdate struct {
	Ref string
	New string
	Old string
}

// Zero returns the null object name of oid's length.
func Zero(oid string) string { return strings.Repeat("0", max(len(oid), 40)) }

func validRefName(n string) bool {
	return strings.HasPrefix(n, "refs/") && !strings.ContainsAny(n, " \t\n\x00")
}

// Update applies updates as one transaction: all or none.
func (r *Relay) Update(ctx context.Context, ups []RefUpdate) error {
	return r.update(ctx, ups, nil)
}

// update is Update with -c settings for the transaction.
func (r *Relay) update(ctx context.Context, ups []RefUpdate, config []string) error {
	if len(ups) == 0 {
		return nil
	}
	// Without start/commit lines, update-ref --stdin is still one
	// transaction.
	var b strings.Builder
	for _, u := range ups {
		if !validRefName(u.Ref) || (u.New != "" && !ValidOID(u.New)) || (u.Old != "" && !ValidOID(u.Old)) {
			return fmt.Errorf("invalid ref update %q %q %q", u.Ref, u.New, u.Old)
		}
		switch {
		case u.New == "":
			fmt.Fprintf(&b, "delete %s", u.Ref)
		default:
			fmt.Fprintf(&b, "update %s %s", u.Ref, u.New)
		}
		if u.Old != "" {
			fmt.Fprintf(&b, " %s", u.Old)
		}
		b.WriteByte('\n')
	}
	res, err := r.Host.Run(ctx, Call{Relay: r.Dir, Config: config, Args: []string{"update-ref", "--stdin"}, Stdin: strings.NewReader(b.String())})
	if err != nil {
		return fmt.Errorf("updating refs: %w%s", err, tail(res.Stderr))
	}
	return nil
}

// ClearIncoming deletes the scratch refs of a bank.
func (r *Relay) ClearIncoming(ctx context.Context) error {
	refs, err := r.Refs(ctx, IncomingPrefix)
	if err != nil {
		return err
	}
	var ups []RefUpdate
	for _, ref := range refs {
		ups = append(ups, RefUpdate{Ref: ref.Name})
	}
	return r.Update(ctx, ups)
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Relay) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	res, err := r.run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	var ee *execx.ExitError
	if errors.As(err, &ee) && res.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// LeftRight counts the commits only in a and only in b.
func (r *Relay) LeftRight(ctx context.Context, a, b string) (int, int, error) {
	s, err := r.out(ctx, "rev-list", "--count", "--left-right", a+"..."+b)
	if err != nil {
		return 0, 0, err
	}
	l, rr, ok := strings.Cut(s, "\t")
	if !ok {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", s)
	}
	left, err1 := strconv.Atoi(l)
	right, err2 := strconv.Atoi(rr)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", s)
	}
	return left, right, nil
}

// CountNew counts the commits reachable from oid but from no relay branch.
func (r *Relay) CountNew(ctx context.Context, oid string) (int, error) {
	s, err := r.out(ctx, "rev-list", "--count", oid, "--not", "--branches")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// HeadBranch returns the branch the relay's HEAD names.
func (r *Relay) HeadBranch(ctx context.Context) (string, error) {
	s, err := r.out(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	b, ok := strings.CutPrefix(s, HeadsPrefix)
	if !ok || CheckName(b) != nil {
		return "", fmt.Errorf("the relay's HEAD names %q", s)
	}
	return b, nil
}

// SetHead points the relay's HEAD at a branch.
func (r *Relay) SetHead(ctx context.Context, branch string) error {
	if err := CheckName(branch); err != nil {
		return fmt.Errorf("default branch %q: %w", branch, err)
	}
	_, err := r.run(ctx, "symbolic-ref", "HEAD", HeadsPrefix+branch)
	return err
}

// CheckRefFormat runs git check-ref-format --branch on a name.
func (r *Relay) CheckRefFormat(ctx context.Context, name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if _, err := r.run(ctx, "check-ref-format", "--branch", name); err != nil {
		return errors.New("refused by git check-ref-format --branch")
	}
	return nil
}

// tail is the last lines of a command's error output for messages.
func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	return "\n" + s
}
