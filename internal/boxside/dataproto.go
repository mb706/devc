package boxside

// The data workers run in the helper container (plan 3.3, work order C06):
// `devc box data-worker` reads one DataRequest as JSON on stdin, acts on
// the project directory mounted at /target (and, for restores from restic,
// the staging directory at /staging), and prints one DataResult. The host
// decodes the result strictly, with a size cap, and validates it.

// HelperStaging is where the helper container mounts a staging directory
// (restic restores) read-only.
const HelperStaging = "/staging"

// DataProtoVersion is the version of DataRequest and DataResult.
const DataProtoVersion = 1

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
	// data root; empty means all), and whether to restore in place.
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
	// Digest asks seal and setup for the digest of what they cover (the
	// manifest); Content adds the content hash (seal, push-stage, digest).
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
	// that changed while the worker ran), relative to the data root.
	Skipped []string `json:"skipped,omitempty"`
	// Created lists directories setup created.
	Created []string `json:"created,omitempty"`
	// TrashID is the trash entry rm or an in-place restore created.
	TrashID string `json:"trash_id,omitempty"`
	// Paths are the paths trash-restore put back, Displaced the ones an
	// in-place restore moved to the trash, Removed the IDs deleted.
	Paths     []string `json:"paths,omitempty"`
	Displaced []string `json:"displaced,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	// Dest is the directory a restore copied into (relative to the data
	// root), "" for an in-place restore.
	Dest string `json:"dest,omitempty"`
	// Unsupported is set by reflink-save when the filesystem cannot clone.
	Unsupported bool `json:"unsupported,omitempty"`
	// Exclude lists what a savepoint leaves out: input zones and sealed
	// entries (reflink-save, and ls for restic).
	Exclude []string `json:"exclude,omitempty"`
	// Same counts the files pull-in left alone because they were
	// identical already.
	Same int64 `json:"same,omitempty"`
	// Stage is the push staging directory push-stage created.
	Stage string `json:"stage,omitempty"`
	// Digests describe the entries for the manifest (seal, setup with
	// Digest, reflink-save, pull-in, push-stage, digest).
	Digests []EntryDigest `json:"digests,omitempty"`
	// RemoteDigests (pull-in) describe the same entries as the remote had
	// them, from the staged tree: the remote's observation, which differs
	// from Digests when sealed entries were skipped or local-only content
	// was kept (finding D2-12).
	RemoteDigests []EntryDigest `json:"remote_digests,omitempty"`
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
	Reason string   `json:"reason"` // rm or restore
	Paths  []string `json:"paths"`
}
