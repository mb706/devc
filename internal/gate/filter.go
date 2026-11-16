// This file decides commands under the read-only profile of an SSH grant.

package gate

import (
	"fmt"
	"slices"
	"strings"
)

// ShellMetacharacters may not appear in a command of the read-only
// profile: they would let one allowed command start another.
const ShellMetacharacters = ";|&$`<>()\n"

// ReadOnlyNote is how the read-only profile describes itself (help texts
// and refusals).
const ReadOnlyNote = "the read-only profile is a convenience, not a strong boundary: an allowed command that runs programs or scripts (sbatch, find -exec, an editor) can still do anything the account can"

// CheckReadOnly decides a command of a box under the read-only profile:
// its first word must be one of allowed (compared literally), and it may
// contain no shell metacharacters and no control characters. An empty
// command (an interactive shell) and sftp are refused by the caller.
func CheckReadOnly(command string, allowed []string) error {
	for _, r := range command {
		if strings.ContainsRune(ShellMetacharacters, r) || r < 0x20 && r != '\t' || r == 0x7f {
			return fmt.Errorf("the command contains %q; commands on a read-only host may not use shell syntax", r)
		}
	}
	first := firstWord(command)
	if first == "" {
		return fmt.Errorf("an interactive shell is not allowed on a read-only host")
	}
	if !slices.Contains(allowed, first) {
		if len(allowed) == 0 {
			return fmt.Errorf("%q is not allowed: no command is allowed on this read-only host", first)
		}
		return fmt.Errorf("%q is not allowed on this read-only host (allowed: %s)", first, strings.Join(allowed, ", "))
	}
	return nil
}

// firstWord is the first blank-separated word, as a shell splits it.
func firstWord(s string) string {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}
