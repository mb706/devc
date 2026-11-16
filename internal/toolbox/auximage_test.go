package toolbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/mb706/devc/assets"
	"github.com/mb706/devc/internal/engine"
)

// TestEnsureAuxImage: built when missing or when the embedded files
// change, not otherwise; --rebuild builds fresh. The installer keeps the
// hash it had before multi-file images (so its cache stays valid).
func TestEnsureAuxImage(t *testing.T) {
	ctx := context.Background()
	eng := engine.NewFake()
	dir := filepath.Join(t.TempDir(), "aux", "desktop")
	img := AuxImage{Ref: "localhost/devc-aux/test:latest", Name: "test",
		Files: map[string][]byte{"Containerfile": []byte("FROM scratch\nCOPY entry /entry\n"), "entry": []byte("#!/bin/sh\n")}}
	if err := EnsureAuxImage(ctx, eng, img, dir, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(eng.Builds) != 1 || eng.Builds[0].Labels["devc.aux"] != "test" || eng.Builds[0].Labels["devc.aux.hash"] != img.Hash() ||
		eng.Builds[0].NoCache || eng.Builds[0].File != filepath.Join(dir, "Containerfile") {
		t.Fatalf("builds %+v", eng.Builds)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "entry")); err != nil || string(b) != "#!/bin/sh\n" {
		t.Errorf("context: %q %v", b, err)
	}
	if err := EnsureAuxImage(ctx, eng, img, dir, false, nil); err != nil || len(eng.Builds) != 1 {
		t.Errorf("rebuilt an up-to-date image: %v", err)
	}
	img.Files["entry"] = []byte("#!/bin/sh\necho changed\n")
	if err := EnsureAuxImage(ctx, eng, img, dir, false, nil); err != nil || len(eng.Builds) != 2 {
		t.Errorf("a changed file did not rebuild: %v", err)
	}
	if err := EnsureAuxImage(ctx, eng, img, dir, true, nil); err != nil || len(eng.Builds) != 3 || !eng.Builds[2].NoCache || !eng.Builds[2].Pull {
		t.Errorf("rebuild: %v %+v", err, eng.Builds)
	}
	for _, bad := range []map[string][]byte{{"entry": nil}, {"Containerfile": nil, "../x": nil}, {"Containerfile": nil, ".hidden": nil}} {
		if err := EnsureAuxImage(ctx, eng, AuxImage{Ref: img.Ref, Name: "test", Files: bad}, dir, true, nil); err == nil {
			t.Errorf("files %v accepted", bad)
		}
	}
	sum := sha256.Sum256(assets.InstallerContainerfile)
	inst := AuxImage{Ref: InstallerImage, Name: "installer", Files: map[string][]byte{"Containerfile": assets.InstallerContainerfile}}
	if inst.Hash() != hex.EncodeToString(sum[:]) {
		t.Error("the installer image's hash changed")
	}
}
