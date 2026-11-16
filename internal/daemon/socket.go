// This file implements the daemon's checks on a box's agent socket: opening
// it without following a symlink and verifying the process that listens on
// it.

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Errors of DialBoxSocket that say what is at the path.
var (
	// ErrNoSocket: nothing is there (a box without an agent).
	ErrNoSocket = errors.New("no agent socket")
	// ErrNotSocket: something that is not a socket is there (a symlink, a
	// file, a directory): refused, never followed.
	ErrNotSocket = errors.New("not a socket")
)

// DialBoxSocket connects to the Unix socket name in dir, which a box
// controls. The entry is opened with O_PATH|O_NOFOLLOW beneath a descriptor
// of dir, must be a socket by fstat, and is connected through
// /proc/self/fd/<n>, so the connection reaches exactly the inode checked: a
// symlink, a regular file or a directory is refused, and a rename between
// the check and the connect cannot redirect it. dir itself is created by
// the host (the box's runtime directory) and may not be a symlink either.
func DialBoxSocket(ctx context.Context, dir, name string) (*net.UnixConn, error) {
	if name == "" || strings.Contains(name, "/") || name == "." || name == ".." {
		return nil, fmt.Errorf("invalid socket name %q", name)
	}
	dfd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("%s: %w", dir, ErrNoSocket)
		}
		return nil, fmt.Errorf("opening %s: %w", dir, err)
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat(dfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, name), ErrNoSocket)
		}
		return nil, fmt.Errorf("opening %s: %w", filepath.Join(dir, name), err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("checking %s: %w", filepath.Join(dir, name), err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, fmt.Errorf("%s is a %s: %w", filepath.Join(dir, name), fileType(st.Mode), ErrNotSocket)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", "/proc/self/fd/"+strconv.Itoa(fd))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", filepath.Join(dir, name), err)
	}
	return c.(*net.UnixConn), nil
}

func fileType(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return "symlink"
	case unix.S_IFREG:
		return "regular file"
	case unix.S_IFDIR:
		return "directory"
	case unix.S_IFIFO:
		return "FIFO"
	case unix.S_IFCHR, unix.S_IFBLK:
		return "device"
	}
	return "file of an unknown type"
}

// PeerCred returns the credentials of a Unix socket's peer as the kernel
// recorded them (for a connecting client: those of the process that called
// listen), with the PID translated into this process's PID namespace.
func PeerCred(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, cerr
}

var containerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxCgroupFile bounds /proc/<pid>/cgroup.
const maxCgroupFile = 64 << 10

// CheckPeer verifies that pid is a process of the container containerID:
// its cgroup path (read in procDir, normally /proc) must have a component
// libpod-<id> or libpod-<id>.scope (the engine's conmon, in
// libpod-conmon-<id>.scope, is outside the box and does not match). When
// mainPID is known and both PID namespaces can be read, the peer must also
// share the PID namespace of the container's main process.
func CheckPeer(procDir string, pid int32, containerID string, mainPID int) error {
	if pid <= 0 {
		return errors.New("the peer's process is not visible from the host")
	}
	if !containerIDRE.MatchString(containerID) {
		return fmt.Errorf("invalid container ID %q", containerID)
	}
	b, err := readBounded(filepath.Join(procDir, strconv.Itoa(int(pid)), "cgroup"), maxCgroupFile)
	if err != nil {
		return fmt.Errorf("reading the cgroup of the peer (pid %d): %w", pid, err)
	}
	if !cgroupHasContainer(b, containerID) {
		return fmt.Errorf("the peer (pid %d) is not a process of the box's container", pid)
	}
	if mainPID > 0 {
		peerNS, err1 := os.Readlink(filepath.Join(procDir, strconv.Itoa(int(pid)), "ns", "pid"))
		boxNS, err2 := os.Readlink(filepath.Join(procDir, strconv.Itoa(mainPID), "ns", "pid"))
		if err1 == nil && err2 == nil && peerNS != boxNS {
			return fmt.Errorf("the peer (pid %d) is not in the box's PID namespace", pid)
		}
	}
	return nil
}

// cgroupHasContainer reports whether a /proc/<pid>/cgroup file places the
// process in the container's cgroup on some hierarchy.
func cgroupHasContainer(b []byte, id string) bool {
	want1, want2 := "libpod-"+id, "libpod-"+id+".scope"
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, comp := range strings.Split(parts[2], "/") {
			if comp == want1 || comp == want2 {
				return true
			}
		}
	}
	return false
}

func readBounded(file string, max int64) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", file, max)
	}
	return b, nil
}
