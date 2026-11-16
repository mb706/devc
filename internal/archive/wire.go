package archive

import (
	"time"
)

// Endpoints of the hub (machine certificates only; work order C17).
//
//	PUT /machine/archive/<realm>/<path>?offset=<n>[&final=1]
//	    append to the machine's own archive (body: the bytes)
//	GET /machine/archive-index
//	    every machine's archives: ListReply
//	GET /machine/archive-index/<machine>/<realm>
//	    one archive: Index
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

// PutReply answers an upload: the stored length after it and, for a
// bundle, whether it is complete.
type PutReply struct {
	Length   int64 `json:"length"`
	Complete bool  `json:"complete,omitempty"`
}

// ErrorReply is the body of a refused request. Length is set when an
// upload's offset differed from the stored length (409); Complete when the
// bundle is complete already; Quota when an archive quota was reached
// (507).
type ErrorReply struct {
	Error    string `json:"error"`
	Length   *int64 `json:"length,omitempty"`
	Complete bool   `json:"complete,omitempty"`
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
// false for a bundle whose upload did not finish.
type Entry struct {
	Path     string    `json:"path"`
	Length   int64     `json:"length"`
	Complete bool      `json:"complete,omitempty"`
	Modified time.Time `json:"modified"`
}

// Index lists one machine's archive of one realm.
type Index struct {
	Machine string  `json:"machine"`
	Realm   string  `json:"realm"`
	Entries []Entry `json:"entries"`
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
