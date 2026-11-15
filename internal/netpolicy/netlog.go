package netpolicy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// LogEntry is one decision of the allowlist proxy, a line of
// durable/realms/<realm>/netlog/<project>.jsonl.
type LogEntry struct {
	Time time.Time `json:"time"`
	// Session is the devc session the connection came from, when known
	// (the proxy sees connections, not processes, so it is usually empty).
	Session string `json:"session,omitempty"`
	// Method is CONNECT or the HTTP method of a plain request.
	Method  string `json:"method"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Allowed bool   `json:"allowed"`
	// Reason says why a request was refused (or which rule allowed it).
	Reason string `json:"reason,omitempty"`
	// Addr is the address the proxy connected to.
	Addr string `json:"addr,omitempty"`
}

// MaxLogBytes is the size at which a project's log moves to <log>.1
// (replacing the previous one), so a box that floods the proxy costs at
// most twice this on disk.
const MaxLogBytes = 4 << 20

// logText makes box-supplied text safe for a log line: printable ASCII and
// valid UTF-8 letters only, at most 255 bytes.
func logText(s string) string {
	if len(s) > 255 {
		s = s[:255]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError, r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NetLog appends entries to one project's log.
type NetLog struct {
	mu   sync.Mutex
	path string
	max  int64
}

// NewNetLog returns the log at path (created with its directory on the
// first entry).
func NewNetLog(path string) *NetLog { return &NetLog{path: path, max: MaxLogBytes} }

// Append writes one entry. Failures are returned; the proxy's decision
// does not depend on them.
func (l *NetLog) Append(e LogEntry) error {
	e.Host, e.Reason, e.Method = logText(e.Host), logText(e.Reason), logText(e.Method)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(l.path); err == nil && fi.Mode().IsRegular() && fi.Size()+int64(len(b)) > l.max {
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadNetLog returns the entries of the log at path, the rotated part
// first, oldest first. Lines that are not valid entries are skipped.
func ReadNetLog(path string) ([]LogEntry, error) {
	var out []LogEntry
	for _, p := range []string{path + ".1", path} {
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			f.Close()
			return out, fmt.Errorf("%s is not a regular file", p)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			var e LogEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil && !e.Time.IsZero() {
				out = append(out, e)
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
