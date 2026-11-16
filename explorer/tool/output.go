package main

// This file writes the site: the app's files and the data scripts.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// siteMarker marks a directory the tool wrote, so that it may replace it.
const siteMarker = ".explorer-site"

// siteSizes are the sizes of what was written, for the summary.
type siteSizes struct {
	core, chunks, docs, total int64
	nChunks, nDocs            int
}

// writeSite replaces out with the app's files and the data.
func writeSite(out, appDir string, core *Core, chunks map[string]*Chunk, docs []*docFile) (siteSizes, error) {
	var sz siteSizes
	if es, err := os.ReadDir(out); err == nil && len(es) > 0 {
		if _, err := os.Stat(filepath.Join(out, siteMarker)); err != nil {
			return sz, fmt.Errorf("%s exists and was not written by this tool; remove it or choose another --out", out)
		}
		if err := os.RemoveAll(out); err != nil {
			return sz, err
		}
	}
	for _, d := range []string{"data/chunks", "data/docs"} {
		if err := os.MkdirAll(filepath.Join(out, filepath.FromSlash(d)), 0o755); err != nil {
			return sz, err
		}
	}
	if err := os.WriteFile(filepath.Join(out, siteMarker), []byte("written by explorer/tool; replaced on every build\n"), 0o644); err != nil {
		return sz, err
	}
	err := filepath.WalkDir(appDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(appDir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(out, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		sz.total += int64(len(b))
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		return sz, err
	}
	n, err := writeScript(filepath.Join(out, "data", "core.js"), "core", "", core)
	if err != nil {
		return sz, err
	}
	sz.core = n
	for _, c := range chunks {
		n, err := writeScript(filepath.Join(out, filepath.FromSlash(chunkPath(c.ID))), "chunk", c.ID, c)
		if err != nil {
			return sz, err
		}
		sz.chunks += n
		sz.nChunks++
	}
	for _, d := range docs {
		n, err := writeScript(filepath.Join(out, filepath.FromSlash(d.Chunk)), "doc", d.Path, DocText{Path: d.Path, Text: d.text})
		if err != nil {
			return sz, err
		}
		sz.docs += n
		sz.nDocs++
	}
	sz.total += sz.core + sz.chunks + sz.docs
	return sz, nil
}

// writeScript writes a data file: a call of explorerRegister.
func writeScript(path, kind, key string, v any) (int64, error) {
	var b bytes.Buffer
	b.WriteString("explorerRegister(")
	k, _ := json.Marshal(kind)
	b.Write(k)
	b.WriteString(",")
	kk, _ := json.Marshal(key)
	b.Write(kk)
	b.WriteString(",")
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return 0, err
	}
	// Encode ends with a newline.
	b.Truncate(b.Len() - 1)
	b.WriteString(");\n")
	return int64(b.Len()), os.WriteFile(path, b.Bytes(), 0o644)
}

// human formats a byte count.
func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// hostPath translates a path below the root to the host's view of the
// repository (the tool runs in a container that mounts it elsewhere).
func hostPath(root, hostRoot, p string) string {
	if hostRoot == "" {
		return p
	}
	if rel, ok := strings.CutPrefix(p, root); ok {
		return hostRoot + rel
	}
	return p
}
