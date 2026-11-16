// This file binds the readiness of a box of level internet to the one start
// whose namespace the host filtered: the record the host writes, the
// host's and box init's ways of computing it, and the /proc reads behind
// them.

package netpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/execx"
)

// BindingFile is the name of the binding record in the box's generated
// /etc/devc directory, which the box sees read-only.
const BindingFile = "netns.json"

// BindingVersion is the format of the record.
const BindingVersion = 1

// Binding ties the readiness of a box of level internet to the one start
// whose network namespace devc filtered (finding B1). The host writes it
// after the rules are installed and verified, between init and start; box
// init refuses to report ready unless its own namespace, its PID 1 and the
// boot match. A plain `podman start` creates a new namespace and a new
// main process: the engine hands out namespace inode numbers again after a
// restart (seen on the development host: the same number four times in a
// row), so the inode alone would not do, but the start time of PID 1
// within the same boot is unique to one start. The box cannot write the
// record.
type Binding struct {
	V int `json:"v"`
	// NetNS is the inode number of the network namespace.
	NetNS uint64 `json:"netns"`
	// PID1Start is the start time of the box's main process (field 22 of
	// /proc/<pid>/stat: clock ticks since boot).
	PID1Start uint64 `json:"pid1_start"`
	// BootID is the kernel's boot ID.
	BootID string `json:"boot_id"`
}

// Equal reports whether two bindings name the same start.
func (b Binding) Equal(o Binding) bool {
	return b.V == o.V && b.NetNS == o.NetNS && b.PID1Start == o.PID1Start && b.BootID == o.BootID
}

// valid refuses a record that could match by accident: another format, or
// a zero inode, start time or boot ID.
func (b Binding) valid() error {
	if b.V != BindingVersion || b.NetNS == 0 || b.PID1Start == 0 || !validBootID(b.BootID) {
		return errors.New("the network binding record is incomplete")
	}
	return nil
}

func validBootID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'):
		default:
			return false
		}
	}
	return true
}

// maxBinding caps the record.
const maxBinding = 4096

// ParseBinding decodes a record strictly.
func ParseBinding(data []byte) (Binding, error) {
	var b Binding
	if len(data) > maxBinding {
		return b, errors.New("the network binding record is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, fmt.Errorf("the network binding record cannot be parsed: %w", err)
	}
	if dec.More() {
		return b, errors.New("the network binding record has data after it")
	}
	return b, b.valid()
}

// ReadBinding reads a record without following a symlink.
func ReadBinding(path string) (Binding, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Binding{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBinding+1))
	if err != nil {
		return Binding{}, err
	}
	return ParseBinding(data)
}

// WriteBinding writes a record (mode 0644: the box reads it) by rename.
func WriteBinding(path string, b Binding) error {
	if err := b.valid(); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RemoveBinding removes a stale record.
func RemoveBinding(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the old network binding: %w", err)
	}
	return nil
}

// ProcFS reads process facts below a proc file system (Root "/proc"
// normally; tests point it elsewhere).
type ProcFS struct{ Root string }

func (p ProcFS) root() string {
	if p.Root == "" {
		return "/proc"
	}
	return p.Root
}

// StartTime returns field 22 of /proc/<pid>/stat (pid may be "self").
func (p ProcFS) StartTime(pid string) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(p.root(), pid, "stat"))
	if err != nil {
		return 0, err
	}
	return ParseStartTime(b)
}

// ParseStartTime extracts the start time from the content of a stat file.
// The command name (field 2) may contain spaces and parentheses, so the
// fields are counted after its last ')'.
func ParseStartTime(stat []byte) (uint64, error) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, errors.New("unexpected format of a /proc stat file")
	}
	f := strings.Fields(string(stat[i+1:]))
	// f[0] is field 3 (state); the start time is field 22.
	if len(f) < 20 {
		return 0, errors.New("unexpected format of a /proc stat file")
	}
	v, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil || v == 0 {
		return 0, errors.New("unexpected start time in a /proc stat file")
	}
	return v, nil
}

// NSInode returns the inode number of /proc/<pid>/ns/<ns>.
func (p ProcFS) NSInode(pid, ns string) (uint64, error) {
	return Inode(filepath.Join(p.root(), pid, "ns", ns))
}

// BootID returns the kernel's boot ID.
func (p ProcFS) BootID() (string, error) {
	b, err := os.ReadFile(filepath.Join(p.root(), "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if !validBootID(s) {
		return "", errors.New("unexpected format of the boot ID")
	}
	return s, nil
}

// Inode returns the inode number of a file (following a namespace's magic
// link to the namespace itself).
func Inode(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no inode for %s", path)
	}
	return st.Ino, nil
}

// HostProc is what the host reads about a box's main process; tests
// replace it.
type HostProc interface {
	StartTime(pid int) (uint64, error)
	// NetNS is the inode of the process's network namespace. It may fail
	// with a permission error on hosts that hide other processes.
	NetNS(pid int) (uint64, error)
	BootID() (string, error)
}

type hostProc struct{ fs ProcFS }

func (h hostProc) StartTime(pid int) (uint64, error) { return h.fs.StartTime(strconv.Itoa(pid)) }
func (h hostProc) NetNS(pid int) (uint64, error)     { return h.fs.NSInode(strconv.Itoa(pid), "net") }
func (h hostProc) BootID() (string, error)           { return h.fs.BootID() }

// proc is the host's /proc unless a test replaced it.
func (in *Installer) proc() HostProc {
	if in.Proc != nil {
		return in.Proc
	}
	return hostProc{}
}

// NamespaceInode returns the inode number of the namespace at path, seen
// through the engine's user and mount namespace (where the engine's
// namespace files are mounted), the same way the rules are installed.
func (in *Installer) NamespaceInode(ctx context.Context, namespace string) (uint64, error) {
	if err := checkNamespace(namespace); err != nil {
		return 0, err
	}
	look := in.LookPath
	if look == nil {
		look = execx.LookPath
	}
	stat, err := look("stat")
	if err != nil {
		return 0, fmt.Errorf("stat is needed for network level internet (package coreutils): %w", err)
	}
	res, err := in.Engine.Unshare(ctx, []string{stat, "-L", "-c", "%i", "--", namespace}, nil)
	if err != nil {
		return 0, fmt.Errorf("reading the box's network namespace: %w%s", err, tail(res))
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(res.Stdout)), 10, 64)
	if err != nil || v == 0 {
		return 0, fmt.Errorf("reading the box's network namespace: unexpected answer %q", strings.TrimSpace(string(res.Stdout)))
	}
	return v, nil
}

// Bind computes the binding of a box whose namespace is at namespace and
// whose main process is pid (an initialised or running container). The
// namespace the rules went into must be the main process's.
func (in *Installer) Bind(ctx context.Context, namespace string, pid int) (Binding, error) {
	if pid <= 0 {
		return Binding{}, errors.New("the engine reports no main process for the box, so its network cannot be bound to this start")
	}
	ino, err := in.NamespaceInode(ctx, namespace)
	if err != nil {
		return Binding{}, err
	}
	pr := in.proc()
	// The main process's own namespace, where the host may look at it: it
	// must be the one the rules went into.
	switch own, err := pr.NetNS(pid); {
	case err == nil && own != ino:
		return Binding{}, fmt.Errorf("the box's main process (pid %d) is not in the network namespace the engine reports", pid)
	case err != nil && !errors.Is(err, os.ErrPermission):
		return Binding{}, fmt.Errorf("reading the network namespace of pid %d: %w", pid, err)
	}
	start, err := pr.StartTime(pid)
	if err != nil {
		return Binding{}, fmt.Errorf("reading the start time of pid %d: %w", pid, err)
	}
	boot, err := pr.BootID()
	if err != nil {
		return Binding{}, fmt.Errorf("reading the boot ID: %w", err)
	}
	return Binding{V: BindingVersion, NetNS: ino, PID1Start: start, BootID: boot}, nil
}

// CheckBinding is box init's side: the record at path must name this box's
// network namespace, its PID 1 and this boot. fs is the box's /proc.
func CheckBinding(path string, fs ProcFS) error {
	want, err := ReadBinding(path)
	if err != nil {
		return fmt.Errorf("%w; start the box with 'devc up', never with a plain 'podman start'", notBound(err))
	}
	ino, err := fs.NSInode("self", "net")
	if err != nil {
		return fmt.Errorf("reading this box's network namespace: %w", err)
	}
	start, err := fs.StartTime("1")
	if err != nil {
		return fmt.Errorf("reading the start time of PID 1: %w", err)
	}
	boot, err := fs.BootID()
	if err != nil {
		return fmt.Errorf("reading the boot ID: %w", err)
	}
	got := Binding{V: BindingVersion, NetNS: ino, PID1Start: start, BootID: boot}
	if !got.Equal(want) {
		return errors.New("this start of the box is not the one devc installed the network rules for (network namespace, " +
			"main process or boot differ); start the box with 'devc up', never with a plain 'podman start'")
	}
	return nil
}

// notBound explains a missing or unreadable record to the user, who most
// likely started the box with a plain `podman start`.
func notBound(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("devc has not bound this box's network rules to this start (no record)")
	}
	return fmt.Errorf("the network binding record is unusable: %w", err)
}
