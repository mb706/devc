package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/data"
)

func TestSyncOutput(t *testing.T) {
	var b bytes.Buffer
	printSyncPreview(&b, &data.SyncResult{Push: true, Remote: "nas", Via: "ssh-master", Files: 2, Size: 2000, Updated: 1, BackupDir: ".devc-backup/20261115T040500Z",
		Items: []data.Item{{Change: "<f+++++++++", Size: 1000, Name: "scratch/a"}, {Change: "<f.st......", Size: 1000, Name: "scratch/b"}}})
	if out := b.String(); !strings.Contains(out, "<f.st...... scratch/b\n") || !strings.Contains(out, "2 files (2.0 kB) to push to nas through the connection of 'devc ssh login'; 1 of them replace files there, which are kept in .devc-backup/20261115T040500Z") {
		t.Errorf("push preview:\n%s", out)
	}
	b.Reset()
	st := &data.DataStatus{Machine: "ws", Entries: []data.StatusEntry{
		{Path: "scratch/w", Present: true, Files: 2, Size: 5, State: data.StateChanged, Last: &data.Record{Time: "2026-11-15T04:05:00Z", Action: data.ActionPush, Machine: "laptop", Remote: "nas"},
			Machines: []data.RecordedAt{{Name: "laptop"}}, Remotes: []data.RecordedAt{{Name: "nas"}, {Name: "c1", Same: true}}},
		{Path: "results/gone", State: data.StateAbsent},
	}}
	if err := printDataStatus(&b, st); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"machine ws", "scratch/w", "changed", "push on laptop (nas)", "laptop*", "nas*,c1", "results/gone", "absent"} {
		if !strings.Contains(out, want) {
			t.Errorf("status table lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	printVerification(&b, &data.Verification{Path: "results/run1", Current: boxside.EntryDigest{Files: 1, Size: 3, Digest: "sha256:aa", Content: "sha256:bb"},
		Last: &data.Record{Action: data.ActionSeal, Machine: "ws", Time: "2026-11-15T04:05:00Z", Digest: "sha256:cc"}, ContentRecord: &data.Record{Action: data.ActionSeal, Machine: "ws"}, ContentMatches: true}, true)
	if out := b.String(); !strings.Contains(out, "DIFFERS from the last record (seal on ws") || !strings.Contains(out, "content matches the stored hash") {
		t.Errorf("verification:\n%s", out)
	}
}
