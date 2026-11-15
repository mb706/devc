package boxside

// The box side of the SSH gate (work order C10b section 3). The generated
// /etc/devc/ssh_config gives every granted host a ProxyCommand,
// `devc box gate-connect <host>`, which opens a host.gate stream through
// the box agent and relays ssh's bytes; the stream ends at the gate's
// sshd on the host. The box never holds a credential for the remote host
// and never sees the master connection.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// SSHConfigFile is the generated SSH configuration in a box.
const SSHConfigFile = "/etc/devc/ssh_config"

// gateAnswerTimeout bounds the wait for the host's answer to host.gate.
const gateAnswerTimeout = 30 * time.Second

// gateExit is gate-connect's status when no stream could be opened: what
// ssh itself returns for a connection that failed.
const gateExit = 255

var gateHostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// GateConnect relays standard input and output to the gate for host
// through the box agent at addr (AgentAbstract in a box). Problems are one
// line on stderr, which ssh shows, and exit status 255.
func GateConnect(addr, host string, stdin io.Reader, stdout, stderr io.Writer) int {
	if !gateHostRE.MatchString(host) {
		fmt.Fprintf(stderr, "devc: %q is not a valid SSH host alias\n", host)
		return gateExit
	}
	conn, err := openAgentStream(addr, proto.MethodHostGate, proto.GateParams{Host: host}, gateAnswerTimeout)
	var pe *proto.Error
	switch {
	case err == nil:
	case errors.As(err, &pe) && pe.Code == proto.CodeHostUnavailable:
		fmt.Fprintf(stderr, "devc: ssh %s: the SSH gate is not available because the devc daemon on the host is not connected to this box; ask the user to start it ('devc daemon' or 'devc service install')\n", host)
		return gateExit
	case errors.As(err, &pe):
		fmt.Fprintf(stderr, "devc: ssh %s: %s\n", host, proto.Printable(pe.Message))
		return gateExit
	default:
		fmt.Fprintf(stderr, "devc: ssh %s: the box agent does not answer (%s); ask the user to restart the box\n", host, proto.Printable(err.Error()))
		return gateExit
	}
	return relayGit(conn, stdin, stdout)
}

// sshIncludeLine makes the user's SSH configuration read devc's first.
const sshIncludeLine = "Include " + SSHConfigFile

// EnsureSSHInclude puts "Include /etc/devc/ssh_config" at the top of
// ~/.ssh/config unless the line is there already, creating the file (0600)
// and the directory (0700) if needed. ssh uses the first value it finds for
// an option, so devc's blocks for the granted hosts come before anything
// the user's file says.
func EnsureSSHInclude(home string) error {
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	file := filepath.Join(dir, "config")
	if fi, err := os.Lstat(file); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link (a dotfile?); devc did not add %q to it, the system-wide configuration still applies", file, sshIncludeLine)
	}
	old, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		old = nil
	case err != nil:
		return fmt.Errorf("reading %s: %w", file, err)
	}
	sc := bufio.NewScanner(bytes.NewReader(old))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == sshIncludeLine {
			return nil
		}
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(file); err == nil {
		mode = fi.Mode().Perm()
	}
	content := append([]byte(sshIncludeLine+"\n"), old...)
	tmp := file + ".devc-tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return fmt.Errorf("writing %s: %w", file, err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", file, err)
	}
	return nil
}
