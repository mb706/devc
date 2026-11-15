package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
)

// Limits of API calls.
const (
	// MaxResponse caps what the facade passes to a box.
	MaxResponse = 5 << 20
	// maxAPIBody caps JSON answers devc reads for itself.
	maxAPIBody = 1 << 20
	// requestTimeout bounds one call, logs included.
	requestTimeout = 2 * time.Minute
	maxRedirects   = 5
	// APIVersion is the REST API version devc asks for.
	APIVersion = "2022-11-28"
)

// Media types.
const (
	acceptJSON = "application/vnd.github+json"
	acceptDiff = "application/vnd.github.diff"
)

// Client calls the REST API with one credential.
type Client struct {
	API   *url.URL
	Token string
	HTTP  *http.Client
}

// NewClient returns a client for the API base with a credential sent as a
// bearer token (personal and installation tokens, gh's tokens and App JWTs
// all accept that form).
func NewClient(api *url.URL, token string) *Client {
	c := &Client{API: api, Token: token}
	c.HTTP = &http.Client{Timeout: requestTimeout, CheckRedirect: c.checkRedirect}
	return c
}

// checkRedirect follows a few redirects (job logs redirect to signed
// storage URLs), never to plain http unless the API itself is plain http
// (a test server), and never from https to http. Go's client drops the
// Authorization header itself on a redirect to another host.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" && !(req.URL.Scheme == "http" && c.API.Scheme == "http" && strings.EqualFold(req.URL.Host, c.API.Host)) {
		return fmt.Errorf("refusing a redirect to %s", req.URL.Scheme)
	}
	return nil
}

// Response is an answer as received, capped.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
	// Truncated says the body was longer than the cap; Total is the length
	// read (at most the cap plus one byte unless keepEnd).
	Truncated bool
	Dropped   int64
}

// OK reports a 2xx status.
func (r *Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// endpoint is the API base joined with an already validated path.
func (c *Client) endpoint(path string, query url.Values) *url.URL {
	u := *c.API
	u.Path = c.API.Path + path
	u.RawPath = ""
	u.RawQuery = query.Encode()
	return &u
}

// do sends one request built from parts devc chose. With keepEnd a body
// above max keeps its last max bytes (logs), otherwise its first.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, accept string, body any, max int64, keepEnd bool) (*Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, query).String(), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "devc")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, c.scrubErr(fmt.Errorf("calling GitHub (%s %s): %w", method, path, unwrapURL(err)))
	}
	defer resp.Body.Close()
	out := &Response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type")}
	if keepEnd {
		out.Body, out.Dropped, err = readTail(resp.Body, max)
		out.Truncated = out.Dropped > 0
	} else {
		out.Body, err = io.ReadAll(io.LimitReader(resp.Body, max+1))
		if int64(len(out.Body)) > max {
			out.Body, out.Truncated = out.Body[:max], true
		}
	}
	if err != nil {
		return nil, c.scrubErr(fmt.Errorf("reading GitHub's answer to %s %s: %w", method, path, err))
	}
	out.Body = c.Scrub(out.Body)
	return out, nil
}

// unwrapURL drops the *url.Error wrapper, whose text repeats the URL.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// readTail reads r and keeps its last max bytes; dropped counts the rest.
func readTail(r io.Reader, max int64) ([]byte, int64, error) {
	buf := make([]byte, 0, min(max, 1<<20))
	var dropped int64
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if over := int64(len(buf)) - max; over > 0 {
				dropped += over
				buf = append(buf[:0], buf[over:]...)
			}
		}
		if err == io.EOF {
			return buf, dropped, nil
		}
		if err != nil {
			return nil, 0, err
		}
	}
}

// Scrub replaces the client's credential wherever it appears (defence in
// depth: GitHub never echoes it, but nothing devc passes on may carry it).
func (c *Client) Scrub(b []byte) []byte {
	if c.Token == "" || !bytes.Contains(b, []byte(c.Token)) {
		return b
	}
	return bytes.ReplaceAll(b, []byte(c.Token), []byte("[redacted]"))
}

func (c *Client) scrubErr(err error) error {
	if err == nil || c.Token == "" || !strings.Contains(err.Error(), c.Token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.Token, "[redacted]"))
}

// APIError is a non-2xx answer: the status and GitHub's message.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub answered %d", e.Status)
	}
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

// ErrorOf makes an APIError of a response, with GitHub's message and the
// first validation error's message when there is one.
func ErrorOf(r *Response) *APIError {
	var m struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	e := &APIError{Status: r.Status}
	if json.Unmarshal(r.Body, &m) == nil {
		e.Message = m.Message
		for _, x := range m.Errors {
			detail := x.Message
			if detail == "" && x.Code != "" {
				detail = x.Field + " " + x.Code
			}
			if detail != "" {
				e.Message += " (" + detail + ")"
				break
			}
		}
	}
	e.Message = proto.Printable(proto.Truncate(e.Message, 500))
	return e
}

// Call sends a JSON request and decodes a 2xx JSON answer into out.
func (c *Client) Call(ctx context.Context, method, path string, body, out any) error {
	r, err := c.do(ctx, method, path, nil, acceptJSON, body, maxAPIBody, false)
	if err != nil {
		return err
	}
	if !r.OK() {
		return ErrorOf(r)
	}
	if out == nil {
		return nil
	}
	if r.Truncated {
		return fmt.Errorf("GitHub's answer to %s %s is larger than %d bytes", method, path, maxAPIBody)
	}
	if err := json.Unmarshal(r.Body, out); err != nil {
		return fmt.Errorf("GitHub's answer to %s %s is not the expected JSON: %w", method, path, err)
	}
	return nil
}

// RepoPath is /repos/<owner>/<repo> followed by rest (which starts with "/"
// or is empty).
func RepoPath(r Repo, rest string) string {
	return "/repos/" + r.Owner + "/" + r.Name + rest
}

// PullRequest is what devc reads of a created pull request or comment.
type PullRequest struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
}

// NewPull is the body of POST /repos/{owner}/{repo}/pulls.
type NewPull struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}

// CreatePull opens a pull request.
func (c *Client) CreatePull(ctx context.Context, r Repo, p NewPull) (PullRequest, error) {
	var out PullRequest
	err := c.Call(ctx, http.MethodPost, RepoPath(r, "/pulls"), p, &out)
	return out, err
}

// Comment adds a comment to an issue or pull request.
func (c *Client) Comment(ctx context.Context, r Repo, number int64, body string) (PullRequest, error) {
	var out PullRequest
	err := c.Call(ctx, http.MethodPost, RepoPath(r, fmt.Sprintf("/issues/%d/comments", number)), map[string]string{"body": body}, &out)
	return out, err
}

// HostTokenSecret is the realm secret that holds the host's own GitHub
// credential.
const HostTokenSecret = "github-token"

// ErrNoHostToken: no host credential is available.
var ErrNoHostToken = errors.New("no GitHub credential on the host")

// HostToken returns the host's own credential for a realm: the realm
// secret github-token, otherwise what `gh auth token` prints when gh is
// installed. It never goes to a box.
func HostToken(ctx context.Context, store secrets.Store, realm string, runner execx.Runner, web *url.URL) (string, error) {
	v, err := store.Get(realm, HostTokenSecret)
	switch {
	case err == nil:
		tok := strings.TrimSpace(string(v))
		if !validToken(tok) {
			return "", fmt.Errorf("the secret %s of realm %s is not a usable token (one line of printable characters)", HostTokenSecret, realm)
		}
		return tok, nil
	case !errors.Is(err, secrets.ErrNotFound):
		return "", fmt.Errorf("reading the secret %s of realm %s: %w", HostTokenSecret, realm, err)
	}
	if runner == nil {
		return "", fmt.Errorf("%w: run 'devc secret set %s %s'", ErrNoHostToken, realm, HostTokenSecret)
	}
	res, err := runner.Run(ctx, execx.Cmd{Name: "gh", Args: []string{"auth", "token", "--hostname", web.Hostname()}, Dir: "/",
		Timeout: 15 * time.Second, MaxOutput: 64 << 10, Setsid: true})
	tok := strings.TrimSpace(string(res.Stdout))
	if err != nil || !validToken(tok) {
		return "", fmt.Errorf("%w: run 'devc secret set %s %s' or log in with 'gh auth login'", ErrNoHostToken, realm, HostTokenSecret)
	}
	return tok, nil
}

func validToken(s string) bool {
	if s == "" || len(s) > 4096 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}
