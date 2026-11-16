// This file is the protocol between the host and the data workers, which
// run in the helper container (plan 3.3, work order C06): `devc box
// data-worker` reads one DataRequest as JSON on stdin, acts on the project
// directory mounted at /target (and, for restic restores and pulls, a
// staging directory at /staging), and prints one DataResult. The host
// decodes the result strictly, with a size cap, and validates it.

package boxside

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// HelperStaging is where the helper container mounts a staging directory
// read-only: what restic restored, or what a pull received.
const HelperStaging = "/staging"

// DataProtoVersion is the version of DataRequest and DataResult.
const DataProtoVersion = 1

// Limits of a worker's report (finding E2-05). The host captures and
// decodes at most MaxReport bytes of it: the same value in both places.
// A pull records at most MaxSyncEntries entries, and only as many as fit
// the report (CheckPullEntries); the host checks that against the preview
// before anything is transferred, the worker against the staged tree
// before anything is merged, so a pull is never refused after it changed
// data. The lists a report carries besides the entries are capped
// (maxReported texts of at most maxReportedText bytes, MaxReportedPaths
// displaced paths), so what they take is bounded too.
const (
	MaxReport        = 64 << 20
	MaxSyncEntries   = 100000
	MaxReportedPaths = 100
	maxReportedText  = 512
	// reportFixed bounds the fields of a pull-in report that are neither
	// entries nor capped lists (version, counts, trash ID).
	reportFixed = 64 << 10
)

// Data worker operations.
const (
	OpSetup         = "setup"
	OpSeal          = "seal"
	OpUnseal        = "unseal"
	OpRm            = "rm"
	OpTrashRestore  = "trash-restore"
	OpTrashEmpty    = "trash-empty"
	OpLs            = "ls"
	OpReflinkSave   = "reflink-save"
	OpSavepointDrop = "savepoint-drop"
	OpRestore       = "restore"
	// Work order C14 (data sync and manifest).
	OpPullIn      = "pull-in"
	OpPushStage   = "push-stage"
	OpPushUnstage = "push-unstage"
	OpDigest      = "digest"
)

// Restore sources.
const (
	SourceSavepoint = "savepoint" // .devc/savepoints/<id> (reflink)
	SourceStaging   = "staging"   // /staging/project/<root> (restic)
)

// DataRequest is what the host asks a data worker to do.
type DataRequest struct {
	V      int        `json:"v"`
	Op     string     `json:"op"`
	Layout DataLayout `json:"layout"`
	// Time is the host's time (RFC 3339, UTC); it stamps records and IDs.
	Time string `json:"time"`
	// Path is the entry of seal, unseal and rm, relative to the data root.
	Path string `json:"path,omitempty"`
	// Commit is the relay's tip of the default branch, recorded by seal.
	Commit string `json:"commit,omitempty"`
	// IDs are trash entries (trash-restore takes one, trash-empty several)
	// or savepoints (savepoint-drop).
	IDs []string `json:"ids,omitempty"`
	// Savepoint names the savepoint of reflink-save and restore.
	Savepoint string `json:"savepoint,omitempty"`
	// Restore: where the content comes from, which paths (relative to the
	// data root; empty means all), and whether to restore in place. Paths
	// are also the targets of pull-in and push-stage and the entries of
	// digest.
	Source  string   `json:"source,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	InPlace bool     `json:"in_place,omitempty"`
	// Sizes asks ls to measure the zones.
	Sizes bool `json:"sizes,omitempty"`
	// Label names the remote of pull-in (it labels the trash entry).
	Label string `json:"label,omitempty"`
	// Stage is the push staging directory below PushStagingDir.
	Stage string `json:"stage,omitempty"`
	// Check makes pull-in check its targets against the tree and stop.
	Check bool `json:"check,omitempty"`
	// Digest asks seal, setup and reflink-save for the digest of what they
	// cover (the manifest); Content adds the content hash (seal,
	// push-stage, digest).
	Digest  bool `json:"digest,omitempty"`
	Content bool `json:"content,omitempty"`
}

// DataResult is what a data worker reports.
type DataResult struct {
	V  int    `json:"v"`
	Op string `json:"op"`
	// Already is set when seal or unseal found nothing to do.
	Already bool `json:"already,omitempty"`
	// Files and Size count what was sealed, unsealed, saved or restored
	// (regular files and symlinks; bytes of regular files).
	Files int64 `json:"files"`
	Size  int64 `json:"size"`
	// Skipped lists entries left alone (FIFOs, sockets, devices, entries
	// that changed while the worker ran, sealed entries a pull did not
	// touch), each a path relative to the data root with the reason.
	Skipped []string `json:"skipped,omitempty"`
	// Created lists directories setup created.
	Created []string `json:"created,omitempty"`
	// TrashID is the trash entry rm, an in-place restore or pull-in
	// created.
	TrashID string `json:"trash_id,omitempty"`
	// Paths are the paths rm moved and trash-restore put back, Displaced
	// the ones an in-place restore or pull-in moved to the trash, Removed
	// the IDs deleted.
	Paths     []string `json:"paths,omitempty"`
	Displaced []string `json:"displaced,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	// Dest is the directory a restore copied into (relative to the data
	// root), "" for an in-place restore.
	Dest string `json:"dest,omitempty"`
	// Unsupported is set by reflink-save when the filesystem cannot clone.
	Unsupported bool `json:"unsupported,omitempty"`
	// Exclude lists what a savepoint leaves out: input zones and sealed
	// entries (reflink-save; setup reports it for the restic driver, and
	// ls too).
	Exclude []string `json:"exclude,omitempty"`
	// DisplacedTotal counts the paths moved out of the way when Displaced
	// lists only some of them (MaxReportedPaths).
	DisplacedTotal int64 `json:"displaced_total,omitempty"`
	// Same counts the files pull-in left alone because they were
	// identical already.
	Same int64 `json:"same,omitempty"`
	// Stage is the push staging directory push-stage created.
	Stage string `json:"stage,omitempty"`
	// Digests describe the entries for the manifest (seal, setup with
	// Digest, reflink-save, pull-in, push-stage, digest). Those of pull-in
	// carry the remote's observation and the merge's outcome as well.
	Digests []EntryDigest `json:"digests,omitempty"`
	// ls
	Zones   []ZoneInfo   `json:"zones,omitempty"`
	Entries []EntryInfo  `json:"entries,omitempty"`
	Trash   []TrashInfo  `json:"trash,omitempty"`
	Seals   []SealRecord `json:"seals,omitempty"`
}

// ZoneInfo describes one zone directory.
type ZoneInfo struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Files  int64  `json:"files"`
	Size   int64  `json:"size"`
}

// EntryInfo is one direct child of a sealed zone.
type EntryInfo struct {
	Path   string `json:"path"`
	Sealed bool   `json:"sealed"`
	// Type is "dir", "file", "symlink" or "other".
	Type string `json:"type"`
}

// TrashInfo describes one trash entry.
type TrashInfo struct {
	ID     string   `json:"id"`
	Time   string   `json:"time"`
	Reason string   `json:"reason"`
	Paths  []string `json:"paths"`
	Files  int64    `json:"files"`
	Size   int64    `json:"size"`
}

// SealRecord is one line of .devc/seals.jsonl.
type SealRecord struct {
	V      int    `json:"v"`
	Op     string `json:"op"` // seal or unseal
	Path   string `json:"path"`
	Time   string `json:"time"`
	Files  int64  `json:"files,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// TrashMeta is .devc/trash/<id>/meta.json.
type TrashMeta struct {
	V      int      `json:"v"`
	ID     string   `json:"id"`
	Time   string   `json:"time"`
	Reason string   `json:"reason"` // rm, restore or pull
	Paths  []string `json:"paths"`
}

// MarshalReport is a worker's report as it is printed: compact JSON (no
// indentation) without HTML escaping, so that a path takes at most what
// PathCost says.
func MarshalReport(res *DataResult) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PathCost is what a path takes in a report written by MarshalReport,
// quotes included.
func PathCost(p string) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(p)
	return buf.Len() - 1 // the encoder's newline
}

// PrintedPathCost bounds PathCost of a path from rsync's listing of it,
// where the bytes rsync cannot print appear as \#ooo and the rest as they
// are (made printable by the host): a byte that is printable ASCII costs
// what JSON makes of it, any other byte 6, the most JSON makes of one
// byte (\u00XX). An escape \#ooo then costs at least the 6 its byte can.
func PrintedPathCost(p string) int {
	n := 2
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '"' || c == '\\':
			n += 2
		case c >= 0x20 && c < 0x7f:
			n++
		default:
			n += 6
		}
	}
	return n
}

// entryOverhead bounds what one entry of a pull-in report takes besides
// its path: every other field at its longest, and a separating comma.
var entryOverhead = func() int {
	d := strings.Repeat("0", 64)
	b, _ := json.Marshal(EntryDigest{Type: "symlink", Files: math.MaxInt64, Size: math.MaxInt64,
		Digest: "sha256:" + d, Content: "sha256:" + d, Remote: "sha256:" + d, Outcome: OutcomePartial})
	return len(b) - len(`""`) + 1
}()

// reportReserve bounds what a pull-in report takes besides its entries:
// the fixed fields and the capped lists (each skipped text and displaced
// path at JSON's worst, 6 bytes per byte).
func reportReserve() int64 {
	skipped := int64(maxReported+1) * int64(6*(maxReportedText+len("…"))+3)
	displaced := int64(MaxReportedPaths) * int64(6*4096+3)
	return reportFixed + skipped + displaced
}

// CheckPullEntries refuses a pull whose report could not hold its
// entries: more than MaxSyncEntries of them, or more bytes than MaxReport
// leaves them, with cost bounding what each path takes (PathCost for the
// staged tree, PrintedPathCost for rsync's listing).
func CheckPullEntries(entries []string, cost func(string) int) error {
	return checkSyncEntries("pull", entries, cost, MaxSyncEntries, MaxReport-reportReserve())
}

// checkSyncEntries is CheckPullEntries for a pull or a push (what), with
// the limits as arguments (the worker's tests lower them).
func checkSyncEntries(what string, entries []string, cost func(string) int, maxEntries int, budget int64) error {
	if len(entries) > maxEntries {
		return fmt.Errorf("the %s covers %d entries (direct children of the zones named, and the other paths), more than devc records in one %s (%d): %s fewer paths at a time",
			what, len(entries), what, maxEntries, what)
	}
	var total int64
	for _, e := range entries {
		total += int64(entryOverhead + cost(e))
	}
	if total > budget {
		return fmt.Errorf("the %s covers %d entries whose names take more room than devc's report of a %s allows (%d of %d bytes): %s fewer paths at a time",
			what, len(entries), what, total, budget, what)
	}
	return nil
}
