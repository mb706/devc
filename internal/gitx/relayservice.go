package gitx

// The relay service (work order C10a section 1): a box's git push and
// fetch reach the relay through the daemon, which runs git upload-pack or
// git receive-pack on the relay with the box's channel stream as standard
// input and output. The box never gets a path to the relay; the project is
// the one bound to the channel.
//
// Pushes are checked by two hooks in a host-controlled directory, which
// are links to the devc binary (no shell is involved): update applies the
// policy table (CheckUpdate) to each ref, post-receive records what was
// accepted the way a bank would (RecordPush) and forwards branches listed
// in git.forward. Ref names and object names in hook input come from a box:
// they are validated before use and only ever passed as single arguments
// or update-ref lines.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/paths"
)

// Services a box may ask for.
const (
	ServiceUploadPack  = "upload-pack"
	ServiceReceivePack = "receive-pack"
)

// Hook names in the relay hooks directory.
const (
	HookUpdate      = "update"
	HookPostReceive = "post-receive"
)

// Environment of the relay hooks, set by the daemon for receive-pack.
const (
	// EnvRelayProject names the project (realm/project) whose relay is
	// served: the one bound to the box's channel.
	EnvRelayProject = "DEVC_RELAY_PROJECT"
	// EnvRelayReport is the file post-receive writes its report to.
	EnvRelayReport = "DEVC_RELAY_REPORT"
)

// serveTimeout bounds one upload-pack or receive-pack run.
const serveTimeout = 30 * time.Minute

// ServeCall is the host git command that serves one service on a relay.
// upload-pack hides devc's bookkeeping; receive-pack also hides
// refs/remotes, refuses deletions on its own (the update hook refuses them
// too), never collects garbage and runs the hooks in hooksDir, with env
// added for them.
func ServeCall(relay, service, hooksDir string, env []string, stdin io.Reader) (Call, error) {
	c := Call{Relay: relay, Stdin: stdin, Timeout: serveTimeout, Env: env}
	switch service {
	case ServiceUploadPack:
		c.Config = []string{"uploadpack.hideRefs=refs/devc"}
		c.Args = []string{"upload-pack", "--strict", "--timeout=600", relay}
	case ServiceReceivePack:
		if !filepath.IsAbs(hooksDir) {
			return Call{}, fmt.Errorf("relay hooks directory %q is not absolute", hooksDir)
		}
		c.HooksDir = hooksDir
		c.Config = []string{"receive.hideRefs=refs/devc", "receive.hideRefs=refs/remotes", "receive.denyDeletes=true",
			"receive.autogc=false", "receive.fsckObjects=true", "receive.advertisePushOptions=false",
			"receive.updateServerInfo=false", "receive.shallowUpdate=false"}
		c.Args = []string{"receive-pack", relay}
	default:
		return Call{}, fmt.Errorf("unknown git service %q", service)
	}
	return c, nil
}

// isZeroOID reports whether oid is the null object name.
func isZeroOID(oid string) bool { return strings.Trim(oid, "0") == "" }

// ErrPolicy wraps the refusals of the push policy.
var ErrPolicy = errors.New("refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrPolicy}, args...)...)
}

// CheckUpdate applies the push policy (work order C10a section 1) to one
// ref update a box sent:
//
//	refs/heads/<b>  create and update allowed, also non-fast-forward;
//	                deletion refused; protected branches refused; name
//	                limits as for banks
//	refs/tags/<t>   creation allowed; update and deletion refused
//	anything else   refused
//
// checkBranch validates a branch name (CheckName plus git's own check; nil:
// CheckName).
func CheckUpdate(refname, oldOID, newOID string, protected []string, checkBranch func(string) error) error {
	if checkBranch == nil {
		checkBranch = CheckName
	}
	if !validRefName(refname) || len(refname) > 300 || !ValidOID(oldOID) || !ValidOID(newOID) || len(oldOID) != len(newOID) {
		return refuse("malformed ref update")
	}
	switch {
	case strings.HasPrefix(refname, HeadsPrefix):
		b := refname[len(HeadsPrefix):]
		if isZeroOID(newOID) {
			return refuse("deleting branches is not allowed: the relay keeps every branch (delete %s in the box only)", printableName(b))
		}
		if err := checkBranch(b); err != nil {
			return refuse("invalid branch name %q: %v", printableName(b), err)
		}
		if Protected(protected, b) {
			return refuse("%s is protected (git.protected): the relay keeps its own tip; push it with 'devc push' on the host", b)
		}
		return nil
	case strings.HasPrefix(refname, TagsPrefix):
		t := refname[len(TagsPrefix):]
		if isZeroOID(newOID) {
			return refuse("deleting tags is not allowed")
		}
		if !isZeroOID(oldOID) {
			return refuse("tag %s exists in the relay: tags can be created, not moved", printableName(t))
		}
		if err := CheckName(t); err != nil {
			return refuse("invalid tag name %q: %v", printableName(t), err)
		}
		return nil
	}
	return refuse("only branches (refs/heads/*) and tags (refs/tags/*) can be pushed to the relay, not %s", printableName(refname))
}

// PushedRef is one line of post-receive input: an update receive-pack
// applied.
type PushedRef struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

// maxPushedRefs bounds the post-receive input devc reads.
const maxPushedRefs = 10000

// ParseHookInput reads post-receive input ("<old> <new> <ref>" per line)
// strictly.
func ParseHookInput(r io.Reader) ([]PushedRef, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 4096)
	var out []PushedRef
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		f := strings.Split(line, " ")
		if len(f) != 3 || !ValidOID(f[0]) || !ValidOID(f[1]) || !validRefName(f[2]) {
			return nil, fmt.Errorf("malformed hook input line %q", printableName(line))
		}
		if len(out) == maxPushedRefs {
			return nil, fmt.Errorf("more than %d updated refs", maxPushedRefs)
		}
		out = append(out, PushedRef{Old: f[0], New: f[1], Ref: f[2]})
	}
	return out, sc.Err()
}

// PushReport is what post-receive recorded for one push: it goes to the
// daemon (for a note event) and, formatted, to the pusher.
type PushReport struct {
	Branches []BranchChange `json:"branches"`
	Tags     []Recorded     `json:"tags"`
	Forwards []Forward      `json:"forwards"`
	Problems []string       `json:"problems"`
}

// forwardTimeout bounds one forward from the post-receive hook (the pusher
// waits for it).
const forwardTimeout = 2 * time.Minute

// RecordPush records the updates of a push the way a bank would (finding
// B8): every new branch tip gets a bank ref unless it was the branch's last
// one; the box's observed tip moves with it when the box's own branch of
// that name has the pushed tip (a push of another local branch, as in
// "git push origin wip:feature", is no observation of the box's feature);
// a new tag gets a refs/devc/tags record. Branches matching git.forward
// are then pushed to the upstream. The caller (the daemon) holds the box
// lock for the whole receive-pack run.
func (s *Service) RecordPush(ctx context.Context, p *Project, pushed []PushedRef) *PushReport {
	rep := &PushReport{Branches: []BranchChange{}, Tags: []Recorded{}, Forwards: []Forward{}, Problems: []string{}}
	set, err := p.Relay.RefSet(ctx)
	if err != nil {
		rep.Problems = append(rep.Problems, err.Error())
		return rep
	}
	local, err := s.boxHeads(ctx, p)
	if err != nil {
		// Without the box's own branches no tip is observed; the next bank
		// observes them.
		local = nil
	}
	now := s.now().Unix()
	updated := map[string]string{}
	for _, u := range pushed {
		switch {
		case strings.HasPrefix(u.Ref, HeadsPrefix) && !isZeroOID(u.New):
			b := u.Ref[len(HeadsPrefix):]
			if CheckName(b) != nil {
				continue
			}
			ch := BranchChange{Branch: b, New: u.New}
			if !isZeroOID(u.Old) {
				ch.Old = u.Old
			}
			var ups []RefUpdate
			if last := set.LastBank[b]; last.OID != u.New {
				ch.BankRef = BankPrefix + b + "/" + strconv.FormatInt(stamp(now, last.Time), 10)
				ups = append(ups, RefUpdate{Ref: ch.BankRef, New: u.New, Old: Zero(u.New)})
			}
			if local != nil && local[b] == u.New && set.BoxTips[b] != u.New {
				ups = append(ups, boxTipUpdate(set.BoxTips, b, u.New))
			}
			if err := p.Relay.Update(ctx, ups); err != nil {
				ch.Error = printable(err.Error())
				rep.Problems = append(rep.Problems, fmt.Sprintf("recording %s: %s", b, ch.Error))
			}
			rep.Branches = append(rep.Branches, ch)
			updated[b] = u.New
		case strings.HasPrefix(u.Ref, TagsPrefix) && isZeroOID(u.Old) && !isZeroOID(u.New):
			t := u.Ref[len(TagsPrefix):]
			if CheckName(t) != nil || set.DevcTags[t][u.New] {
				continue
			}
			rec := Recorded{Name: t, OID: u.New, Ref: DevcTagsPrefix + t + "/" + strconv.FormatInt(stamp(now, set.NewestTag[t]), 10)}
			if err := p.Relay.Update(ctx, []RefUpdate{{Ref: rec.Ref, New: rec.OID, Old: Zero(rec.OID)}}); err != nil {
				rec.Error = printable(err.Error())
				rep.Problems = append(rep.Problems, fmt.Sprintf("recording tag %s: %s", t, rec.Error))
			}
			rep.Tags = append(rep.Tags, rec)
		}
	}
	if p.Resolved.Upstream != "" && len(p.Resolved.Git.Forward) > 0 && len(updated) > 0 {
		fctx, cancel := context.WithTimeout(ctx, forwardTimeout*time.Duration(min(len(updated), 5)))
		rep.Forwards = s.forward(fctx, p, updated, set.Github)
		cancel()
	}
	return rep
}

// boxHeads lists the box's own branches (git in the box, as dev); the
// answer is validated like every report from a box.
func (s *Service) boxHeads(ctx context.Context, p *Project) (map[string]string, error) {
	out, err := s.boxGit(ctx, p, "/", "--git-dir="+paths.InBoxRepoDir(p.Ref), "for-each-ref", "--format=%(objectname) %(refname)", HeadsPrefix)
	if err != nil {
		return nil, err
	}
	refs, err := parseRefList(out)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, r := range refs {
		if b, ok := strings.CutPrefix(r.Name, HeadsPrefix); ok {
			m[b] = r.OID
		}
	}
	return m, nil
}

// FormatPush writes what the pusher sees of a push's recording: forwards
// and problems only (git itself lists the updated refs).
func FormatPush(r *PushReport) string {
	var b strings.Builder
	for _, f := range r.Forwards {
		if f.Error != "" {
			fmt.Fprintf(&b, "devc: forwarding %s to the upstream failed: %s\n", f.Branch, f.Error)
		} else {
			fmt.Fprintf(&b, "devc: forwarded %s to the upstream\n", f.Branch)
		}
	}
	for _, pr := range r.Problems {
		fmt.Fprintf(&b, "devc: %s\n", pr)
	}
	return b.String()
}

// Summary is a one-line description of a push for a note event.
func (r *PushReport) Summary() string {
	var parts []string
	for _, c := range r.Branches {
		if c.Error == "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", c.Branch, short(c.New)))
		}
	}
	for _, t := range r.Tags {
		if t.Error == "" {
			parts = append(parts, "tag "+t.Name)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	s := "pushed to the relay: " + strings.Join(parts, ", ")
	for _, f := range r.Forwards {
		if f.Error == "" {
			s += "; forwarded " + f.Branch
		} else {
			s += "; forwarding " + f.Branch + " failed"
		}
	}
	return s
}

// EnsureRelayHooks makes dir hold exactly the relay hooks: symbolic links
// named update and post-receive pointing at the devc executable exe (which
// dispatches on its name). dir is host-owned (below the state directory's
// local part, which no box mounts); anything else in it is removed.
func EnsureRelayHooks(dir, exe string) error {
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("the devc executable %q is not an absolute path", exe)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	want := map[string]bool{HookUpdate: true, HookPostReceive: true}
	for _, de := range des {
		if !want[de.Name()] {
			if err := os.RemoveAll(filepath.Join(dir, de.Name())); err != nil {
				return err
			}
		}
	}
	for name := range want {
		p := filepath.Join(dir, name)
		if target, err := os.Readlink(p); err == nil && target == exe {
			continue
		}
		tmp := p + ".new"
		_ = os.Remove(tmp)
		if err := os.Symlink(exe, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return nil
}

// EnsureEmptyDir creates dir (0700) and checks that it is empty: it is the
// hooks path of every other host git command.
func EnsureEmptyDir(dir string, mkdir func(string) error) error {
	if err := mkdir(dir); err != nil {
		return err
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(des) > 0 {
		return fmt.Errorf("%s must be empty (it is the hooks directory of devc's git commands): remove %s", dir, des[0].Name())
	}
	return nil
}

// FetchThrough brings the upstream into the relay before the box fetches
// (work order C10a section 1): a non-interactive fetch bounded by ctx, then
// the relay's branches are fast-forwarded as devc pull does. Box tips are
// not touched, so a later bank keeps the advanced heads. The caller holds
// the box lock.
func (s *Service) FetchThrough(ctx context.Context, p *Project) (*PullResult, error) {
	res := &PullResult{Branches: []BranchChange{}, Diverged: []string{}, Notes: []string{}}
	if !p.Relay.Exists() || p.Resolved.Upstream == "" {
		return res, nil
	}
	if err := s.fetchUpstream(ctx, p); err != nil {
		return res, err
	}
	return res, s.syncHeads(ctx, p, res)
}

// RelayExists reports whether the relay of dir exists (for callers that
// have no Project).
func RelayExists(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, "HEAD"))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return err == nil && fi.Mode().IsRegular()
}
