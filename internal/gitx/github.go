package gitx

import (
	"context"
	"fmt"
	"strings"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/paths"
)

// GitHubRemote is what a box repository gets in the GitHub token modes
// (work order C15): a second remote "github" at the upstream's HTTPS URL
// and devc's credential helper for the GitHub host, which asks the daemon
// for a token each time git needs one.
type GitHubRemote struct {
	// URL is the remote's URL; Scope the URL the helper is registered for
	// (credential.<Scope>.helper); Helper the helper command.
	URL    string
	Scope  string
	Helper string
}

// ConfigureGitHub sets the remote and helper up in the box repository
// (on), or removes what devc set there before (off). A box without a valid
// repository is left alone; `devc project init` calls it again. The empty
// helper value first resets the helpers of the box's global configuration
// for that host, so git asks devc's helper only.
func (s *Service) ConfigureGitHub(ctx context.Context, p *Project, gr GitHubRemote, on bool) error {
	st, err := s.repoState(ctx, p)
	if err != nil {
		return err
	}
	if st.Repo != boxside.RepoValid {
		return nil
	}
	gitDir := "--git-dir=" + paths.InBoxRepoDir(p.Ref)
	key := "credential." + gr.Scope + ".helper"
	// -z: values end in NUL, so an empty value survives the trimming of
	// the output.
	helpers := func() ([]string, error) {
		code, out, err := s.boxGitCode(ctx, p, "/", gitDir, "config", "-z", "--get-all", key)
		if err != nil || code != 0 {
			return nil, err
		}
		return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
	}
	if on {
		for _, kv := range [][2]string{{"remote.github.url", gr.URL}, {"remote.github.fetch", "+refs/heads/*:refs/remotes/github/*"}} {
			if _, err := s.boxGit(ctx, p, "/", gitDir, "config", "--replace-all", kv[0], kv[1]); err != nil {
				return err
			}
		}
		if cur, err := helpers(); err != nil {
			return err
		} else if len(cur) == 2 && cur[0] == "" && cur[1] == gr.Helper {
			return nil
		}
		if _, _, err := s.boxGitCode(ctx, p, "/", gitDir, "config", "--unset-all", key); err != nil {
			return err
		}
		for _, v := range []string{"", gr.Helper} {
			if _, err := s.boxGit(ctx, p, "/", gitDir, "config", "--add", key, v); err != nil {
				return err
			}
		}
		return nil
	}
	cur, err := helpers()
	if err != nil {
		return err
	}
	ours := len(cur) > 0
	for _, v := range cur {
		ours = ours && (v == "" || v == gr.Helper)
	}
	if ours {
		if _, _, err := s.boxGitCode(ctx, p, "/", gitDir, "config", "--unset-all", key); err != nil {
			return err
		}
	}
	code, url, err := s.boxGitCode(ctx, p, "/", gitDir, "config", "--get", "remote.github.url")
	if err != nil {
		return err
	}
	if code == 0 && url == gr.URL {
		// The section only: the box's refs/remotes/github/* also come from
		// the relay and stay.
		if _, err := s.boxGit(ctx, p, "/", gitDir, "config", "--remove-section", "remote.github"); err != nil {
			return fmt.Errorf("removing the remote github: %w", err)
		}
	}
	return nil
}
