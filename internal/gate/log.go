// This file appends to a realm's gate log, one line per gate session.

package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/paths"
)

// Entry is one line of a realm's gate log.
type Entry struct {
	Time    time.Time `json:"time"`
	Box     string    `json:"box"`
	Project string    `json:"project"`
	Host    string    `json:"host"`
	Mode    Mode      `json:"mode"`
	TTY     bool      `json:"tty,omitempty"`
	// Command is what the box asked to run, truncated to
	// MaxLoggedCommand bytes.
	Command string `json:"command,omitempty"`
	// Exit is the remote exit status (255 when ssh failed).
	Exit int `json:"exit"`
	// Duration is in seconds.
	Duration float64 `json:"duration"`
	// Refused says why the gate did not run the command.
	Refused string `json:"refused,omitempty"`
}

// Truncate shortens s to at most n bytes without splitting a character.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	// s[cut] is the first byte left out; while it continues a character,
	// that character would be split.
	for i := 0; i < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}
	return s[:cut]
}

// AppendLog appends an entry to the realm's gate log
// (durable/realms/<realm>/gate-log.jsonl, 0600). Concurrent gate
// sessions append whole lines under an exclusive lock. Nothing rotates or
// bounds the log.
func AppendLog(p *paths.Paths, realm string, e Entry) error {
	file := p.GateLog(realm)
	if err := p.Ensure(filepath.Dir(file)); err != nil {
		return err
	}
	e.Command = Truncate(e.Command, MaxLoggedCommand)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE|noFollow, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
