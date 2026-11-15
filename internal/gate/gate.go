// Package gate is the host side of the SSH gate (work order C10b, spec
// 10.2): the master connections that `devc ssh login` keeps in the
// runtime directory, the gate's host key and the per-box keys, the
// configuration of the sshd the daemon starts for every box stream, the
// forced command `devc gate-exec` that this sshd runs, the read-only
// profile, the gate log, and the files a box with a grant gets.
//
// Security rules. A master's control socket never leaves <runtime>/gate,
// which no box mounts: through a control socket a client can ask the
// master for port and socket forwards into the host's namespaces (spec
// 10.2, tested during design). A box only ever reaches `sshd -i` over its
// channel; that sshd has every kind of forwarding switched off, accepts
// only that box's key and runs the forced command for everything. The
// grant is that of the channel the stream arrived on: the box ID in the
// forced command comes from the daemon, never from the box, and gate-exec
// checks the grant again against the current configuration (fail closed,
// README rule 6; rule 1: policy only from the config directory). Host
// commands are argument vectors (rule 3); the box's command reaches the
// remote shell as one argument, neither split nor quoted on the host. Host
// aliases are validated before they reach ssh, so none can pass for an
// option. Box-supplied text is logged truncated and is never interpreted
// on the host. The per-box private key is generated on the runtime tmpfs
// and exists afterwards only in the box's secrets directory (rule 5).
package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/mb706/devc/internal/config"
)

// Fixed names of the gate.
const (
	// SFTPMarker is the "program" of the gate sshd's sftp subsystem. sshd
	// passes it to the forced command in SSH_ORIGINAL_COMMAND, which is how
	// gate-exec tells an sftp request from a command.
	SFTPMarker = "devc-sftp-marker"
	// HostKeyAlias is the name under which a box knows the gate's host key.
	HostKeyAlias = "devc-gate"
	// EnvSSHConfig names an ssh configuration file that every host ssh
	// call of the gate uses with -F (tests); otherwise the user's own
	// configuration applies.
	EnvSSHConfig = "DEVC_SSH_CONFIG"
	// MaxLoggedCommand bounds the command text in the gate log.
	MaxLoggedCommand = 4096
)

// maxSocketPath is the room in sun_path (108 bytes with the NUL); ssh
// first binds a master's socket at <ControlPath>.<16 random characters>
// and links it into place afterwards, so that name must fit too.
const (
	maxSocketPath = 107
	sshTempSuffix = 17
)

// ValidHost reports whether host may be used as an SSH host alias.
func ValidHost(host string) bool { return config.ValidHostAlias(host) }

// CheckHost returns an error for an alias that is not valid.
func CheckHost(host string) error {
	if !ValidHost(host) {
		return fmt.Errorf("%q is not a valid SSH host alias (letters, digits, '.', '_' and '-'; it must not start with '-')", host)
	}
	return nil
}

// hostHash names a host's files in the gate directory: aliases may be up
// to 253 characters long, socket paths only 107 bytes.
func hostHash(host string) string {
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:8])
}

// SocketPath is the control socket of the master connection to host in
// the gate runtime directory dir. It fails when the path would be too long
// for a Unix socket.
func SocketPath(dir, host string) (string, error) {
	if err := CheckHost(host); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "m-"+hostHash(host)+".sock")
	if len(p)+sshTempSuffix > maxSocketPath {
		return "", fmt.Errorf("the control socket path %s is too long for a Unix socket (%d bytes, at most %d): use a shorter runtime directory (DEVC_RUNTIME_DIR)",
			p, len(p), maxSocketPath-sshTempSuffix)
	}
	return p, nil
}

// HostFile records which alias a socket belongs to (for `devc ssh ls`).
func HostFile(dir, host string) string {
	return filepath.Join(dir, "m-"+hostHash(host)+".host")
}
