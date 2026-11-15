package boxside

// The box side of GitHub (work order C15): `devc gh …` reads through the
// host (host.github: one answer line, then the body), and, in projects that
// grant a token, the launcher and the git credential helper `devc box
// git-credential` ask the host for it each time (host.github.token), so a
// fresh token replaces one that expired. Nothing here stores a token.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// Limits of host.github answers: the host caps bodies at 5 MiB; a little
// slack covers notes.
const (
	githubAnswerTimeout = 4 * time.Minute
	maxGitHubBody       = 5<<20 + 4096
	tokenTimeout        = 15 * time.Second
)

// GitHubRead asks the host for one read through the agent at addr and
// returns its answer and body.
func GitHubRead(addr string, p proto.GitHubParams) (proto.GitHubReply, []byte, error) {
	var rep proto.GitHubReply
	c, raw, err := openAgentStreamResult(addr, proto.MethodHostGitHub, p, githubAnswerTimeout)
	if err != nil {
		return rep, nil, err
	}
	defer c.Close()
	if err := proto.DecodeResult(raw, &rep); err != nil {
		return rep, nil, fmt.Errorf("invalid answer from the host: %w", err)
	}
	if rep.Length < 0 || rep.Length > maxGitHubBody {
		return rep, nil, fmt.Errorf("invalid answer from the host (length %d)", rep.Length)
	}
	_ = c.SetDeadline(time.Now().Add(githubAnswerTimeout))
	body := make([]byte, rep.Length)
	if _, err := io.ReadFull(c, body); err != nil {
		return rep, nil, fmt.Errorf("the host's answer ended early: %w", err)
	}
	return rep, body, nil
}

// FetchGitHubToken asks the host for the project's GitHub token.
func FetchGitHubToken(addr string) (proto.GitHubTokenReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tokenTimeout)
	defer cancel()
	var r proto.GitHubTokenReply
	if err := AgentCallAt(ctx, addr, proto.MethodHostGitHubToken, proto.GitHubTokenParams{}, &r); err != nil {
		return r, err
	}
	if r.Token == "" || strings.ContainsAny(r.Token, " \t\r\n") {
		return r, errors.New("the host sent no usable token")
	}
	return r, nil
}

// CredentialHelper is the path git runs as the box's credential helper for
// the GitHub host (credential.<scope>.helper).
const CredentialHelper = "/opt/devc/bin/devc box git-credential"

// maxCredentialInput bounds what git sends a credential helper.
const maxCredentialInput = 64 << 10

// GitCredential implements the git credential helper protocol for action
// ("get", "store" or "erase"): for get it reads git's description of the
// URL and, when it is the host the token is for, prints the token. A
// refusal is printed on stderr (git shows it) and answered with quit=1, so
// git neither asks other helpers nor prompts. store and erase do nothing:
// tokens are never stored in the box.
func GitCredential(action string, stdin io.Reader, stdout, stderr io.Writer, fetch func() (proto.GitHubTokenReply, error)) int {
	attrs := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(stdin, maxCredentialInput))
	sc.Buffer(make([]byte, 4096), maxCredentialInput)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	if action != "get" {
		return 0
	}
	r, err := fetch()
	if err != nil {
		var pe *proto.Error
		switch {
		case errors.As(err, &pe) && pe.Code == proto.CodeHostUnavailable:
			fmt.Fprintln(stderr, "devc: no GitHub token: the devc daemon on the host is not connected to this box")
		case errors.As(err, &pe):
			fmt.Fprintf(stderr, "devc: no GitHub token: %s\n", proto.Printable(pe.Message))
		default:
			fmt.Fprintf(stderr, "devc: no GitHub token: the box agent does not answer (%s)\n", proto.Printable(err.Error()))
		}
		fmt.Fprintln(stdout, "quit=1")
		return 0
	}
	if (attrs["protocol"] != "" && attrs["protocol"] != r.Protocol) || (attrs["host"] != "" && !strings.EqualFold(attrs["host"], r.Host)) {
		// Not the host the token is for: let git go on as it would.
		return 0
	}
	fmt.Fprintf(stdout, "protocol=%s\nhost=%s\nusername=%s\npassword=%s\n", r.Protocol, r.Host, r.Username, r.Token)
	if r.Expires != nil {
		fmt.Fprintf(stdout, "password_expiry_utc=%s\n", strconv.FormatInt(r.Expires.Unix(), 10))
	}
	return 0
}
