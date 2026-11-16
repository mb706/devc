// This file defines the machine link between a daemon and a hub, and what
// the daemon reports about its hub links and archive uploads.

package proto

// The machine link (work order C11a section 3): a machine daemon dials out
// to a hub and keeps one WebSocket connection, one JSON-RPC object per text
// frame. Over it the hub is the caller of an allowlist of the daemon's
// methods (internal/daemon decides which and with what origin); the
// machine calls only hub.hello and hub.role.

import (
	"errors"
	"regexp"
	"time"
	"unicode/utf8"
)

// HubProtocol is the version of the machine link, exchanged by hub.hello.
const HubProtocol = 1

// Methods of the machine link, served by the hub, and the daemon's local
// method that reports its hub connections.
const (
	MethodHubHello   = "hub.hello"
	MethodHubRole    = "hub.role"
	MethodHubsStatus = "hubs.status"
)

// HubHello is what a machine sends first: the protocol version, its host
// name, its devc version and the daemon's instance ID. The hub names the
// machine after its certificate, never after Name.
type HubHello struct {
	Protocol int    `json:"protocol"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Instance string `json:"instance"`
}

var instanceRE = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// Validate checks a hello that came from a machine.
func (h *HubHello) Validate() error {
	switch {
	case h.Protocol != HubProtocol:
		return errors.New("unknown protocol version")
	case !shortText(h.Name, 255) || !shortText(h.Version, 128):
		return errors.New("invalid name or version")
	case !instanceRE.MatchString(h.Instance):
		return errors.New("invalid instance")
	}
	return nil
}

// shortText accepts printable UTF-8 of at most max bytes without control
// characters.
func shortText(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && Printable(s) == s
}

// HubHelloReply is the hub's answer: its protocol version, its own
// instance ID and the machine name it took from the certificate.
type HubHelloReply struct {
	Protocol int    `json:"protocol"`
	Hub      string `json:"hub"`
	Machine  string `json:"machine"`
}

// HubRole tells a hub whether this machine asks it to send notifications
// (the first connected hub in the machine's list).
type HubRole struct {
	Notifier bool `json:"notifier"`
}

// States of a hub connection on the machine.
const (
	HubConnecting   = "connecting"
	HubConnected    = "connected"
	HubDisconnected = "disconnected"
	HubNoCreds      = "no-credentials"
)

// HubStatus is one hub connection as the daemon reports it (hubs.status).
type HubStatus struct {
	URL      string     `json:"url"`
	State    string     `json:"state"`
	Detail   string     `json:"detail,omitempty"`
	Since    *time.Time `json:"since,omitempty"`
	Notifier bool       `json:"notifier"`
	// Machine is the name the hub knows this machine by.
	Machine string `json:"machine,omitempty"`
	// Archive is the state of this machine's archive on the hub.
	Archive *ArchiveHubStatus `json:"archive,omitempty"`
}

// HubsStatusResult is the result of hubs.status, in the order of the hub
// list in config.toml.
type HubsStatusResult struct {
	Hubs []HubStatus `json:"hubs"`
}

// MethodArchiveStatus is the daemon's local method that reports the
// archive uploads to every hub (work order C17); hubs cannot call it.
const MethodArchiveStatus = "archive.status"

// Archive states of a hub.
const (
	ArchiveIdle      = "idle"
	ArchiveUploading = "uploading"
	ArchiveError     = "error"
	ArchiveNoCreds   = "no-credentials"
	ArchiveStarting  = "starting"
	// ArchiveIncomplete: the last pass ended without an error but could
	// not see all durable state (finding D1-16).
	ArchiveIncomplete = "incomplete"
)

// ArchiveHubStatus is the archive of this machine on one hub: what is
// behind per realm after the last pass, and the last error (a reached
// quota says what to do).
type ArchiveHubStatus struct {
	URL        string               `json:"url"`
	Machine    string               `json:"machine,omitempty"`
	State      string               `json:"state"`
	Error      string               `json:"error,omitempty"`
	Quota      bool                 `json:"quota,omitempty"`
	LastPass   *time.Time           `json:"last_pass,omitempty"`
	LastUpload *time.Time           `json:"last_upload,omitempty"`
	Sent       int64                `json:"sent"`
	Realms     []ArchiveRealmStatus `json:"realms"`
}

// ArchiveRealmStatus is one realm on one hub: files and bytes not yet
// uploaded, repositories with refs not yet recorded, entries of the
// shared harness state left out (with a few examples) and credential
// files excluded.
type ArchiveRealmStatus struct {
	Realm         string   `json:"realm"`
	FilesBehind   int      `json:"files_behind"`
	BytesBehind   int64    `json:"bytes_behind"`
	ReposBehind   int      `json:"repos_behind"`
	Skipped       int      `json:"skipped"`
	SkippedSample []string `json:"skipped_sample,omitempty"`
	Excluded      int      `json:"excluded"`
	// Waiting counts mutable files that changed since their last
	// generation and wait for [sync] mutable_interval to pass.
	Waiting int `json:"waiting,omitempty"`
	// Incomplete says why the pass could not see all of the realm's
	// durable state (the shared harness state could not be listed).
	Incomplete string `json:"incomplete,omitempty"`
}

// ArchiveStatusResult is the result of archive.status, in the order of the
// hub list.
type ArchiveStatusResult struct {
	Hubs []ArchiveHubStatus `json:"hubs"`
}
