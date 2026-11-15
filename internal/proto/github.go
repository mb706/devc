package proto

// GitHub (work order C15). host.github is the read-only facade: the box
// names an operation and its parameters, never a URL, a method or a
// header; the daemon answers with one response line (GitHubReply) and then
// exactly Length bytes of the body, as GitHub sent them, and closes the
// stream. host.github.token hands a box whose project grants one (grants.github =
// "app" or "token:<secret>") the token for git and gh; it is never logged.

import "time"

// Box to host methods of work order C15.
const (
	MethodHostGitHub      = "host.github"
	MethodHostGitHubToken = "host.github.token"
)

// Read operations of host.github.
const (
	GitHubPRList    = "pr.list"
	GitHubPRView    = "pr.view"
	GitHubIssueList = "issue.list"
	GitHubIssueView = "issue.view"
	GitHubRunList   = "run.list"
	GitHubRunView   = "run.view"
	GitHubAPI       = "api"
)

// GitHubOps lists the read operations.
var GitHubOps = []string{GitHubPRList, GitHubPRView, GitHubIssueList, GitHubIssueView, GitHubRunList, GitHubRunView, GitHubAPI}

// GitHubParams ask for one read. Only the fields of the operation may be
// set; the daemon refuses anything else.
type GitHubParams struct {
	Op string `json:"op"`
	// Repo is "owner/name" (default: the project's own repository); it must
	// be the project's or listed in grants.github_read.
	Repo string `json:"repo,omitempty"`
	// Number is a pull request, issue or workflow run.
	Number   int64 `json:"number,omitempty"`
	Comments bool  `json:"comments,omitempty"`
	Diff     bool  `json:"diff,omitempty"`
	Log      bool  `json:"log,omitempty"`
	Failed   bool  `json:"failed,omitempty"`
	// Branch filters workflow runs; State lists open, closed or all; Limit
	// is the number of entries of a list (1-100, default 30).
	Branch string `json:"branch,omitempty"`
	State  string `json:"state,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	// Path is the REST path of api: repos/<owner>/<repo>[/…][?query].
	Path string `json:"path,omitempty"`
}

// GitHubReply precedes the body of a host.github answer. Status is
// GitHub's HTTP status; Text says the body is text (a diff or a log), not
// JSON; Truncated says the body was cut at the size limit (Note says how).
type GitHubReply struct {
	Status    int    `json:"status"`
	Text      bool   `json:"text,omitempty"`
	Length    int64  `json:"length"`
	Truncated bool   `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
}

// GitHubTokenParams ask for the project's token (host.github.token).
type GitHubTokenParams struct{}

// GitHubTokenReply carries a token for git and gh, and the protocol and
// host it is for (the credential helper answers only for them). Expires is
// set for minted tokens. It is never logged.
type GitHubTokenReply struct {
	Token    string     `json:"token"`
	Username string     `json:"username"`
	Protocol string     `json:"protocol"`
	Host     string     `json:"host"`
	Expires  *time.Time `json:"expires,omitempty"`
}
