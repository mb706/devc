// This file implements the box's side of the spool: appending validated
// events, and looking for a session's end.

package boxside

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// SpoolDir is where a box writes its events; the host reads it from
// <runtime>/<box-id>/spool through internal/safefs.
const SpoolDir = RunDir + "/" + SpoolDirName

// Payload is the type and data of an event before it gets a session and a
// timestamp.
type Payload struct {
	Type string
	Data any
}

// AppendEvents appends payloads as events of session to its spool file
// below spoolDir in one write, so concurrent writers never interleave
// within a line. Each payload is validated as the host would validate it;
// one that fails (or whose line would be too long) is replaced by a note,
// so a mapping bug shows up in the journal instead of being dropped.
func AppendEvents(spoolDir, session string, now time.Time, ps []Payload) error {
	if !names.ValidSessionID(session) {
		return fmt.Errorf("invalid session ID %q", session)
	}
	var buf []byte
	for _, p := range ps {
		line, err := eventLine(session, now, p)
		if err != nil {
			line, err = eventLine(session, now, Payload{proto.TypeNote, proto.Note{Text: proto.Truncate("devc: dropped a "+p.Type+" event: "+err.Error(), proto.MaxNoteText)}})
			if err != nil {
				return err
			}
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if len(buf) == 0 {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(spoolDir, session+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The box's own spool: tell the host (work order C08). Other spool
	// directories are tests.
	if filepath.Clean(spoolDir) == SpoolDir {
		hintHost(session)
	}
	return nil
}

// eventLine encodes one event and validates it as the host will; a line
// the host would skip as too long is an error.
func eventLine(session string, now time.Time, p Payload) ([]byte, error) {
	e, err := proto.New(session, p.Type, p.Data, now)
	if err != nil {
		return nil, err
	}
	if _, err := proto.ValidateData(e.Type, e.Data); err != nil {
		return nil, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(b) >= proto.MaxLine {
		return nil, fmt.Errorf("event of %d bytes is too long", len(b))
	}
	return b, nil
}

// spoolHasEnd reports whether the spool file of session already holds a
// session.end event. A missing file counts as no end: the host removes a
// spool file only after ingesting its end, and a repeated end is dropped
// on the host.
func spoolHasEnd(spoolDir, session string) bool {
	f, err := os.Open(filepath.Join(spoolDir, session+".jsonl"))
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), proto.MaxLine+1)
	for sc.Scan() {
		var e struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Type == proto.TypeSessionEnd {
			return true
		}
	}
	return false
}
