// This file implements the store of command receipts in local/receipts.

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// The receipt store (work order C09 section 3): one file per command ID in
// local/receipts/, written before anything is done for the command. A file
// is created whole and exclusively (written to a temporary name, then
// hard-linked to its final name, which fails if another request got there
// first), so a repeated ID can never act twice; later states replace it by
// rename.

// receiptKeep is how long receipts are kept.
const receiptKeep = 7 * 24 * time.Hour

// cmdRecord is a stored command and its receipt.
type cmdRecord struct {
	V       int           `json:"v"`
	Seq     int64         `json:"seq"`
	Receipt proto.Receipt `json:"receipt"`
	Command proto.Command `json:"command"`
	// Phase of a start command: "" until the session exists, "prompt" while
	// its first prompt waits.
	Phase string `json:"phase,omitempty"`
}

// receiptStore keeps one file per command ID in its directory.
type receiptStore struct {
	dir string
	// failSave, when set, makes save fail for the records it refuses
	// (tests: a full disk).
	failSave atomic.Pointer[func(*cmdRecord) error]
	// syncDir, when set, replaces syncDirectory (tests: a failing or
	// observed directory sync).
	syncDir atomic.Pointer[func(dir string) error]
}

// syncDirectory fsyncs a directory, so a link or rename in it survives a
// power failure (finding D1-11).
func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// publish makes the directory entry of a new or replaced record durable.
func (s *receiptStore) publish() error {
	sync := syncDirectory
	if f := s.syncDir.Load(); f != nil {
		sync = *f
	}
	if err := sync(s.dir); err != nil {
		return fmt.Errorf("making the receipt durable: %w", err)
	}
	return nil
}

func (s *receiptStore) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *receiptStore) ensure() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.dir, 0o700)
}

// load reads the record of id; fs.ErrNotExist when there is none.
func (s *receiptStore) load(id string) (*cmdRecord, error) {
	if !proto.ValidCommandID(id) {
		return nil, fmt.Errorf("invalid command id")
	}
	f, err := os.OpenFile(s.path(id), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r cmdRecord
	dec := json.NewDecoder(f)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("the receipt of %s is corrupt: %w", id, err)
	}
	if r.Command.ID != id {
		return nil, fmt.Errorf("the receipt file of %s names %q", id, r.Command.ID)
	}
	return &r, nil
}

// writeTemp writes a record to a new temporary file in the store's
// directory and syncs it, ready to be linked or renamed into place.
func (s *receiptStore) writeTemp(r *cmdRecord) (string, error) {
	if err := s.ensure(); err != nil {
		return "", err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(s.dir, ".tmp-receipt-")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// create stores a new record. When the ID has a record already, nothing is
// written and that record is returned with created false.
func (s *receiptStore) create(r *cmdRecord) (*cmdRecord, bool, error) {
	if old, err := s.load(r.Command.ID); err == nil {
		return old, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	tmp, err := s.writeTemp(r)
	if err != nil {
		return nil, false, err
	}
	err = os.Link(tmp, s.path(r.Command.ID))
	os.Remove(tmp)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			old, lerr := s.load(r.Command.ID)
			if lerr != nil {
				return nil, false, lerr
			}
			return old, false, nil
		}
		return nil, false, err
	}
	// Accepted only once the new entry is durable; otherwise it is taken
	// back, so a retry with the same ID starts afresh.
	if err := s.publish(); err != nil {
		os.Remove(s.path(r.Command.ID))
		return nil, false, err
	}
	return r, true, nil
}

// save replaces a record (after create).
func (s *receiptStore) save(r *cmdRecord) error {
	if f := s.failSave.Load(); f != nil {
		if err := (*f)(r); err != nil {
			return err
		}
	}
	tmp, err := s.writeTemp(r)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(r.Command.ID)); err != nil {
		os.Remove(tmp)
		return err
	}
	// The transition counts only once the rename is durable: a
	// "delivering" record that a power failure could undo would let the
	// command be dispatched again (finding D1-11).
	return s.publish()
}

// list returns every record, in acceptance order; unreadable files are
// skipped.
func (s *receiptStore) list() ([]*cmdRecord, error) {
	des, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*cmdRecord
	for _, de := range des {
		id, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok || !de.Type().IsRegular() || !proto.ValidCommandID(id) {
			continue
		}
		r, err := s.load(id)
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// remove deletes a record.
func (s *receiptStore) remove(id string) {
	if proto.ValidCommandID(id) {
		_ = os.Remove(s.path(id))
	}
}

// cleanTemps removes temporary files a crash left behind.
func (s *receiptStore) cleanTemps() {
	des, _ := os.ReadDir(s.dir)
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".tmp-receipt-") {
			_ = os.Remove(filepath.Join(s.dir, de.Name()))
		}
	}
}
