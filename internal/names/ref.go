// This file checks git branch and tag names that come from users or boxes.

package names

import (
	"fmt"
	"strings"
)

// MaxRefLen is devc's limit for git branch and tag names, on top of git's
// own rules.
const MaxRefLen = 200

// CheckRef reports why a git branch or tag name is refused, or nil. It
// applies git-check-ref-format's rules for refs/heads/<n> (with --branch's
// ban on a leading "-") and devc's own limits: printable ASCII only, at most
// MaxRefLen bytes, not "HEAD". A name that passes is safe in a ref, in an
// argument vector (it is never an option) and in a message.
func CheckRef(n string) error {
	switch {
	case n == "":
		return fmt.Errorf("empty name")
	case len(n) > MaxRefLen:
		return fmt.Errorf("longer than %d bytes", MaxRefLen)
	case strings.HasPrefix(n, "-"):
		return fmt.Errorf("starts with '-'")
	case n == "HEAD" || n == "@":
		return fmt.Errorf("%q is reserved", n)
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if c <= 0x20 || c >= 0x7f {
			return fmt.Errorf("contains a control, space or non-ASCII character")
		}
		if strings.IndexByte("~^:?*[\\", c) >= 0 {
			return fmt.Errorf("contains %q", c)
		}
	}
	switch {
	case strings.HasPrefix(n, "/") || strings.HasSuffix(n, "/") || strings.Contains(n, "//"):
		return fmt.Errorf("has an empty path component")
	case strings.Contains(n, ".."):
		return fmt.Errorf("contains '..'")
	case strings.Contains(n, "@{"):
		return fmt.Errorf("contains '@{'")
	case strings.HasSuffix(n, "."):
		return fmt.Errorf("ends with '.'")
	}
	for _, comp := range strings.Split(n, "/") {
		if strings.HasPrefix(comp, ".") {
			return fmt.Errorf("has a component starting with '.'")
		}
		if strings.HasSuffix(comp, ".lock") {
			return fmt.Errorf("has a component ending in '.lock'")
		}
	}
	return nil
}
