package boxside

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
)

func TestGitCredential(t *testing.T) {
	exp := time.Unix(1_900_000_000, 0)
	tok := proto.GitHubTokenReply{Token: "ghs_x", Username: "x-access-token", Protocol: "https", Host: "github.com", Expires: &exp}
	calls := 0
	fetch := func() (proto.GitHubTokenReply, error) { calls++; return tok, nil }
	run := func(action, in string, f func() (proto.GitHubTokenReply, error)) (string, string) {
		var out, errb bytes.Buffer
		if code := GitCredential(action, strings.NewReader(in), &out, &errb, f); code != 0 {
			t.Errorf("%s: exit %d", action, code)
		}
		return out.String(), errb.String()
	}
	out, _ := run("get", "protocol=https\nhost=github.com\npath=acme/app.git\n\n", fetch)
	if out != "protocol=https\nhost=github.com\nusername=x-access-token\npassword=ghs_x\npassword_expiry_utc=1900000000\n" {
		t.Errorf("get: %q", out)
	}
	// Another host: nothing, so git goes on as it would.
	if out, _ := run("get", "protocol=https\nhost=gitlab.com\n\n", fetch); out != "" {
		t.Errorf("another host: %q", out)
	}
	if out, _ := run("get", "protocol=http\nhost=github.com\n\n", fetch); out != "" {
		t.Errorf("another protocol: %q", out)
	}
	// store and erase never ask and store nothing.
	calls = 0
	for _, a := range []string{"store", "erase"} {
		if out, _ := run(a, "protocol=https\nhost=github.com\nusername=u\npassword=p\n\n", fetch); out != "" || calls != 0 {
			t.Errorf("%s: %q, %d calls", a, out, calls)
		}
	}
	// A refusal: a message for the user and quit, so git does not prompt.
	out, errs := run("get", "protocol=https\nhost=github.com\n\n", func() (proto.GitHubTokenReply, error) {
		return proto.GitHubTokenReply{}, proto.Errorf(proto.CodeRefused, "grants.github is relay")
	})
	if out != "quit=1\n" || !strings.Contains(errs, "grants.github is relay") {
		t.Errorf("refused: %q %q", out, errs)
	}
	out, errs = run("get", "protocol=https\nhost=github.com\n\n", func() (proto.GitHubTokenReply, error) {
		return proto.GitHubTokenReply{}, proto.Errorf(proto.CodeHostUnavailable, "x")
	})
	if out != "quit=1\n" || !strings.Contains(errs, "not connected") {
		t.Errorf("no daemon: %q %q", out, errs)
	}
}

func TestLaunchGitHubToken(t *testing.T) {
	l := fakeLaunch()
	root := launchRoot(t, l)
	l.GitHubToken = true
	var errb bytes.Buffer
	asked := 0
	e := LaunchEnv{Root: root, Environ: []string{"PATH=/usr/bin"}, Stderr: &errb, NewSession: func() string { return "s0000000000aaaaaaaaaa" },
		FetchGitHubToken: func() (proto.GitHubTokenReply, error) {
			asked++
			return proto.GitHubTokenReply{Token: "ghs_launch"}, nil
		}}
	p, err := PrepareLaunch(&l, nil, e)
	if err != nil {
		t.Fatal(err)
	}
	if m := envMap(p.Env); m["GH_TOKEN"] != "ghs_launch" || asked != 1 {
		t.Errorf("token mode: %v, asked %d", m, asked)
	}
	e.FetchGitHubToken = func() (proto.GitHubTokenReply, error) {
		return proto.GitHubTokenReply{}, proto.Errorf(proto.CodeRefused, "grants.github changed")
	}
	p, _ = PrepareLaunch(&l, nil, e)
	if m := envMap(p.Env); m["GH_TOKEN"] != "" || !strings.Contains(errb.String(), "without a GitHub token: grants.github changed") {
		t.Errorf("refused: %v %q", m, errb.String())
	}
	// Without the mode nothing is asked.
	l.GitHubToken = false
	asked = 0
	e.FetchGitHubToken = func() (proto.GitHubTokenReply, error) { asked++; return proto.GitHubTokenReply{Token: "x"}, nil }
	p, _ = PrepareLaunch(&l, nil, e)
	if m := envMap(p.Env); m["GH_TOKEN"] != "" || asked != 0 {
		t.Errorf("relay mode: %v, asked %d", m, asked)
	}
}
