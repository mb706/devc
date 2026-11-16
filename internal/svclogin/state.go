// This file holds the machine's login state of cookie services and the
// request log of every service. Both live in host state that no box mounts;
// state files are read without following symlinks and replaced whole by a
// rename, and the log is appended to without following one.

package svclogin

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// maxStateFile bounds a state file when it is read.
const maxStateFile = 64 << 10

// State is the login state of a cookie service on this machine: the daemon
// marks a service as needing a login when its jar is too old or the
// upstream answers 401 or redirects to its login page; a login newer than
// the mark clears it.
type State struct {
	V          int       `json:"v"`
	NeedsLogin bool      `json:"needs_login,omitempty"`
	Since      time.Time `json:"since,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	// AutoError and AutoAt describe the last failed automatic login.
	AutoError string    `json:"auto_error,omitempty"`
	AutoAt    time.Time `json:"auto_at,omitempty"`
}

// Current reports whether the mark still applies to the jar: set, and not
// older than the jar's login.
func (s State) Current(j *Jar) (bool, string) {
	if !s.NeedsLogin {
		return false, ""
	}
	if j != nil && !j.LoginAt.Before(s.Since) {
		return false, ""
	}
	return true, s.Reason
}

// ReadState reads a state file; a missing file is the zero state.
func ReadState(path string) (State, error) {
	b, err := readCapped(path, maxStateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return State{V: 1}, nil
	}
	if err != nil {
		return State{V: 1}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil || s.V != 1 {
		return State{V: 1}, fmt.Errorf("%s is corrupt", path)
	}
	return s, nil
}

// WriteState replaces a state file (0600, directory 0700).
func WriteState(path string, s State) error {
	s.V = 1
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return b, nil
}

func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MaxLogBytes is the size at which a service log is rotated to <file>.1.
const MaxLogBytes = 4 << 20

// LogEntry is one request a box made to a service. Headers and bodies are
// never logged; the path is the normalised path without the query.
type LogEntry struct {
	Time   time.Time `json:"time"`
	Box    string    `json:"box"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Status int       `json:"status"`
	// Reason says why devc refused or failed a request itself.
	Reason string `json:"reason,omitempty"`
}

// Log appends to a service's log.
type Log struct {
	mu   sync.Mutex
	path string
	max  int64
}

// NewLog returns the log at path (created with its directory on the first
// entry).
func NewLog(path string) *Log { return &Log{path: path, max: MaxLogBytes} }

// logText makes a field printable and short.
func logText(s string, n int) string {
	s = truncate(s, n)
	if !utf8.ValidString(s) {
		return printable(s)
	}
	out := []rune{}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		out = append(out, r)
	}
	return string(out)
}

// Append writes one entry; a failure does not change any decision.
func (l *Log) Append(e LogEntry) error {
	e.Box, e.Method, e.Path, e.Reason = logText(e.Box, 100), logText(e.Method, 20), logText(e.Path, 500), logText(e.Reason, 300)
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
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadLog returns the entries of a log and its rotated part, oldest first;
// lines that do not parse are skipped.
func ReadLog(path string) ([]LogEntry, error) {
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
		sc := bufio.NewScanner(io.LimitReader(f, 2*MaxLogBytes))
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			var e LogEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				out = append(out, e)
			}
		}
		f.Close()
	}
	return out, nil
}
