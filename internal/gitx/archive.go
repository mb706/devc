// This file implements the relay in the archive (work order C17): bundles
// of new objects made by host git on the relay, and restoring a relay from
// bundles. Bundle files come from a hub or an export and are hostile
// input; git fetches them with the relay's fsck settings into a scratch
// namespace, and refs are then created from a validated list, never moved.

package gitx

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RestorePrefix holds the refs a restore fetched until it is done.
const RestorePrefix = "refs/devc/restore/"

// archiveSpaces are the ref namespaces the archive carries: those devc
// itself writes in a relay, without the scratch namespaces (incoming,
// restore). Anything else, refs/replace/ above all (finding D1-07: a
// replacement ref changes how an existing banked commit reads without
// moving its bank ref), is neither archived nor restored.
var archiveSpaces = []string{HeadsPrefix, GithubPrefix, TagsPrefix, BankPrefix, SnapPrefix, DevcTagsPrefix, BoxTipPrefix}

// ArchiveRef reports whether the archive carries ref name.
func ArchiveRef(name string) bool {
	if !validRefName(name) {
		return false
	}
	for _, p := range archiveSpaces {
		if strings.HasPrefix(name, p) && len(name) > len(p) {
			return true
		}
	}
	return false
}

// ArchiveRefs lists the refs the archive carries (ArchiveRef).
func (r *Relay) ArchiveRefs(ctx context.Context) (map[string]string, error) {
	refs, err := r.Refs(ctx, archiveSpaces...)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, ref := range refs {
		if ArchiveRef(ref.Name) {
			out[ref.Name] = ref.OID
		}
	}
	return out, nil
}

// HasObjects returns those of oids the relay has.
func (r *Relay) HasObjects(ctx context.Context, oids []string) ([]string, error) {
	if len(oids) == 0 {
		return nil, nil
	}
	var in strings.Builder
	for _, o := range oids {
		if ValidOID(o) {
			in.WriteString(o + "\n")
		}
	}
	res, err := r.Host.Run(ctx, Call{Relay: r.Dir, Args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"}, Stdin: strings.NewReader(in.String()), MaxOutput: 64 << 20})
	if err != nil {
		return nil, fmt.Errorf("checking objects: %w%s", err, tail(res.Stderr))
	}
	var out []string
	for _, l := range strings.Split(string(res.Stdout), "\n") {
		if oid, typ, ok := strings.Cut(l, " "); ok && ValidOID(oid) && typ != "missing" {
			out = append(out, oid)
		}
	}
	return out, nil
}

// Bundle writes to file (a host path the caller owns) a bundle of refs
// with the objects not reachable from excludes, which must exist in the
// relay. It reports false, writing nothing, when no object is new.
func (r *Relay) Bundle(ctx context.Context, file string, refs map[string]string, excludes []string) (bool, error) {
	if len(refs) == 0 {
		return false, nil
	}
	names := make([]string, 0, len(refs))
	for n := range refs {
		if !validRefName(n) {
			return false, fmt.Errorf("invalid ref %q", n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var in strings.Builder
	for _, n := range names {
		in.WriteString(n + "\n")
	}
	for _, e := range excludes {
		if ValidOID(e) {
			in.WriteString("^" + e + "\n")
		}
	}
	res, err := r.Host.Run(ctx, Call{Relay: r.Dir, Args: []string{"bundle", "create", "--quiet", file, "--stdin"}, Stdin: strings.NewReader(in.String()), Timeout: transferTimeout})
	if err != nil {
		if strings.Contains(string(res.Stderr), "empty bundle") {
			return false, nil
		}
		return false, fmt.Errorf("bundling the relay: %w%s", err, tail(res.Stderr))
	}
	return true, nil
}

// FetchBundle fetches the objects and refs of a bundle file into
// RestorePrefix, checking every object (fsck).
func (r *Relay) FetchBundle(ctx context.Context, file string) error {
	res, err := r.Host.Run(ctx, Call{Relay: r.Dir, Timeout: transferTimeout,
		Config: []string{"fetch.fsckObjects=true", "transfer.fsckObjects=true"},
		Args:   []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-gc", file, "+refs/*:" + RestorePrefix + "*"}})
	if err != nil {
		return fmt.Errorf("fetching a bundle into the relay: %w%s", err, tail(res.Stderr))
	}
	return nil
}

// RestoreRefs creates the refs of want that the relay lacks (their objects
// must be present) and leaves existing ones alone. It returns the refs it
// created and those it left because they differ or lack objects.
func (r *Relay) RestoreRefs(ctx context.Context, want map[string]string) (created, conflicts []string, err error) {
	have, err := r.ArchiveRefs(ctx)
	if err != nil {
		return nil, nil, err
	}
	var oids []string
	for _, o := range want {
		oids = append(oids, o)
	}
	present, err := r.HasObjects(ctx, oids)
	if err != nil {
		return nil, nil, err
	}
	ok := map[string]bool{}
	for _, o := range present {
		ok[o] = true
	}
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	var ups []RefUpdate
	for _, n := range names {
		o := want[n]
		switch cur, exists := have[n]; {
		case !ArchiveRef(n) || !ValidOID(o):
			conflicts = append(conflicts, n+" (not a ref the archive restores)")
		case exists && cur == o:
		case exists:
			conflicts = append(conflicts, n)
		case !ok[o]:
			conflicts = append(conflicts, n+" (objects missing from the bundles)")
		default:
			ups = append(ups, RefUpdate{Ref: n, New: o, Old: Zero(o)})
			created = append(created, n)
		}
	}
	if err := r.Update(ctx, ups); err != nil {
		return nil, nil, err
	}
	return created, conflicts, nil
}

// DropRestoreRefs deletes the scratch refs of a restore.
func (r *Relay) DropRestoreRefs(ctx context.Context) error {
	refs, err := r.Refs(ctx, RestorePrefix)
	if err != nil {
		return err
	}
	var ups []RefUpdate
	for _, ref := range refs {
		ups = append(ups, RefUpdate{Ref: ref.Name})
	}
	return r.Update(ctx, ups)
}
