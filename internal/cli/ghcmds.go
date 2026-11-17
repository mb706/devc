// This file holds the GitHub commands (work order C15): `devc gh …` in a
// box reads through the host's facade; `devc box git-credential` is the
// box's git credential helper in the token modes; `devc pr create|comment`
// on the host publish a branch and open a pull request, or comment, with
// the host's credential (also what approved pr and comment requests run);
// the start hook gives the box repository its second remote and helper in
// the token modes, and removes them in relay mode.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/github"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
)

// --- in the box: devc gh ---

// ghRead sends one read and prints the answer: raw with asJSON (and for
// text answers), else through format. The read is validated here first,
// with a placeholder for the project's repository, so a mistake is a
// usage error at once; the host validates it again with the real one.
func (a *app) ghRead(cmd *cobra.Command, p proto.GitHubParams, asJSON bool, format func(io.Writer, []byte) error) error {
	if err := requireBox(a.getenv); err != nil {
		return err
	}
	if _, err := github.PlanRead(p, github.Repo{Owner: "x", Name: "x"}, nil); err != nil && !errors.Is(err, github.ErrRepoNotAllowed) {
		return usageErrorf("%s", errText(err))
	}
	rep, body, err := boxside.GitHubRead(boxside.AgentAbstract, p)
	if err != nil {
		return requestError(err)
	}
	if rep.Note != "" {
		fmt.Fprintf(a.stderr, "devc: %s\n", proto.Printable(rep.Note))
	}
	if rep.Status < 200 || rep.Status > 299 {
		if asJSON {
			a.stdout.Write(escapeControls(body))
			fmt.Fprintln(a.stdout)
		}
		msg := github.ErrorOf(&github.Response{Status: rep.Status, Body: body})
		fmt.Fprintf(a.stderr, "devc: %s\n", msg.Error())
		return exitCode(1)
	}
	if rep.Text || asJSON || format == nil {
		out := escapeControls(body)
		if rep.Text {
			out = []byte(proto.PrintableText(string(body)))
		}
		a.stdout.Write(out)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			fmt.Fprintln(a.stdout)
		}
		return nil
	}
	if err := format(a.stdout, body); err != nil {
		return fmt.Errorf("reading GitHub's answer: %w (use --json for the raw answer)", err)
	}
	return nil
}

func newGHCmd(a *app) *cobra.Command {
	var repo string
	var asJSON bool
	common := func(c *cobra.Command) {
		c.Flags().StringVar(&repo, "repo", "", "another repository (owner/name) listed in grants.github_read")
		c.Flags().BoolVar(&asJSON, "json", false, "print GitHub's JSON as it came")
	}
	number := func(s, what string) (int64, error) {
		n, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
		if err != nil || n <= 0 {
			return 0, usageErrorf("%q is not a %s number", printableArg(s), what)
		}
		return n, nil
	}
	var state string
	var limit int
	prList := &cobra.Command{
		Use: "list [--state open|closed|all] [--limit n]", Short: "List pull requests", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.ghRead(cmd, proto.GitHubParams{Op: proto.GitHubPRList, Repo: repo, State: state, Limit: limit}, asJSON, formatPRList)
		},
	}
	var comments, diff bool
	prView := &cobra.Command{
		Use: "view <number> [--comments] [--diff]", Short: "Show a pull request", Args: rangeArgs(1, 1, "devc gh pr view <number>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := number(args[0], "pull request")
			if err != nil {
				return err
			}
			return a.ghRead(cmd, proto.GitHubParams{Op: proto.GitHubPRView, Repo: repo, Number: n, Comments: comments, Diff: diff}, asJSON, formatPRView(comments))
		},
	}
	issueList := &cobra.Command{
		Use: "list [--state open|closed|all] [--limit n]", Short: "List issues", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.ghRead(cmd, proto.GitHubParams{Op: proto.GitHubIssueList, Repo: repo, State: state, Limit: limit}, asJSON, formatIssueList)
		},
	}
	issueView := &cobra.Command{
		Use: "view <number> [--comments]", Short: "Show an issue", Args: rangeArgs(1, 1, "devc gh issue view <number>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := number(args[0], "issue")
			if err != nil {
				return err
			}
			return a.ghRead(cmd, proto.GitHubParams{Op: proto.GitHubIssueView, Repo: repo, Number: n, Comments: comments}, asJSON, formatIssueView(comments))
		},
	}
	var branch string
	runList := &cobra.Command{
		Use: "list [--branch b] [--limit n]", Short: "List workflow runs", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.ghRead(cmd, proto.GitHubParams{Op: proto.GitHubRunList, Repo: repo, Branch: branch, Limit: limit}, asJSON, formatRunList)
		},
	}
	var logs, failed bool
	runView := &cobra.Command{
		Use: "view <run-id> [--log] [--failed]", Short: "Show a workflow run and its jobs; --log shows the jobs' logs", Args: rangeArgs(1, 1, "devc gh run view <run-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := number(args[0], "run")
			if err != nil {
				return err
			}
			return a.ghRead(cmd, proto.GitHubParams{Op: proto.GitHubRunView, Repo: repo, Number: n, Log: logs, Failed: failed}, asJSON, formatRunView)
		},
	}
	api := &cobra.Command{
		Use:   "api <path>",
		Short: "GET a REST path below repos/<owner>/<repo>/ and print the JSON",
		Args:  rangeArgs(1, 1, "devc gh api repos/<owner>/<repo>/<path>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.ghRead(cmd, proto.GitHubParams{Op: proto.GitHubAPI, Path: args[0]}, true, nil)
		},
	}
	for _, c := range []*cobra.Command{prList, prView, issueList, issueView, runList, runView} {
		common(c)
	}
	for _, c := range []*cobra.Command{prList, issueList} {
		c.Flags().StringVar(&state, "state", "", "open (default), closed or all")
	}
	for _, c := range []*cobra.Command{prList, issueList, runList} {
		c.Flags().IntVar(&limit, "limit", 0, "how many (1-100, default 30)")
	}
	prView.Flags().BoolVar(&comments, "comments", false, "also show the comments")
	prView.Flags().BoolVar(&diff, "diff", false, "show the diff instead")
	issueView.Flags().BoolVar(&comments, "comments", false, "also show the comments")
	runList.Flags().StringVar(&branch, "branch", "", "only runs of this branch")
	runView.Flags().BoolVar(&logs, "log", false, "show the jobs' logs (the last 5 MiB)")
	runView.Flags().BoolVar(&failed, "failed", false, "with --log: only the failed jobs")
	cmd := group("gh", "Read pull requests, issues and workflow runs from GitHub (inside a box)", false,
		group("pr", "Pull requests", false, prList, prView),
		group("issue", "Issues", false, issueList, issueView),
		group("run", "Workflow runs", false, runList, runView),
		api)
	cmd.Long = "Read-only access to the project's GitHub repository (and those listed in grants.github_read), served by the " +
		"devc daemon on the host with the user's credential, which never enters the box. To open a pull request or " +
		"comment, use 'devc request pr' or 'devc request comment'. Runs only inside a box."
	return cmd
}

// Readable forms of GitHub's answers. Every field is GitHub's (box-written
// text in practice) and made printable.

type ghObj = map[string]any

func ghStr(m ghObj, path string) string {
	var v any = m
	for _, k := range strings.Split(path, ".") {
		o, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = o[k]
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func decodeJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	return d.Decode(v)
}

func formatPRList(w io.Writer, b []byte) error {
	var list []ghObj
	if err := decodeJSON(b, &list); err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(w, "no pull requests")
		return nil
	}
	tw := table(w, "#", "STATE", "TITLE", "BRANCH", "AUTHOR")
	for _, p := range list {
		st := ghStr(p, "state")
		if ghStr(p, "draft") == "true" {
			st += " (draft)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ghStr(p, "number"), oneLine(st, 20), oneLine(ghStr(p, "title"), 70), oneLine(ghStr(p, "head.ref"), 40), oneLine(ghStr(p, "user.login"), 30))
	}
	return tw.Flush()
}

func formatIssueList(w io.Writer, b []byte) error {
	var list []ghObj
	if err := decodeJSON(b, &list); err != nil {
		return err
	}
	tw := table(w, "#", "STATE", "TITLE", "LABELS", "AUTHOR")
	n := 0
	for _, p := range list {
		if _, isPR := p["pull_request"]; isPR {
			continue // GitHub lists pull requests among issues
		}
		n++
		var labels []string
		if ls, ok := p["labels"].([]any); ok {
			for _, l := range ls {
				if lm, ok := l.(map[string]any); ok {
					labels = append(labels, ghStr(lm, "name"))
				}
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ghStr(p, "number"), oneLine(ghStr(p, "state"), 10), oneLine(ghStr(p, "title"), 70), dash(oneLine(strings.Join(labels, ","), 40)), oneLine(ghStr(p, "user.login"), 30))
	}
	if n == 0 {
		fmt.Fprintln(w, "no issues")
		return nil
	}
	return tw.Flush()
}

func printBody(w io.Writer, s string) {
	s = strings.TrimSpace(proto.PrintableText(strings.ReplaceAll(s, "\r\n", "\n")))
	if s == "" {
		s = "(no description)"
	}
	fmt.Fprintf(w, "\n%s\n", s)
}

func printComments(w io.Writer, list []ghObj) {
	fmt.Fprintf(w, "\n--- %d comment(s) ---\n", len(list))
	for _, c := range list {
		fmt.Fprintf(w, "\n%s at %s:\n%s\n", oneLine(ghStr(c, "user.login"), 40), oneLine(ghStr(c, "created_at"), 30),
			strings.TrimSpace(proto.PrintableText(strings.ReplaceAll(ghStr(c, "body"), "\r\n", "\n"))))
	}
}

func formatPRView(withComments bool) func(io.Writer, []byte) error {
	return func(w io.Writer, b []byte) error {
		var p ghObj
		var comments []ghObj
		if withComments {
			var both struct {
				PR       ghObj   `json:"pull_request"`
				Comments []ghObj `json:"comments"`
			}
			if err := decodeJSON(b, &both); err != nil {
				return err
			}
			p, comments = both.PR, both.Comments
		} else if err := decodeJSON(b, &p); err != nil {
			return err
		}
		state := ghStr(p, "state")
		if ghStr(p, "merged") == "true" {
			state = "merged"
		} else if ghStr(p, "draft") == "true" {
			state += " (draft)"
		}
		fmt.Fprintf(w, "#%s %s\n", ghStr(p, "number"), oneLine(ghStr(p, "title"), 200))
		fmt.Fprintf(w, "%s · %s wants to merge %s into %s\n", oneLine(state, 30), oneLine(ghStr(p, "user.login"), 40), oneLine(ghStr(p, "head.ref"), 80), oneLine(ghStr(p, "base.ref"), 80))
		fmt.Fprintln(w, oneLine(ghStr(p, "html_url"), 300))
		printBody(w, ghStr(p, "body"))
		if withComments {
			printComments(w, comments)
		}
		return nil
	}
}

func formatIssueView(withComments bool) func(io.Writer, []byte) error {
	return func(w io.Writer, b []byte) error {
		var p ghObj
		var comments []ghObj
		if withComments {
			var both struct {
				Issue    ghObj   `json:"issue"`
				Comments []ghObj `json:"comments"`
			}
			if err := decodeJSON(b, &both); err != nil {
				return err
			}
			p, comments = both.Issue, both.Comments
		} else if err := decodeJSON(b, &p); err != nil {
			return err
		}
		fmt.Fprintf(w, "#%s %s\n", ghStr(p, "number"), oneLine(ghStr(p, "title"), 200))
		fmt.Fprintf(w, "%s · opened by %s\n", oneLine(ghStr(p, "state"), 20), oneLine(ghStr(p, "user.login"), 40))
		fmt.Fprintln(w, oneLine(ghStr(p, "html_url"), 300))
		printBody(w, ghStr(p, "body"))
		if withComments {
			printComments(w, comments)
		}
		return nil
	}
}

func runState(r ghObj) string {
	if c := ghStr(r, "conclusion"); c != "" {
		return c
	}
	return ghStr(r, "status")
}

func formatRunList(w io.Writer, b []byte) error {
	var out struct {
		Runs []ghObj `json:"workflow_runs"`
	}
	if err := decodeJSON(b, &out); err != nil {
		return err
	}
	if len(out.Runs) == 0 {
		fmt.Fprintln(w, "no workflow runs")
		return nil
	}
	tw := table(w, "ID", "STATE", "WORKFLOW", "TITLE", "BRANCH", "EVENT", "CREATED")
	for _, r := range out.Runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ghStr(r, "id"), oneLine(runState(r), 20), oneLine(ghStr(r, "name"), 30), oneLine(ghStr(r, "display_title"), 50),
			oneLine(ghStr(r, "head_branch"), 30), oneLine(ghStr(r, "event"), 20), oneLine(ghStr(r, "created_at"), 25))
	}
	return tw.Flush()
}

func formatRunView(w io.Writer, b []byte) error {
	var out struct {
		Run  ghObj `json:"run"`
		Jobs struct {
			Jobs []ghObj `json:"jobs"`
		} `json:"jobs"`
	}
	if err := decodeJSON(b, &out); err != nil {
		return err
	}
	r := out.Run
	fmt.Fprintf(w, "run %s: %s (%s)\n", ghStr(r, "id"), oneLine(ghStr(r, "display_title"), 200), oneLine(runState(r), 20))
	fmt.Fprintf(w, "workflow %s on %s (%s, %s)\n", oneLine(ghStr(r, "name"), 60), oneLine(ghStr(r, "head_branch"), 60), oneLine(ghStr(r, "event"), 20), oneLine(ghStr(r, "head_sha"), 40))
	fmt.Fprintln(w, oneLine(ghStr(r, "html_url"), 300))
	fmt.Fprintln(w)
	for _, j := range out.Jobs.Jobs {
		fmt.Fprintf(w, "job %s: %s\n", oneLine(ghStr(j, "name"), 100), oneLine(runState(j), 20))
		if steps, ok := j["steps"].([]any); ok {
			for _, s := range steps {
				if sm, ok := s.(map[string]any); ok && ghStr(sm, "conclusion") != "success" && ghStr(sm, "conclusion") != "skipped" {
					fmt.Fprintf(w, "  step %s: %s\n", oneLine(ghStr(sm, "name"), 100), oneLine(runState(sm), 20))
				}
			}
		}
	}
	fmt.Fprintln(w, "\n(--log shows the logs, --log --failed those of failed jobs)")
	return nil
}

// newBoxGitCredentialCmd is `devc box git-credential <get|store|erase>`,
// the credential helper git runs for the GitHub host in the token modes.
func newBoxGitCredentialCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:    "git-credential <get|store|erase>",
		Short:  "git credential helper: the project's GitHub token, asked from the daemon",
		Hidden: true,
		Args:   rangeArgs(1, 1, "devc box git-credential <get|store|erase>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			code := boxside.GitCredential(args[0], cmd.InOrStdin(), a.stdout, a.stderr, func() (proto.GitHubTokenReply, error) {
				return boxside.FetchGitHubToken(boxside.AgentAbstract)
			})
			if code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
}

// --- on the host: devc pr ---

// maxBodyFile bounds --body-file.
const maxBodyFile = 64 << 10

// githubClient returns the project's repository and a client with the
// host's credential.
func (a *app) githubClient(ctx context.Context, e *boxEnv, ref names.ProjectRef) (github.Repo, *github.Client, error) {
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		return github.Repo{}, nil, err
	}
	ep, err := github.EndpointsFromEnv(a.getenv)
	if err != nil {
		return github.Repo{}, nil, err
	}
	repo, err := github.ParseRepo(r.Upstream, ep.Web)
	if err != nil {
		return github.Repo{}, nil, fmt.Errorf("%s: %w", ref, err)
	}
	tok, err := github.HostToken(ctx, secrets.NewFileStore(e.cfg.Paths), ref.Realm, a.runner, ep.Web)
	if err != nil {
		return github.Repo{}, nil, err
	}
	return repo, github.NewClient(ep.API, tok), nil
}

// readBody returns --body or the contents of --body-file ("-": standard
// input).
func (a *app) readBody(body, file string) (string, error) {
	if file == "" {
		return body, nil
	}
	if body != "" {
		return "", usageErrorf("give --body or --body-file, not both")
	}
	var rd io.Reader
	if file == "-" {
		rd = a.stdinReader()
	} else {
		f, err := os.Open(file)
		if err != nil {
			return "", err
		}
		defer f.Close()
		rd = f
	}
	b, err := io.ReadAll(io.LimitReader(rd, maxBodyFile+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxBodyFile {
		return "", fmt.Errorf("%s is larger than %d bytes", file, maxBodyFile)
	}
	return string(b), nil
}

// prArgs splits [ref] and the remaining positional arguments: the first is
// the project when it names one, or when there are more than want.
func (a *app) prArgs(e *boxEnv, args []string, want int) (names.ProjectRef, []string, error) {
	var refArgs []string
	if len(args) > want {
		refArgs, args = args[:1], args[1:]
	}
	if len(args) > want {
		return names.ProjectRef{}, nil, usageErrorf("too many arguments")
	}
	ref, err := a.resolveRef(e.cfg, refArgs)
	return ref, args, err
}

type prCreateOptions struct {
	branch, title, body, bodyFile, base string
	draft, yes                          bool
}

func newPRCmd(a *app) *cobra.Command {
	var o prCreateOptions
	create := &cobra.Command{
		Use:   "create [<realm>/<project>] --branch <b> [--title t] [--body b | --body-file f] [--base b] [--draft] [--yes]",
		Short: "Push a branch to the upstream and open a pull request on GitHub",
		Long: "Bank, push the branch to the upstream as 'devc push' does (showing what would change and asking, unless " +
			"--yes), then open the pull request with your GitHub credential (the realm secret github-token, else 'gh auth " +
			"token') and print its URL. The title defaults to the subject of the branch's newest commit, the base to the " +
			"project's default branch. Before the bank, the base itself, the default branch and a protected branch " +
			"(git.protected) are refused; after it (the bank forwards the branches listed in git.forward, as every bank " +
			"does), a branch the relay does not have; only then is anything pushed for the pull request.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.prCreate(cmd, args, o)
		},
	}
	create.Flags().StringVar(&o.branch, "branch", "", "the branch to publish (required)")
	create.Flags().StringVar(&o.title, "title", "", "title (default: the newest commit's subject)")
	create.Flags().StringVar(&o.body, "body", "", "description")
	create.Flags().StringVar(&o.bodyFile, "body-file", "", "read the description from a file (- for standard input)")
	create.Flags().StringVar(&o.base, "base", "", "the branch to merge into (default: the project's default branch)")
	create.Flags().BoolVar(&o.draft, "draft", false, "open it as a draft")
	create.Flags().BoolVar(&o.yes, "yes", false, "push without asking")

	var body, bodyFile string
	comment := &cobra.Command{
		Use:   "comment [<realm>/<project>] <number> --body <text>",
		Short: "Comment on a pull request or issue of the project's GitHub repository",
		Args:  rangeArgs(1, 2, "devc pr comment [<realm>/<project>] <number> --body <text>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.prComment(cmd, args, body, bodyFile)
		},
	}
	comment.Flags().StringVar(&body, "body", "", "the comment")
	comment.Flags().StringVar(&bodyFile, "body-file", "", "read the comment from a file (- for standard input)")
	return group("pr", "Open pull requests and comment on GitHub", false, create, comment)
}

// prCreate is `devc pr create`, also what an approved pr request runs
// (with --yes). It reads the host credential before anything is pushed,
// so a missing credential publishes nothing, then publishes the branch as
// devc push does and opens the pull request from it.
func (a *app) prCreate(cmd *cobra.Command, args []string, o prCreateOptions) error {
	if o.branch == "" {
		return usageErrorf("--branch is required")
	}
	if err := names.CheckRef(o.branch); err != nil {
		return usageErrorf("--branch %q: %v", printableArg(o.branch), err)
	}
	if o.base != "" {
		if err := names.CheckRef(o.base); err != nil {
			return usageErrorf("--base %q: %v", printableArg(o.base), err)
		}
	}
	body, err := a.readBody(o.body, o.bodyFile)
	if err != nil {
		return err
	}
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, err := a.resolveRef(e.cfg, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), gitTimeout)
	defer cancel()
	// Configuration problems refuse publishing, as for devc push (C07r).
	if err := a.refuseProblems(e, ref); err != nil {
		return err
	}
	repo, client, err := a.githubClient(ctx, e, ref)
	if err != nil {
		return err
	}
	title, base, err := a.publishBranch(ctx, e, ref, o)
	if err != nil {
		return err
	}
	if o.title != "" {
		title = o.title
	}
	if o.base != "" {
		base = o.base
	}
	if title == "" {
		title = o.branch
	}
	pr, err := client.CreatePull(ctx, repo, github.NewPull{Title: title, Head: o.branch, Base: base, Body: body, Draft: o.draft})
	if err != nil {
		return fmt.Errorf("opening the pull request: %w", err)
	}
	fmt.Fprintf(a.stderr, "devc: opened pull request #%d on %s: %s into %s\n", pr.Number, repo, o.branch, base)
	fmt.Fprintln(a.stdout, proto.Printable(pr.HTMLURL))
	return nil
}

// publishBranch checks the branch against the base, the default branches
// and git.protected (checkPRBranch) before anything else, banks (which
// forwards the branches listed in git.forward, the owner's standing
// instruction), checks that the relay has the branch (checkPRBanked) and
// only then pushes it as devc push does, with the box lock held (finding
// H2-03). It returns the branch's newest commit subject and the base.
func (a *app) publishBranch(ctx context.Context, e *boxEnv, ref names.ProjectRef, o prCreateOptions) (string, string, error) {
	unlock, err := a.lockBox(ctx, e, ref, 0)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	p, err := a.gitProject(ctx, e, ref)
	if err != nil {
		return "", "", err
	}
	base, err := checkPRBranch(ctx, p, o)
	if err != nil {
		return "", "", err
	}
	if p.Running {
		res, err := e.git.Bank(ctx, p)
		fmt.Fprint(a.stderr, gitx.FormatBank(res, true))
		if err != nil {
			return "", "", fmt.Errorf("banking before the push: %w", err)
		}
	}
	if err := checkPRBanked(ctx, p, o.branch); err != nil {
		return "", "", err
	}
	po := gitx.PushOptions{Branches: []string{o.branch}, Yes: o.yes}
	if a.isTerminal() {
		po.Confirm = a.confirmFunc()
		po.Color = a.stdoutTerminal()
	}
	if err := e.git.Push(ctx, p, po); err != nil {
		if errors.Is(err, gitx.ErrNotConfirmed) {
			fmt.Fprintf(a.stderr, "devc: %v; no pull request was opened\n", err)
			return "", "", exitCode(1)
		}
		return "", "", err
	}
	title := ""
	if res, err := e.git.Host.Run(ctx, gitx.Call{Relay: p.Relay.Dir, Mode: gitx.Display, Args: []string{"log", "-1", "--format=%s", gitx.HeadsPrefix + o.branch, "--"}}); err == nil {
		title = strings.TrimSpace(gitx.Sanitize(res.Stdout))
	}
	return proto.Truncate(title, 256), base, nil
}

// checkPRBranch validates a pull request's branch with what the host knows
// locally, before the bank and before anything is pushed (findings X10 3,
// H2-03): it is neither the pull request's base, nor the project's
// default branch, nor the upstream's default branch as the relay records
// it (its HEAD), nor protected by git.protected. It returns the base:
// --base, else the default branch.
func checkPRBranch(ctx context.Context, p *gitx.Project, o prCreateOptions) (string, error) {
	if !p.Relay.Exists() {
		return "", fmt.Errorf("%s has no relay yet: run 'devc project init %s'; nothing was pushed", p.Ref, p.Ref)
	}
	head, _ := p.Relay.HeadBranch(ctx)
	def := p.Resolved.DefaultBranch
	if def == "" {
		def = head
	}
	if def == "" {
		def = "main"
	}
	base := o.base
	if base == "" {
		base = def
	}
	b := o.branch
	switch {
	case b == base:
		return "", fmt.Errorf("the branch %s is the pull request's base: open it from another branch; nothing was banked or pushed", b)
	case b == def || b == head:
		return "", fmt.Errorf("%s is the default branch of %s's upstream: a pull request publishes work from another branch; nothing was banked or pushed", b, p.Ref)
	case gitx.Protected(p.Resolved.Git.Protected, b):
		return "", fmt.Errorf("%s is a protected branch of %s (git.protected): a pull request publishes work from another branch; nothing was banked or pushed", b, p.Ref)
	}
	return base, nil
}

// checkPRBanked refuses a pull request whose branch the relay does not
// have after the bank: nothing is pushed for it (the bank itself may have
// forwarded branches listed in git.forward).
func checkPRBanked(ctx context.Context, p *gitx.Project, b string) error {
	set, err := p.Relay.RefSet(ctx)
	if err != nil {
		return err
	}
	if set.Heads[b] == "" {
		return fmt.Errorf("the relay of %s has no branch %s: commit it in the box and bank it first ('devc bank'); nothing was pushed for the pull request", p.Ref, b)
	}
	return nil
}

// prComment is `devc pr comment`, also what an approved comment request
// runs: one comment with the host credential, nothing pushed.
func (a *app) prComment(cmd *cobra.Command, args []string, body, bodyFile string) error {
	text, err := a.readBody(body, bodyFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return usageErrorf("--body (or --body-file) is required")
	}
	last := args[len(args)-1]
	n, err := strconv.ParseInt(strings.TrimPrefix(last, "#"), 10, 64)
	if err != nil || n <= 0 {
		return usageErrorf("%q is not a pull request or issue number", printableArg(last))
	}
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, _, err := a.prArgs(e, args, 1)
	if err != nil {
		return err
	}
	if err := a.refuseProblems(e, ref); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()
	repo, client, err := a.githubClient(ctx, e, ref)
	if err != nil {
		return err
	}
	c, err := client.Comment(ctx, repo, n, text)
	if err != nil {
		return fmt.Errorf("commenting on #%d: %w", n, err)
	}
	fmt.Fprintf(a.stderr, "devc: commented on #%d of %s\n", n, repo)
	fmt.Fprintln(a.stdout, proto.Printable(c.HTMLURL))
	return nil
}

// --- the box repository in the token modes ---

// githubHook gives a box that has just started the remote and helper of its
// project's GitHub mode (a box.StartHook).
type githubHook struct {
	a *app
	e *boxEnv
}

var _ box.StartHook = githubHook{}

func (h githubHook) AfterStart(ctx context.Context, ref names.ProjectRef, _ string) error {
	return h.a.configureGitHub(ctx, h.e, ref)
}

// configureGitHub sets up or removes the box repository's remote github and
// credential helper according to grants.github (work order C15). Nothing is
// done for a project without a relay.
func (a *app) configureGitHub(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		return nil
	}
	p := e.git.Project(r, true)
	if !p.Relay.Exists() {
		return nil
	}
	ep, err := github.EndpointsFromEnv(a.getenv)
	if err != nil {
		return err
	}
	on := r.Grants.GitHubTokenMode()
	repo, rerr := github.ParseRepo(r.Upstream, ep.Web)
	if rerr != nil {
		if on {
			return fmt.Errorf("%s: grants.github is %q, but %v; the box gets no remote github", ref, r.Grants.GitHub, rerr)
		}
		return nil
	}
	gr := gitx.GitHubRemote{URL: github.RemoteURL(ep.Web, repo), Scope: github.CredentialScope(ep.Web), Helper: boxside.CredentialHelper}
	if err := e.git.ConfigureGitHub(ctx, p, gr, on); err != nil {
		return fmt.Errorf("%s: setting up the remote github in the box: %w", ref, err)
	}
	return nil
}
