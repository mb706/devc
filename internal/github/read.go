package github

// The read-only facade (work order C15 section 2). A box sends
// proto.GitHubParams; PlanRead validates every field and decides the
// repository, and Client.Read performs the GET requests the plan stands
// for. Nothing the box sends becomes a URL, a method or a header as it is:
// paths are assembled from known segments and validated names only.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// Errors of PlanRead: a malformed request, or a repository the project may
// not read.
var (
	ErrBadRead        = errors.New("invalid GitHub request")
	ErrRepoNotAllowed = errors.New("repository not allowed")
)

func badRead(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrBadRead}, args...)...)
}

// Limits of reads.
const (
	DefaultLimit = 30
	MaxLimit     = 100
	maxNumber    = 1 << 53
	maxAPIPath   = 2048
	maxJobs      = 100
)

// Plan is a validated read.
type Plan struct {
	Op       string
	Repo     Repo
	Number   int64
	Comments bool
	Diff     bool
	Log      bool
	Failed   bool
	Branch   string
	State    string
	Limit    int
	// Path (api) is what follows /repos/<owner>/<repo>, starting with "/"
	// or empty; Query its query.
	Path  string
	Query url.Values
}

// opFields are the parameters each operation takes (repo is allowed for
// all but api, whose path names the repository).
var opFields = map[string][]string{
	proto.GitHubPRList:    {"repo", "state", "limit"},
	proto.GitHubPRView:    {"repo", "number", "comments", "diff"},
	proto.GitHubIssueList: {"repo", "state", "limit"},
	proto.GitHubIssueView: {"repo", "number", "comments"},
	proto.GitHubRunList:   {"repo", "branch", "limit"},
	proto.GitHubRunView:   {"repo", "number", "log", "failed"},
	proto.GitHubAPI:       {"path"},
}

// PlanRead validates a read from a box. own is the project's repository;
// extra are those of grants.github_read in force.
func PlanRead(p proto.GitHubParams, own Repo, extra []Repo) (Plan, error) {
	allowed, ok := opFields[p.Op]
	if !ok {
		return Plan{}, badRead("unknown operation %q (one of %s)", proto.Printable(proto.Truncate(p.Op, 40)), strings.Join(proto.GitHubOps, ", "))
	}
	for _, f := range []struct {
		name string
		set  bool
	}{{"repo", p.Repo != ""}, {"number", p.Number != 0}, {"comments", p.Comments}, {"diff", p.Diff}, {"log", p.Log},
		{"failed", p.Failed}, {"branch", p.Branch != ""}, {"state", p.State != ""}, {"limit", p.Limit != 0}, {"path", p.Path != ""}} {
		if f.set && !slices.Contains(allowed, f.name) {
			return Plan{}, badRead("%s takes no %s", p.Op, f.name)
		}
	}
	pl := Plan{Op: p.Op, Repo: own, Number: p.Number, Comments: p.Comments, Diff: p.Diff, Log: p.Log, Failed: p.Failed,
		Branch: p.Branch, State: p.State, Limit: p.Limit}
	if p.Repo != "" {
		r, err := ParseRepoName(p.Repo)
		if err != nil {
			return Plan{}, badRead("%v", err)
		}
		pl.Repo = r
	}
	if slices.Contains(allowed, "number") && (p.Number <= 0 || p.Number > maxNumber) {
		return Plan{}, badRead("%s needs a positive number", p.Op)
	}
	if p.Comments && p.Diff {
		return Plan{}, badRead("--diff and --comments cannot be combined")
	}
	if p.Failed && !p.Log {
		return Plan{}, badRead("--failed goes with --log")
	}
	if p.Branch != "" {
		if err := names.CheckRef(p.Branch); err != nil {
			return Plan{}, badRead("branch %q: %v", proto.Printable(proto.Truncate(p.Branch, 80)), err)
		}
	}
	switch p.State {
	case "", "open", "closed", "all":
	default:
		return Plan{}, badRead("state must be open, closed or all")
	}
	if slices.Contains(allowed, "limit") {
		switch {
		case p.Limit == 0:
			pl.Limit = DefaultLimit
		case p.Limit < 0 || p.Limit > MaxLimit:
			return Plan{}, badRead("the limit must be between 1 and %d", MaxLimit)
		}
	}
	if p.Op == proto.GitHubAPI {
		r, rest, q, err := ParseAPIPath(p.Path)
		if err != nil {
			return Plan{}, err
		}
		pl.Repo, pl.Path, pl.Query = r, rest, q
	}
	if !pl.Repo.Equal(own) && !slices.ContainsFunc(extra, pl.Repo.Equal) {
		return Plan{}, fmt.Errorf("%w: %s is neither this project's repository (%s) nor listed in grants.github_read", ErrRepoNotAllowed, pl.Repo, own)
	}
	return pl, nil
}

// segmentRE is one path segment of api: RFC 3986 path characters without
// percent-encoding (so no encoded separators or dots can hide in it).
var segmentRE = regexp.MustCompile(`^[A-Za-z0-9._~:@!$'()*+,;=-]+$`)

// ParseAPIPath validates the path of `devc gh api`: repos/<owner>/<repo>
// with an optional rest and query. No percent-encoding, backslashes,
// fragments, empty, "." or ".." segments are accepted.
func ParseAPIPath(s string) (Repo, string, url.Values, error) {
	if s == "" || len(s) > maxAPIPath {
		return Repo{}, "", nil, badRead("the API path must be 1 to %d characters", maxAPIPath)
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= 0x20 || c >= 0x7f || c == '%' || c == '\\' || c == '#' {
			return Repo{}, "", nil, badRead("the API path %q contains %q (percent-encoding, backslashes, fragments, spaces and control characters are not accepted)", proto.Printable(proto.Truncate(s, 80)), string(rune(c)))
		}
	}
	p, rawq, _ := strings.Cut(s, "?")
	p = strings.TrimPrefix(p, "/")
	segs := strings.Split(p, "/")
	if len(segs) < 3 || segs[0] != "repos" {
		return Repo{}, "", nil, badRead("the API path must start with repos/<owner>/<repo>")
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." || !segmentRE.MatchString(seg) {
			return Repo{}, "", nil, badRead("the API path %q has an empty, '.', '..' or invalid segment", proto.Printable(proto.Truncate(s, 80)))
		}
	}
	if !ValidOwner(segs[1]) || !ValidName(segs[2]) {
		return Repo{}, "", nil, badRead("%s/%s is not a GitHub repository", proto.Printable(segs[1]), proto.Printable(segs[2]))
	}
	rest := ""
	if len(segs) > 3 {
		rest = "/" + strings.Join(segs[3:], "/")
	}
	q, err := url.ParseQuery(rawq)
	if err != nil {
		return Repo{}, "", nil, badRead("the query of the API path is malformed")
	}
	return Repo{Owner: segs[1], Name: segs[2]}, rest, q, nil
}

// ReadResult is what the facade passes back: GitHub's status and body.
type ReadResult struct {
	Status    int
	Text      bool
	Body      []byte
	Truncated bool
	Note      string
}

func resultOf(r *Response, text bool) *ReadResult {
	out := &ReadResult{Status: r.Status, Text: text, Body: r.Body, Truncated: r.Truncated}
	if r.Truncated {
		out.Note = fmt.Sprintf("the answer was longer than %d MiB and was cut there", MaxResponse>>20)
	}
	return out
}

// Read performs a plan: one GET request, or a few for the combined views
// (a pull request or issue with its comments, a run with its jobs, a run's
// job logs).
func (c *Client) Read(ctx context.Context, pl Plan) (*ReadResult, error) {
	get := func(path string, q url.Values, accept string, max int64, keepEnd bool) (*Response, error) {
		return c.do(ctx, http.MethodGet, RepoPath(pl.Repo, path), q, accept, nil, max, keepEnd)
	}
	list := func() url.Values {
		q := url.Values{"per_page": {strconv.Itoa(pl.Limit)}}
		if pl.State != "" {
			q.Set("state", pl.State)
		}
		if pl.Branch != "" {
			q.Set("branch", pl.Branch)
		}
		return q
	}
	n := strconv.FormatInt(pl.Number, 10)
	switch pl.Op {
	case proto.GitHubPRList:
		r, err := get("/pulls", list(), acceptJSON, MaxResponse, false)
		return orErr(r, err, false)
	case proto.GitHubIssueList:
		r, err := get("/issues", list(), acceptJSON, MaxResponse, false)
		return orErr(r, err, false)
	case proto.GitHubRunList:
		r, err := get("/actions/runs", list(), acceptJSON, MaxResponse, false)
		return orErr(r, err, false)
	case proto.GitHubPRView:
		if pl.Diff {
			r, err := get("/pulls/"+n, nil, acceptDiff, MaxResponse, false)
			return orErr(r, err, true)
		}
		if !pl.Comments {
			r, err := get("/pulls/"+n, nil, acceptJSON, MaxResponse, false)
			return orErr(r, err, false)
		}
		return c.combine(get, "pull_request", "/pulls/"+n, "comments", "/issues/"+n+"/comments")
	case proto.GitHubIssueView:
		if !pl.Comments {
			r, err := get("/issues/"+n, nil, acceptJSON, MaxResponse, false)
			return orErr(r, err, false)
		}
		return c.combine(get, "issue", "/issues/"+n, "comments", "/issues/"+n+"/comments")
	case proto.GitHubRunView:
		if pl.Log {
			return c.runLogs(get, n, pl.Failed)
		}
		return c.combine(get, "run", "/actions/runs/"+n, "jobs", "/actions/runs/"+n+"/jobs?per_page=100")
	case proto.GitHubAPI:
		r, err := get(pl.Path, pl.Query, acceptJSON, MaxResponse, false)
		if err != nil {
			return nil, err
		}
		return resultOf(r, !isJSON(r.ContentType)), nil
	}
	return nil, badRead("unknown operation %q", pl.Op)
}

type getFunc func(path string, q url.Values, accept string, max int64, keepEnd bool) (*Response, error)

func orErr(r *Response, err error, text bool) (*ReadResult, error) {
	if err != nil {
		return nil, err
	}
	if !r.OK() {
		text = !isJSON(r.ContentType)
	}
	return resultOf(r, text), nil
}

func isJSON(ct string) bool {
	return ct == "" || strings.Contains(ct, "json")
}

// combine reads two JSON documents and passes them back together as
// {"<k1>": first, "<k2>": second}, each as received. A failure of either
// is passed back as it came.
func (c *Client) combine(get getFunc, k1, p1, k2, p2 string) (*ReadResult, error) {
	half := int64(MaxResponse/2 - 64)
	parts := make([]*Response, 2)
	for i, p := range []string{p1, p2} {
		path, rawq, _ := strings.Cut(p, "?")
		q, _ := url.ParseQuery(rawq)
		r, err := get(path, q, acceptJSON, half, false)
		if err != nil {
			return nil, err
		}
		if !r.OK() {
			return orErr(r, nil, false)
		}
		if r.Truncated {
			return &ReadResult{Status: r.Status, Body: r.Body, Truncated: true,
				Note: fmt.Sprintf("the %s were longer than %d MiB; only that part is shown, cut there", strings.ReplaceAll(k1, "_", " "), half>>20)}, nil
		}
		parts[i] = r
	}
	var b bytes.Buffer
	b.WriteString(`{"` + k1 + `":`)
	b.Write(bytes.TrimSpace(parts[0].Body))
	b.WriteString(`,"` + k2 + `":`)
	b.Write(bytes.TrimSpace(parts[1].Body))
	b.WriteString("}")
	if !json.Valid(b.Bytes()) {
		return nil, errors.New("GitHub's answer is not valid JSON")
	}
	return &ReadResult{Status: http.StatusOK, Body: b.Bytes()}, nil
}

// runLogs gathers the logs of a run's jobs (only the failed ones with
// failed), each under a header line, keeping the last MaxResponse bytes:
// the end of a log is where a failure shows.
func (c *Client) runLogs(get getFunc, run string, failed bool) (*ReadResult, error) {
	r, err := get("/actions/runs/"+run+"/jobs", url.Values{"per_page": {strconv.Itoa(maxJobs)}}, acceptJSON, maxAPIBody*4, false)
	if err != nil {
		return nil, err
	}
	if !r.OK() {
		return orErr(r, nil, false)
	}
	var jobs struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	if r.Truncated || json.Unmarshal(r.Body, &jobs) != nil {
		return nil, errors.New("GitHub's list of the run's jobs is not the expected JSON")
	}
	var out []byte
	var dropped int64
	add := func(b []byte) {
		out = append(out, b...)
		if over := int64(len(out)) - MaxResponse; over > 0 {
			dropped += over
			out = append(out[:0], out[over:]...)
		}
	}
	shown := 0
	for _, j := range jobs.Jobs {
		if failed && j.Conclusion != "failure" {
			continue
		}
		shown++
		state := j.Conclusion
		if state == "" {
			state = j.Status
		}
		add([]byte(fmt.Sprintf("=== job %s (%s) ===\n", proto.Printable(proto.Truncate(j.Name, 200)), proto.Printable(state))))
		lr, err := get("/actions/jobs/"+strconv.FormatInt(j.ID, 10)+"/logs", nil, acceptJSON, MaxResponse, true)
		switch {
		case err != nil:
			add([]byte("(no log: " + err.Error() + ")\n"))
		case !lr.OK():
			add([]byte("(no log: " + ErrorOf(lr).Error() + ")\n"))
		default:
			dropped += lr.Dropped
			add(lr.Body)
			if len(lr.Body) > 0 && lr.Body[len(lr.Body)-1] != '\n' {
				add([]byte("\n"))
			}
		}
	}
	if shown == 0 {
		if failed {
			add([]byte("no job of this run failed\n"))
		} else {
			add([]byte("this run has no jobs\n"))
		}
	}
	res := &ReadResult{Status: http.StatusOK, Text: true, Body: out}
	if dropped > 0 {
		res.Truncated = true
		res.Note = fmt.Sprintf("the logs were longer than %d MiB: the first %d bytes were cut", MaxResponse>>20, dropped)
		head := []byte("[devc: " + res.Note + "]\n")
		if len(out)+len(head) > MaxResponse {
			out = out[len(out)+len(head)-MaxResponse:]
		}
		res.Body = append(head, out...)
	}
	return res, nil
}
