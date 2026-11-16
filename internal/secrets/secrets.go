// Package secrets stores per-realm secrets on the host and writes those a
// box is given into its secrets directory, which must be memory-backed.
// Its callers decide what a box is given: the harness hooks and the SSH
// gate before a start, the daemon on demand.
//
// Security rules: values never appear in argument lists, environment dumps
// or logs (README rule 5); they are written only to 0600 files in the
// store and 0400 files in a box's secrets directory, both created without
// following symlinks. Names are validated before they become file names
// (rule 4).
package secrets

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// ErrNotFound is returned by Get and Delete for a secret that does not exist.
var ErrNotFound = errors.New("no such secret")

// MaxSize bounds a secret value.
const MaxSize = 64 << 10

// Store keeps secrets per realm.
type Store interface {
	Get(realm, name string) ([]byte, error)
	Set(realm, name string, value []byte) error
	Delete(realm, name string) error
	List(realm string) ([]string, error)
}

// FileStore keeps each secret in durable/realms/<realm>/secrets/<name>
// (files 0600, directory 0700).
type FileStore struct {
	Paths *paths.Paths
}

// NewFileStore returns the file backend for a state directory.
func NewFileStore(p *paths.Paths) *FileStore { return &FileStore{Paths: p} }

var _ Store = (*FileStore)(nil)

// check validates the realm and secret names before they become a path.
func check(realm, name string) error {
	if err := names.Check(names.KindRealm, realm); err != nil {
		return err
	}
	return names.Check(names.KindSecret, name)
}

// Get implements Store.
func (s *FileStore) Get(realm, name string) ([]byte, error) {
	if err := check(realm, name); err != nil {
		return nil, err
	}
	return readSecretFile(filepath.Join(s.Paths.SecretsDir(realm), name))
}

// Set implements Store.
func (s *FileStore) Set(realm, name string, value []byte) error {
	if err := check(realm, name); err != nil {
		return err
	}
	if len(value) == 0 {
		return errors.New("the secret is empty")
	}
	if len(value) > MaxSize {
		return fmt.Errorf("the secret is larger than %d bytes", MaxSize)
	}
	dir := s.Paths.SecretsDir(realm)
	if err := s.Paths.Ensure(dir); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return writeFile(dir, name, value, 0o600)
}

// Delete implements Store.
func (s *FileStore) Delete(realm, name string) error {
	if err := check(realm, name); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.Paths.SecretsDir(realm), name))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s in realm %s: %w", name, realm, ErrNotFound)
	}
	return err
}

// List implements Store: the names of the realm's secrets, sorted.
func (s *FileStore) List(realm string) ([]string, error) {
	if err := names.Check(names.KindRealm, realm); err != nil {
		return nil, err
	}
	des, err := os.ReadDir(s.Paths.SecretsDir(realm))
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, de := range des {
		if de.Type().IsRegular() && names.ValidSecret(de.Name()) {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// readSecretFile reads a regular file without following a symlink in its
// last component, with a size cap.
func readSecretFile(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", filepath.Base(p), ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, MaxSize)
	}
	return b, nil
}

// writeFile writes value to dir/name through a new temporary file (created
// exclusively, so nothing planted is followed) and renames it into place.
func writeFile(dir, name string, value []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(dir, ".tmp-"+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(value); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// TrimValue removes one trailing line ending, which a piped or pasted value
// usually carries.
func TrimValue(b []byte) []byte {
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return []byte(s)
}

// EnvAllowPersistent is the environment variable that permits delivering
// secrets to a directory that is not memory-backed.
const EnvAllowPersistent = "DEVC_ALLOW_PERSISTENT_SECRETS"

// Filesystem magic numbers of statfs(2) for memory-backed filesystems.
const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

// fsType reports the statfs type of dir; tests replace it.
var fsType = func(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil
}

// CheckMemoryFS fails unless dir is on tmpfs or ramfs, so delivered
// secrets never reach persistent storage (finding A9).
func CheckMemoryFS(dir string) error {
	t, err := fsType(dir)
	if err != nil {
		return fmt.Errorf("checking the filesystem of %s: %w", dir, err)
	}
	if t == tmpfsMagic || t == ramfsMagic {
		return nil
	}
	return fmt.Errorf("the secrets directory %s is not on tmpfs or ramfs (filesystem type %#x), so delivered secrets would be written to persistent storage: "+
		"point XDG_RUNTIME_DIR or DEVC_RUNTIME_DIR at a tmpfs, or set %s=1 to accept persistent copies", dir, t, EnvAllowPersistent)
}

// Deliver makes dir hold exactly the given secrets as 0400 files (dir 0700),
// each written to a new file and renamed into place; anything else in dir
// is removed. The box mounts the directory, not the files, so it sees what
// the last delivery left there; devc delivers only before a start. Unless
// allowPersistent is set, it refuses to write any secret when dir is not
// memory-backed.
func Deliver(p *paths.Paths, dir string, values map[string][]byte, allowPersistent bool) error {
	if err := p.Ensure(dir); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if len(values) > 0 && !allowPersistent {
		if err := CheckMemoryFS(dir); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(values) {
		if !names.ValidSecret(name) {
			return fmt.Errorf("invalid secret name %q", name)
		}
		if err := writeFile(dir, name, values[name], 0o400); err != nil {
			return fmt.Errorf("delivering secret %s: %w", name, err)
		}
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, de := range des {
		if _, keep := values[de.Name()]; keep && de.Type().IsRegular() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, de.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes a box's secrets directory.
func Remove(dir string) error {
	err := os.RemoveAll(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
