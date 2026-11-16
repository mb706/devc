// This file holds the package's helpers for writing files.

package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeFile writes data atomically with the given mode.
func writeFile(p string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-"+filepath.Base(p)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func marshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
