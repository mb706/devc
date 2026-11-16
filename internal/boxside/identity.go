// This file implements the identity fix (`devc box fix-identity`), which
// `devc up` runs as the box's root after every start.

package boxside

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The fixed in-box accounts (plan 3.1). Kept here rather than imported from
// internal/paths so the in-box code has no host dependencies.
const (
	devName   = "dev"
	devID     = "1000"
	devHome   = "/home/dev"
	vaultName = "vault"
	vaultID   = "2000"
)

// FixIdentity rewrites /etc/passwd and /etc/group below root so that UID and
// GID 1000 are "dev" with home /home/dev and the given shell, and 2000 is
// "vault". Entries using those IDs or names are replaced in place; every
// other line is kept byte for byte. Running it again changes nothing. It
// needs only ownership of the files, not capabilities.
func FixIdentity(root, shell string) (changed bool, err error) {
	nologin := "/bin/false"
	for _, p := range []string{"/usr/sbin/nologin", "/sbin/nologin"} {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			nologin = p
			break
		}
	}
	passwd := filepath.Join(root, "etc", "passwd")
	group := filepath.Join(root, "etc", "group")
	c1, err := rewriteFile(passwd, func(b []byte) []byte { return RewritePasswd(b, shell, nologin) })
	if err != nil {
		return false, err
	}
	c2, err := rewriteFile(group, RewriteGroup)
	if err != nil {
		return c1, err
	}
	return c1 || c2, nil
}

// rewriteFile replaces path with fn of its content, by rename, keeping its
// mode; it writes nothing when the content stays the same.
func rewriteFile(path string, fn func([]byte) []byte) (bool, error) {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	updated := fn(old)
	if bytes.Equal(old, updated) {
		return false, nil
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".devc-tmp")
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, updated, mode); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		// A file bind-mounted by the engine cannot be replaced; write it
		// in place instead.
		if errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EXDEV) {
			return true, os.WriteFile(path, updated, mode)
		}
		return false, fmt.Errorf("replacing %s: %w", path, err)
	}
	return true, nil
}

// account is one fixed account: line renders its canonical entry from the
// fields of the entry it replaces (nil when it is new).
type account struct {
	name, id string
	line     func(old []string) string
}

// rewriteDB replaces the entries of each account (matched by name or by the
// numeric ID in field idField) with its canonical line, at the position of
// the first match, and appends accounts that were absent.
func rewriteDB(content []byte, nfields, idField int, accts []account) []byte {
	text := string(content)
	trailingNL := text == "" || strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	placed := make([]bool, len(accts))
	var out []string
	for _, l := range lines {
		f := strings.Split(l, ":")
		hit := -1
		if len(f) == nfields && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "-") {
			for i, a := range accts {
				if f[0] == a.name || f[idField] == a.id {
					hit = i
					break
				}
			}
		}
		if hit < 0 {
			out = append(out, l)
			continue
		}
		if !placed[hit] {
			out = append(out, accts[hit].line(f))
			placed[hit] = true
		}
	}
	for i, a := range accts {
		if !placed[i] {
			out = append(out, a.line(nil))
		}
	}
	s := strings.Join(out, "\n")
	if trailingNL || len(out) > 0 {
		s += "\n"
	}
	return []byte(s)
}

// RewritePasswd applies the identity fix to the content of /etc/passwd.
func RewritePasswd(content []byte, shell, nologin string) []byte {
	return rewriteDB(content, 7, 2, []account{
		{devName, devID, func([]string) string {
			return strings.Join([]string{devName, "x", devID, devID, devName, devHome, shell}, ":")
		}},
		{vaultName, vaultID, func([]string) string {
			return strings.Join([]string{vaultName, "x", vaultID, vaultID, vaultName, "/nonexistent", nologin}, ":")
		}},
	})
}

// RewriteGroup applies the identity fix to the content of /etc/group. The
// member list of a replaced group is kept.
func RewriteGroup(content []byte) []byte {
	group := func(name, id string) func([]string) string {
		return func(old []string) string {
			members := ""
			if len(old) == 4 {
				members = old[3]
			}
			return strings.Join([]string{name, "x", id, members}, ":")
		}
	}
	return rewriteDB(content, 4, 2, []account{
		{devName, devID, group(devName, devID)},
		{vaultName, vaultID, group(vaultName, vaultID)},
	})
}
