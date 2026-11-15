package boxside

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
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
	for {
		line, ok := readHelperLine(in)
		if !ok || line == "" {
			return 0
		}
		if line == "capabilities" {
			fmt.Fprint(out, "fetch\npush\n\n")
		} else if code, done := stubCommand(line, in, out, stderr, git); done {
			return code
		}
		if err := out.Flush(); err != nil {
			return 1
		}
	}
}

func readHelperLine(in *bufio.Reader) (string, bool) {
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

// stubCommand serves one command of the stub protocol (list, fetch, push);
// done is set when the helper must exit with code.
func stubCommand(line string, in *bufio.Reader, out *bufio.Writer, stderr io.Writer, git Git) (int, bool) {
	switch {
	case line == "list" || line == "list for-push":
		refs, err := git("", nil, nil, "for-each-ref", "--format=%(objectname) %(refname)", "refs/remotes/origin/")
		if err != nil {
			fmt.Fprintf(stderr, "git-remote-devc: listing refs/remotes/origin: %v\n", err)
			return 1, true
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
			l, ok := readHelperLine(in)
			if !ok || l == "" {
				break
			}
		}
		fmt.Fprint(out, "\n")
	case strings.HasPrefix(line, "push "):
		batch := []string{line}
		for {
			l, ok := readHelperLine(in)
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
		return 1, true
	}
	return 0, false
}

// RelayDialer reaches the relay service through the box agent.
type RelayDialer interface {
	// Available reports, quickly, whether the host is connected.
	Available() bool
	// Open asks the host for a git service and returns the stream once the
	// host has accepted. An error that is a *proto.Error carries the
	// host's answer.
	Open(service, session string) (RelayConn, error)
}

// RelayConn is an accepted git stream.
type RelayConn interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// RemoteHelperMain is git-remote-devc (work order C10a section 1). When
// the host is connected it offers git's connect capability: for
// git-upload-pack and git-receive-pack it opens a host.git stream through
// the box agent and relays bytes both ways, and the host runs the service
// on the project's relay (the URL is not used: the host serves the
// project the box belongs to). Without a host it is the C05 stub, and it
// falls back to the stub when the host goes away between the two steps.
func RemoteHelperMain(args []string, stdin io.Reader, stdout, stderr io.Writer, git Git, dial RelayDialer) int {
	if dial == nil || !dial.Available() {
		return RemoteHelper(args, stdin, stdout, stderr, git)
	}
	in := bufio.NewReader(stdin)
	out := bufio.NewWriter(stdout)
	defer out.Flush()
	session := os.Getenv("DEVC_SESSION")
	if !names.ValidSessionID(session) {
		session = ""
	}
	for {
		line, ok := readHelperLine(in)
		if !ok || line == "" {
			return 0
		}
		switch {
		case line == "capabilities":
			// fetch and push stay for the fallback.
			fmt.Fprint(out, "connect\nfetch\npush\n\n")
		case strings.HasPrefix(line, "connect "):
			service := strings.TrimPrefix(strings.TrimPrefix(line, "connect "), "git-")
			if service != "upload-pack" && service != "receive-pack" {
				fmt.Fprintf(stderr, "git-remote-devc: the relay does not offer %q\n", strings.TrimPrefix(line, "connect "))
				return 1
			}
			conn, err := dial.Open(service, session)
			var pe *proto.Error
			switch {
			case err == nil:
			case errors.As(err, &pe) && pe.Code != proto.CodeHostUnavailable:
				fmt.Fprintf(stderr, "git-remote-devc: %s\n", proto.Printable(pe.Message))
				return 1
			default:
				// The host went away: the stub's behaviour.
				fmt.Fprint(out, "fallback\n")
				if err := out.Flush(); err != nil {
					return 1
				}
				continue
			}
			fmt.Fprint(out, "\n")
			if err := out.Flush(); err != nil {
				conn.Close()
				return 1
			}
			return relayGit(conn, in, stdout)
		default:
			if code, done := stubCommand(line, in, out, stderr, git); done {
				return code
			}
		}
		if err := out.Flush(); err != nil {
			return 1
		}
	}
}

// relayGit copies git's side to the host and the host's back until the
// host ends the stream.
func relayGit(conn RelayConn, in io.Reader, stdout io.Writer) int {
	defer conn.Close()
	go func() {
		_, _ = io.Copy(conn, in)
		_ = conn.CloseWrite()
	}()
	if _, err := io.Copy(stdout, conn); err != nil {
		return 1
	}
	return 0
}

// agentRelay dials the relay service through the box agent at Addr.
type agentRelay struct {
	Addr string
}

// DefaultRelayDialer reaches the relay through this box's agent.
func DefaultRelayDialer() RelayDialer { return agentRelay{Addr: AgentAbstract} }

// RelayDialerAt reaches the relay through the agent at addr (tests).
func RelayDialerAt(addr string) RelayDialer { return agentRelay{Addr: addr} }

func (a agentRelay) Available() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return AgentCallAt(ctx, a.Addr, proto.MethodHostPing, nil, nil) == nil
}

// gitAnswerTimeout bounds the wait for the host's answer: it may first
// fetch from the upstream or wait for a bank to finish.
const gitAnswerTimeout = 3 * time.Minute

func (a agentRelay) Open(service, session string) (RelayConn, error) {
	return openAgentStream(a.Addr, proto.MethodHostGit, proto.GitParams{Service: service, Session: session}, gitAnswerTimeout)
}

// openAgentStream opens a stream to the host through the box agent at
// addr with one request, waits up to answerTimeout for the host's answer,
// and returns the stream for the bytes that follow it. An error that is a
// *proto.Error carries the host's answer.
func openAgentStream(addr, method string, p any, answerTimeout time.Duration) (*relayConn, error) {
	c, _, err := openAgentStreamResult(addr, method, p, answerTimeout)
	return c, err
}

// openAgentStreamResult is openAgentStream that also returns the result of
// the host's answer.
func openAgentStreamResult(addr, method string, p any, answerTimeout time.Duration) (*relayConn, json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", addr)
	if err != nil {
		return nil, nil, err
	}
	uc := c.(*net.UnixConn)
	params, _ := json.Marshal(p)
	req, _ := json.Marshal(&proto.RPCMessage{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: method, Params: params})
	_ = uc.SetDeadline(time.Now().Add(answerTimeout))
	if _, err := uc.Write(append(req, '\n')); err != nil {
		uc.Close()
		return nil, nil, err
	}
	br := bufio.NewReaderSize(uc, 64<<10)
	line, err := proto.ReadLine(br, proto.MaxMessage)
	if err != nil {
		uc.Close()
		return nil, nil, err
	}
	m, kind, err := proto.ParseMessage(line)
	if err != nil || kind != proto.KindResponse {
		uc.Close()
		return nil, nil, fmt.Errorf("invalid answer from the host")
	}
	if m.Error != nil {
		uc.Close()
		return nil, nil, m.Error
	}
	_ = uc.SetDeadline(time.Time{})
	return &relayConn{UnixConn: uc, r: br}, m.Result, nil
}

type relayConn struct {
	*net.UnixConn
	r *bufio.Reader
}

func (c *relayConn) Read(b []byte) (int, error) { return c.r.Read(b) }
