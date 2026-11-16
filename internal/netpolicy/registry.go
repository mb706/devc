// This file keeps the records of running forwards in the runtime directory,
// for `devc forward ls` and `stop` and for the router, which routes
// forwarded ports.

package netpolicy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/names"
)

// Record describes a detached forward. Records live in the runtime
// directory (<runtime>/forwards/<id>.json), which no box can write; a
// record whose process is gone is stale and removed when listed.
type Record struct {
	ID        string    `json:"id"`
	Ref       string    `json:"ref"`
	Container string    `json:"container"`
	BoxPort   int       `json:"box_port"`
	Bind      string    `json:"bind"`
	HostPort  int       `json:"host_port"`
	PID       int       `json:"pid"`
	Started   time.Time `json:"started"`
	// ProcStart is the start time of the process (field 22 of its stat
	// file) for a forward in the foreground, whose command line does not
	// name the ID (work order C13: the router routes forwarded ports).
	ProcStart uint64 `json:"proc_start,omitempty"`
}

var forwardIDRE = regexp.MustCompile(`^f[0-9a-f]{6}$`)

// ValidForwardID reports whether s has the form of a forward ID.
func ValidForwardID(s string) bool { return forwardIDRE.MatchString(s) }

// NewForwardID returns a random forward ID ("f" and six hex digits).
func NewForwardID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "f" + hex.EncodeToString(b)
}

// validate checks a record before it is saved and when it is read: the
// container must be the box of the record's project.
func (r Record) validate() error {
	ref, err := names.Parse(r.Ref)
	switch {
	case !ValidForwardID(r.ID):
		return fmt.Errorf("bad forward id %q", r.ID)
	case err != nil:
		return err
	case r.Container != names.BoxName(ref):
		return fmt.Errorf("container %q does not belong to %s", r.Container, r.Ref)
	case r.BoxPort < 1 || r.BoxPort > 65535 || r.HostPort < 1 || r.HostPort > 65535:
		return errors.New("bad port")
	case r.PID <= 1:
		return errors.New("bad pid")
	}
	return nil
}

// Registry keeps the records of detached forwards.
type Registry struct {
	Dir string
	// Alive reports whether a record's process still runs (ProcessAlive
	// when nil).
	Alive func(Record) bool
}

// NewRegistry returns the registry in dir (<runtime>/forwards).
func NewRegistry(dir string) *Registry { return &Registry{Dir: dir} }

func (r *Registry) file(id string) string { return filepath.Join(r.Dir, id+".json") }

// LogFile is where a detached forward writes its messages.
func (r *Registry) LogFile(id string) string { return filepath.Join(r.Dir, id+".log") }

func (r *Registry) drop(id string) {
	_ = os.Remove(r.file(id))
	_ = os.Remove(r.LogFile(id))
}

func (r *Registry) alive(rec Record) bool {
	if r.Alive != nil {
		return r.Alive(rec)
	}
	return ProcessAlive(rec)
}

// Save writes a record (0600, by rename).
func (r *Registry) Save(rec Record) error {
	if err := rec.validate(); err != nil {
		return fmt.Errorf("forward record: %w", err)
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(r.Dir, ".tmp-"+rec.ID+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, r.file(rec.ID))
}

// read loads one record; ok is false for a file that is not a valid record.
func (r *Registry) read(id string) (Record, bool) {
	var rec Record
	b, err := os.ReadFile(r.file(id))
	if err != nil || len(b) > 64<<10 {
		return rec, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&rec) != nil || rec.ID != id || rec.validate() != nil {
		return rec, false
	}
	return rec, true
}

// List returns the live forwards, oldest first. Records of processes that
// are gone, and files that are not valid records, are removed.
func (r *Registry) List() ([]Record, error) {
	des, err := os.ReadDir(r.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, de := range des {
		id, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok || !ValidForwardID(id) || !de.Type().IsRegular() {
			continue
		}
		rec, ok := r.read(id)
		if !ok || !r.alive(rec) {
			r.drop(id)
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.Before(out[j].Started)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Get returns the live forward with this ID.
func (r *Registry) Get(id string) (Record, error) {
	if !ValidForwardID(id) {
		return Record{}, fmt.Errorf("%q is not a forward id (see 'devc forward ls')", id)
	}
	rec, ok := r.read(id)
	if !ok || !r.alive(rec) {
		r.drop(id)
		return Record{}, fmt.Errorf("no forward %s is running (see 'devc forward ls')", id)
	}
	return rec, nil
}

// Remove deletes the record of id when it still names pid (any pid when
// pid is 0), so a forward that exits never removes a newer record.
func (r *Registry) Remove(id string, pid int) error {
	if !ValidForwardID(id) {
		return fmt.Errorf("bad forward id %q", id)
	}
	if pid != 0 {
		if rec, ok := r.read(id); ok && rec.PID != pid {
			return nil
		}
	}
	err := os.Remove(r.file(id))
	_ = os.Remove(r.LogFile(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ProcessAlive reports whether the record's process exists and is the
// forward it describes: a detached forward's command line must name the
// forward ID, a foreground forward's process must have the recorded start
// time, so a reused PID is not mistaken for it.
func ProcessAlive(rec Record) bool {
	if err := syscall.Kill(rec.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(rec.PID), "cmdline"))
	if err != nil {
		return false
	}
	args := strings.Split(string(b), "\x00")
	if rec.ProcStart != 0 {
		st, err := ProcFS{}.StartTime(strconv.Itoa(rec.PID))
		return err == nil && st == rec.ProcStart && slices.Contains(args, "forward")
	}
	return containsSeq(args, "forward", "serve") && containsSeq(args, "--id", rec.ID)
}

func containsSeq(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}
