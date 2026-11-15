package proto

// Requests (work order C10a section 2): small structured messages from a
// box asking the user for something outside the box. A request never
// executes anything by itself; the daemon validates it, stores it, and
// carries it out only after an approval.

import "time"

// Request kinds.
const (
	RequestSeal      = "seal"
	RequestSavepoint = "savepoint"
	RequestNote      = "note"
	RequestPackage   = "package"
	RequestNet       = "net"
	RequestGrant     = "grant"
	RequestPR        = "pr"
)

// Risk classes (plan 3.11): low-risk requests may be approved from
// anywhere, high-risk ones only through the local socket.
const (
	RiskLow  = "low"
	RiskHigh = "high"
)

// RequestRisk returns the risk class of a kind, or "" for an unknown kind.
func RequestRisk(kind string) string {
	switch kind {
	case RequestSeal, RequestSavepoint, RequestNote:
		return RiskLow
	case RequestPackage, RequestNet, RequestGrant, RequestPR:
		return RiskHigh
	}
	return ""
}

// RequestKinds lists every kind.
var RequestKinds = []string{RequestSeal, RequestSavepoint, RequestNote, RequestPackage, RequestNet, RequestGrant, RequestPR}

// DefaultAutoRequests are the kinds approved automatically unless a
// project's [requests] auto says otherwise.
var DefaultAutoRequests = []string{RequestSeal, RequestSavepoint, RequestNote}

// Request statuses. Pending and approved are not final.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDenied   = "denied"
	RequestDone     = "done"
	RequestFailed   = "failed"
	RequestExpired  = "expired"
)

// FinalRequest reports whether a status ends a request.
func FinalRequest(status string) bool {
	switch status {
	case RequestDenied, RequestDone, RequestFailed, RequestExpired:
		return true
	}
	return false
}

// Origins of a decision. Local is the daemon's own socket (the CLI on this
// machine); auto is the project's [requests] auto list; hub is a hub (C11).
const (
	OriginLocal = "local"
	OriginAuto  = "auto"
	OriginHub   = "hub"
)

// RequestParams are what `devc request` sends with host.request. Only the
// fields of the kind may be set; the daemon refuses anything else.
type RequestParams struct {
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
	// seal
	Path string `json:"path,omitempty"`
	// savepoint
	Name string `json:"name,omitempty"`
	// note
	Text string `json:"text,omitempty"`
	// package
	Manager  string   `json:"manager,omitempty"`
	Packages []string `json:"packages,omitempty"`
	// net: host:port or "lan", and a duration
	Target string `json:"target,omitempty"`
	For    string `json:"for,omitempty"`
	// grant: ssh or secret, and Name
	Grant string `json:"grant,omitempty"`
	// pr
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// Request is a stored request.
type Request struct {
	ID      string        `json:"id"`
	Time    time.Time     `json:"time"`
	Ref     string        `json:"ref"`
	Session string        `json:"session,omitempty"`
	Kind    string        `json:"kind"`
	Params  RequestParams `json:"params"`
	Risk    string        `json:"risk"`
	Status  string        `json:"status"`
	Result  string        `json:"result,omitempty"`
	// Updated is the time of the last change; DecidedBy the origin of the
	// decision; Reason the text given with a denial.
	Updated   time.Time `json:"updated"`
	DecidedBy string    `json:"decided_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// RequestReply answers host.request.
type RequestReply struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Risk   string `json:"risk"`
}

// RequestStatusParams ask for a request of the caller's project; with
// Wait (seconds, at most 600) the daemon answers once it is final or the
// time is up.
type RequestStatusParams struct {
	ID   string `json:"id"`
	Wait int    `json:"wait,omitempty"`
}

// RequestStatus is what a box learns of its request.
type RequestStatus struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Risk   string `json:"risk"`
	Status string `json:"status"`
	Result string `json:"result,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// RequestsListParams filter requests.list: pending ones unless All.
type RequestsListParams struct {
	All     bool   `json:"all,omitempty"`
	Realm   string `json:"realm,omitempty"`
	Project string `json:"project,omitempty"`
}

// RequestsListResult lists requests, oldest first.
type RequestsListResult struct {
	Requests []Request `json:"requests"`
}

// DecideParams approve or deny a request. Origin may only name a weaker
// origin than the connection's own (a local client acting for a hub); it
// never raises one.
type DecideParams struct {
	ID      string `json:"id"`
	Approve bool   `json:"approve"`
	Reason  string `json:"reason,omitempty"`
	Origin  string `json:"origin,omitempty"`
}

// DecideResult is the request after the decision.
type DecideResult struct {
	Request Request `json:"request"`
}
