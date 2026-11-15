package boxside

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Check is one self-test item.
type Check struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// SelfTest checks the hardening of the box it runs in by trying things.
// Refused namespace operations must fail with ENOSYS, the errno of the
// seccomp profile's default action: without the seccomp rule the kernel
// would answer differently (EINVAL for a multi-threaded caller or for
// CLONE_NEWUSER|CLONE_FS, or success followed by an AppArmor denial), so
// these items show that the seccomp rule itself is active.
//
// probe runs `devc box probe <name>` in a child process and returns its
// output; it is used for checks that could crash the caller. nil skips them.
func SelfTest(info *Info, probe func(name string) (string, error)) []Check {
	var cs []Check
	add := func(id string, ok bool, format string, args ...any) {
		cs = append(cs, Check{ID: id, OK: ok, Detail: fmt.Sprintf(format, args...)})
	}

	uid, gid := os.Geteuid(), os.Getegid()
	add("identity", uid == 1000 && gid == 1000, "euid %d, egid %d (want 1000, 1000)", uid, gid)

	status, err := procStatus()
	if err != nil {
		add("capabilities", false, "reading /proc/self/status: %v", err)
		add("no-new-privileges", false, "reading /proc/self/status: %v", err)
	} else {
		eff, bnd := status["CapEff"], status["CapBnd"]
		add("capabilities", isZeroHex(eff), "CapEff %s, CapPrm %s, CapBnd %s", eff, status["CapPrm"], bnd)
		add("no-new-privileges", status["NoNewPrivs"] == "1", "NoNewPrivs %s", status["NoNewPrivs"])
	}

	err = unix.Unshare(unix.CLONE_NEWUSER)
	add("userns-unshare", errors.Is(err, unix.ENOSYS), "unshare(CLONE_NEWUSER): %s (want ENOSYS from seccomp)", errText(err))

	// CLONE_NEWUSER|CLONE_FS is rejected by the kernel with EINVAL before
	// any process is created, so this probe is harmless if seccomp lets it
	// through; seccomp answers ENOSYS first.
	err = rawClone(unix.CLONE_NEWUSER | unix.CLONE_FS | uintptr(unix.SIGCHLD))
	add("userns-clone", errors.Is(err, unix.ENOSYS), "clone(CLONE_NEWUSER|CLONE_FS): %s (want ENOSYS from seccomp)", errText(err))

	// clone3 with a NULL argument fails with EFAULT or EINVAL when it is
	// allowed at all.
	_, _, e := unix.Syscall(unix.SYS_CLONE3, 0, 0, 0)
	add("clone3", e == unix.ENOSYS, "clone3: %s (want ENOSYS)", errText(errnoErr(e)))

	ok, detail := socketCheck(unix.AF_ALG, unix.SOCK_SEQPACKET, unix.EAFNOSUPPORT)
	add("af-alg", ok, "%s", detail)
	// The kernel truncates the domain to 32 bits; seccomp sees all 64.
	ok, detail = socketCheck(1<<32|unix.AF_ALG, unix.SOCK_SEQPACKET, 0)
	add("af-alg-high-bits", ok, "%s", detail)
	if probe != nil {
		ok, detail = socketcallCheck(probe)
		add("socketcall", ok, "%s", detail)
	}
	ok, detail = socketsWork()
	add("sockets", ok, "%s", detail)
	ok, detail = nftablesCheck()
	add("nftables", ok, "%s", detail)

	for _, d := range []struct{ id, dir string }{{"toolbox-ro", "/opt/devc"}, {"etc-ro", EtcDir}} {
		err := tryCreate(d.dir)
		add(d.id, err != nil, "creating a file in %s: %s (want a failure)", d.dir, errText(err))
	}
	project := ""
	if info != nil {
		project = info.ProjectDir
	} else if r, p := os.Getenv("DEVC_REALM"), os.Getenv("DEVC_PROJECT"); r != "" && p != "" {
		project = "/w/" + r + "/" + p
	}
	if project == "" {
		add("project-rw", false, "no project directory known (box.json missing and DEVC_REALM/DEVC_PROJECT unset)")
	} else {
		err := tryCreate(project)
		add("project-rw", err == nil, "creating a file in %s: %s", project, errText(err))
	}
	return cs
}

func nativeEndian() binary.ByteOrder { return binary.NativeEndian }

func errText(err error) string {
	if err == nil {
		return "succeeded"
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		if name := unix.ErrnoName(en); name != "" {
			return name + " (" + en.Error() + ")"
		}
	}
	return err.Error()
}

func errnoErr(e syscall.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}

func procStatus() (map[string]string, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m, nil
}

func isZeroHex(s string) bool {
	v, err := strconv.ParseUint(s, 16, 64)
	return err == nil && v == 0
}

// socketCheck tries socket(domain, typ, 0) and expects it to fail with
// want; 0 accepts any failure.
func socketCheck(domain uint64, typ int, want syscall.Errno) (bool, string) {
	fd, _, e := unix.RawSyscall(unix.SYS_SOCKET, uintptr(domain), uintptr(typ), 0)
	if e == 0 {
		unix.Close(int(fd))
		return false, fmt.Sprintf("socket(%#x): succeeded", domain)
	}
	return want == 0 || e == want, fmt.Sprintf("socket(%#x): %s", domain, errText(e))
}

// socketcallCheck: 32-bit programs could create AF_ALG sockets through
// socketcall, whose arguments seccomp cannot inspect, so the profile
// refuses the call with EPERM (the kernel would answer socketcall(0) with
// EINVAL). The probe runs in a child because a kernel without the 32-bit
// entry kills the caller with SIGSEGV.
func socketcallCheck(probe func(string) (string, error)) (bool, string) {
	out, err := probe(ProbeSocketcall)
	out = strings.TrimSpace(out)
	var ee *exec.ExitError
	switch {
	case err == nil && out == "EPERM":
		return true, "socketcall through the 32-bit entry: EPERM (refused by seccomp)"
	case err == nil && out == probeUnsupported:
		return true, "not applicable: no 32-bit x86 system calls on this architecture"
	case errors.As(err, &ee) && ee.ProcessState != nil:
		if ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() && (ws.Signal() == syscall.SIGSEGV || ws.Signal() == syscall.SIGILL) {
			return true, "the kernel has no 32-bit system call entry (" + ws.Signal().String() + "), so socketcall is unreachable"
		}
		return false, fmt.Sprintf("probe failed: %v", err)
	case err != nil:
		return false, fmt.Sprintf("probe failed: %v", err)
	}
	return false, fmt.Sprintf("socketcall through the 32-bit entry: %s (want EPERM from seccomp)", out)
}

// Probe names for `devc box probe`.
const (
	ProbeSocketcall  = "socketcall"
	probeUnsupported = "unsupported"
)

// RunProbe is the child side of a probe: it prints the outcome on out.
func RunProbe(name string, out io.Writer) error {
	switch name {
	case ProbeSocketcall:
		e, ok := probeSocketcall()
		switch {
		case !ok:
			fmt.Fprintln(out, probeUnsupported)
		case e == 0:
			fmt.Fprintln(out, "succeeded")
		default:
			n := unix.ErrnoName(syscall.Errno(e))
			if n == "" {
				n = fmt.Sprintf("errno %d", e)
			}
			fmt.Fprintln(out, n)
		}
		return nil
	}
	return fmt.Errorf("unknown probe %q", name)
}

// ChildProbe returns a probe function that runs `<exe> box probe <name>`.
func ChildProbe(exe string) func(string) (string, error) {
	return func(name string) (string, error) {
		cmd := exec.Command(exe, "box", "probe", name)
		cmd.Dir = "/"
		out, err := cmd.Output()
		return string(out), err
	}
}

func socketsWork() (bool, string) {
	var bad []string
	for _, s := range []struct {
		name   string
		domain int
		typ    int
	}{{"AF_UNIX", unix.AF_UNIX, unix.SOCK_STREAM}, {"AF_INET", unix.AF_INET, unix.SOCK_DGRAM}, {"AF_NETLINK", unix.AF_NETLINK, unix.SOCK_RAW}} {
		fd, err := unix.Socket(s.domain, s.typ|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			bad = append(bad, s.name+": "+errText(err))
			continue
		}
		unix.Close(fd)
	}
	if len(bad) > 0 {
		return false, "ordinary sockets refused: " + strings.Join(bad, ", ")
	}
	return true, "AF_UNIX, AF_INET and AF_NETLINK sockets work"
}

func tryCreate(dir string) error {
	p := filepath.Join(dir, fmt.Sprintf(".devc-selftest-%d", os.Getpid()))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(p)
}

// Netlink constants of nftables (linux/netfilter/nfnetlink.h,
// nf_tables.h).
const (
	nfnlSubsysNftables = 10
	nftMsgNewTable     = 0
	nftMsgGetTable     = 1
)

// nftablesCheck asks the kernel, over netlink, to list the nftables
// tables and to create one. Both must be refused with EPERM: without
// CAP_NET_ADMIN a box process can neither read nor change the firewall
// rules devc installs in its network namespace.
func nftablesCheck() (bool, string) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		return true, "netfilter netlink socket refused: " + errText(err)
	}
	defer unix.Close(fd)
	tv := unix.Timeval{Sec: 2}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return true, "netfilter netlink bind refused: " + errText(err)
	}
	var results []string
	ok := true
	for i, m := range []struct {
		name  string
		typ   uint16
		flags uint16
	}{
		{"list", nfnlSubsysNftables<<8 | nftMsgGetTable, unix.NLM_F_REQUEST | unix.NLM_F_DUMP},
		{"create", nfnlSubsysNftables<<8 | nftMsgNewTable, unix.NLM_F_REQUEST | unix.NLM_F_ACK | unix.NLM_F_CREATE},
	} {
		e := netlinkAsk(fd, m.typ, m.flags, uint32(i+1))
		ok = ok && e == unix.EPERM
		results = append(results, m.name+": "+errText(errnoErr(e)))
	}
	return ok, "nftables " + strings.Join(results, ", ") + " (want EPERM)"
}

// netlinkAsk sends one netfilter message (header and an nfgenmsg for
// AF_INET) and returns the error the kernel answers with; 0 means it did
// not refuse.
func netlinkAsk(fd int, typ, flags uint16, seq uint32) syscall.Errno {
	msg := make([]byte, unix.NLMSG_HDRLEN+4)
	ne := nativeEndian()
	ne.PutUint32(msg[0:], uint32(len(msg)))
	ne.PutUint16(msg[4:], typ)
	ne.PutUint16(msg[6:], flags)
	ne.PutUint32(msg[8:], seq)
	msg[unix.NLMSG_HDRLEN] = unix.AF_INET
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		var en syscall.Errno
		if errors.As(err, &en) {
			return en
		}
		return unix.EIO
	}
	buf := make([]byte, 65536)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			var en syscall.Errno
			if errors.As(err, &en) {
				return en
			}
			return unix.EIO
		}
		for off := 0; off+unix.NLMSG_HDRLEN <= n; {
			l := int(ne.Uint32(buf[off:]))
			t := ne.Uint16(buf[off+4:])
			if l < unix.NLMSG_HDRLEN || off+l > n {
				return unix.EIO
			}
			switch t {
			case unix.NLMSG_ERROR:
				if l < unix.NLMSG_HDRLEN+4 {
					return unix.EIO
				}
				code := int32(ne.Uint32(buf[off+unix.NLMSG_HDRLEN:]))
				return syscall.Errno(-code)
			case unix.NLMSG_DONE:
				return 0
			}
			off += (l + 3) &^ 3
		}
	}
}
