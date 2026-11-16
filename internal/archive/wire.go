package archive

import (
	"time"
)

// Endpoints of the hub (machine certificates only; work order C17).
//
//	PUT /machine/archive/<realm>/<path>?offset=<n>[&final=1][&class=<c>][&mode=<octal>]
//	    append to the machine's own archive (body: the bytes). For a
//	    bundle, final makes it complete. For a file generation, final
//	    marks it complete at its length after the append (an empty body
//	    with final creates an empty file); class (ClassAppend or
//	    ClassMutable) and mode (permission bits) are recorded when the
//	    generation is created: mutable only below files/harness/, and a
//	    file keeps the class of its first generation (409 naming it
//	    otherwise). An empty body without final only reports the stored
//	    length.
//	GET /machine/archive-index
//	    every machine's archives: ListReply
//	GET /machine/archive-index/<machine>/<realm>[?after=<path>]
//	    one archive, a page of at most IndexPage entries: Index
//	GET /machine/archive-file/<machine>/<realm>/<path>[?sha256=<length>]
//	    a file's bytes (Range supported), or the SHA-256 of its first
//	    <length> bytes as HashReply
//	PUT /machine/secrets/<realm>   an encrypted secrets bundle (Sealed)
//	GET /machine/secrets/<realm>[?machine=<m>]   the newest one: SecretsReply
const (
	UploadPrefix  = "/machine/archive/"
	IndexPath     = "/machine/archive-index"
	FilePrefix    = "/machine/archive-file/"
	SecretsPrefix = "/machine/secrets/"
)

// RequestHeader must be 1 on uploads (as on every state-changing hub
// request).
const RequestHeader = "X-Devc-Request"

// MaxChunk is the largest upload a hub accepts in one request by default
// (hub.toml [archive] max_request may lower it).
const MaxChunk = 8 << 20

// Classes of archived files (findings D1-14, E1-04). A machine proposes
// the class when it uploads a file's first generation (from the harness
// manifests); the hub refuses mutable outside shared harness state
// (MutableAllowed) and keeps the class of a file's first generation for
// all later ones. Every generation of an append-only file (a transcript,
// a host-written log) is kept; a mutable file gets a new generation for
// any change, and the hub keeps the newest KeepMutable complete
// generations of it and, for every checkpoint of its retention window
// (the starts of the last days and weeks, by the hub's clock), the newest
// generation completed by then.
const (
	ClassAppend  = "append"
	ClassMutable = "mutable"
	KeepMutable  = 3
)

// IndexPage is the most entries one index answer holds; MaxIndexEntries
// the most a client reads of one archive.
const (
	IndexPage       = 2000
	MaxIndexEntries = 1_000_000
)

// PutReply answers an upload: the stored length after it and, for a
// bundle, whether it is complete.
type PutReply struct {
	Length   int64 `json:"length"`
	Complete bool  `json:"complete,omitempty"`
}

// ErrorReply is the body of a refused request. Length is set when an
// upload's offset differed from the stored length (409); Complete when the
// bundle, or the mutable file generation, is complete already (it takes
// no more bytes); Class when a new file generation named another class
// than the file has on the hub (409, the class to use); Quota when an
// archive quota was reached (507).
type ErrorReply struct {
	Error    string `json:"error"`
	Length   *int64 `json:"length,omitempty"`
	Complete bool   `json:"complete,omitempty"`
	Class    string `json:"class,omitempty"`
	Quota    bool   `json:"quota,omitempty"`
}

// Summary describes one machine's archive of one realm.
type Summary struct {
	Machine string    `json:"machine"`
	Realm   string    `json:"realm"`
	Files   int       `json:"files"`
	Bytes   int64     `json:"bytes"`
	Updated time.Time `json:"updated"`
}

// ListReply is the answer to GET IndexPath.
type ListReply struct {
	Archives []Summary `json:"archives"`
}

// Entry is one stored file, bundle or ref log of an archive. Complete is
// false for a bundle whose upload did not finish; for a file generation
// it says that the machine marked it complete, at Committed bytes
// (finding D1-15: a generation still being uploaded is not used by a
// restore). Class and Mode are what was recorded when the generation was
// created; a generation without a record (an archive from before records
// existed) has no Class. Completed is when the generation first became
// complete, by the hub's clock (never decreasing with the generation
// number of a file); a restore "as of" a time uses it.
type Entry struct {
	Path      string     `json:"path"`
	Length    int64      `json:"length"`
	Complete  bool       `json:"complete,omitempty"`
	Committed *int64     `json:"committed,omitempty"`
	Class     string     `json:"class,omitempty"`
	Mode      uint32     `json:"mode,omitempty"`
	Completed *time.Time `json:"completed,omitempty"`
	Modified  time.Time  `json:"modified"`
}

// Index lists (a page of) one machine's archive of one realm, in the
// order of the hub's tree; More says that another page follows, asked for
// with after=Next.
type Index struct {
	Machine string  `json:"machine"`
	Realm   string  `json:"realm"`
	Entries []Entry `json:"entries"`
	More    bool    `json:"more,omitempty"`
	Next    string  `json:"next,omitempty"`
}

// HashReply is the SHA-256 (hex) of a stored file's first Length bytes.
type HashReply struct {
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
}

// SecretsReply is an encrypted secrets bundle as the hub stored it.
type SecretsReply struct {
	Machine string    `json:"machine"`
	Version int       `json:"version"`
	Created time.Time `json:"created"`
	Sealed  Sealed    `json:"sealed"`
}

// RefLine is one line of a repository's ref log (RefsPath): the refs
// that changed (Set, new values) or went (Del) since the previous line,
// and how many bundles in sequence hold the objects they need. The first
// line sets every ref.
type RefLine struct {
	Bundles int               `json:"bundles"`
	Set     map[string]string `json:"set,omitempty"`
	Del     []string          `json:"del,omitempty"`
}
