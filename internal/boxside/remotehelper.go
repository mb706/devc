package boxside

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// RemoteHelperNote is what a push through the stub helper prints.
const RemoteHelperNote = "devc: commits are banked on the host by devc (devc bank); nothing reaches the upstream until you run 'devc push' on the host"

// RemoteHelper is the stub git-remote-devc (work order C05 section 9),
// reached through remote.origin.url = devc::<realm>/<project>. It speaks
// the remote-helper protocol just far enough for ordinary commands:
//
//   - list: the repository's own refs/remotes/origin/*, which the host
//     keeps equal to the relay's branches, presented as the remote's
//     branches, so a fetch finds nothing to transfer;
//   - fetch: succeeds without doing anything;
//   - push: reports success for every branch update and prints one note;
//     deletions are refused, because the relay never deletes a branch.
//
// git runs the helper with GIT_DIR set; refs are read with git itself. It
// returns the process exit status.
func RemoteHelper(args []string, stdin io.Reader, stdout, stderr io.Writer, git Git) int {
	in := bufio.NewReader(stdin)
	out := bufio.NewWriter(stdout)
	defer out.Flush()
	readLine := func() (string, bool) {
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return "", false
		}
		return strings.TrimRight(line, "\r\n"), true
	}
	for {
		line, ok := readLine()
		if !ok {
			return 0
		}
		switch {
		case line == "":
			return 0
		case line == "capabilities":
			fmt.Fprint(out, "fetch\npush\n\n")
		case line == "list" || line == "list for-push":
			refs, err := git("", nil, nil, "for-each-ref", "--format=%(objectname) %(refname)", "refs/remotes/origin/")
			if err != nil {
				fmt.Fprintf(stderr, "git-remote-devc: listing refs/remotes/origin: %v\n", err)
				return 1
			}
			for _, l := range strings.Split(string(refs), "\n") {
				oid, name, ok := strings.Cut(l, " ")
				if !ok {
					continue
				}
				b := strings.TrimPrefix(name, "refs/remotes/origin/")
				if b == "HEAD" || b == name {
					continue
				}
				fmt.Fprintf(out, "%s refs/heads/%s\n", oid, b)
			}
			fmt.Fprint(out, "\n")
		case strings.HasPrefix(line, "fetch "):
			// Consume the batch; everything is already here.
			for {
				l, ok := readLine()
				if !ok || l == "" {
					break
				}
			}
			fmt.Fprint(out, "\n")
		case strings.HasPrefix(line, "push "):
			batch := []string{line}
			for {
				l, ok := readLine()
				if !ok || l == "" {
					break
				}
				batch = append(batch, l)
			}
			updated := false
			for _, l := range batch {
				spec := strings.TrimPrefix(strings.TrimPrefix(l, "push "), "+")
				src, dst, ok := strings.Cut(spec, ":")
				switch {
				case !ok || dst == "":
					fmt.Fprintf(out, "error %s invalid push command\n", dst)
				case src == "":
					fmt.Fprintf(out, "error %s devc keeps every branch in the relay; delete it in the box only\n", dst)
				default:
					fmt.Fprintf(out, "ok %s\n", dst)
					updated = true
				}
			}
			fmt.Fprint(out, "\n")
			if updated {
				fmt.Fprintln(stderr, RemoteHelperNote)
			}
		default:
			fmt.Fprintf(stderr, "git-remote-devc: unsupported command %q\n", line)
			return 1
		}
		if err := out.Flush(); err != nil {
			return 1
		}
	}
}
