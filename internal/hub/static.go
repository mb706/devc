// The web app's files (work order C12 section 4), embedded from web/ and
// served to device certificates without a login (the login screen is part
// of the app; its code is no secret). Vendored files carry their version
// in the path and are cached for a year; the app's own files are
// revalidated (ETag) on every load. The content security policy of the
// front applies to them as to everything else.

package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/mb706/devc/web"
)

// staticFile is one embedded file ready to serve.
type staticFile struct {
	body      []byte
	ctype     string
	etag      string
	immutable bool
}

// staticTypes maps the extensions the hub serves to content types; files
// with another extension are not served.
var staticTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
}

var (
	staticOnce  sync.Once
	staticFiles map[string]staticFile
)

// loadStatic reads the embedded files once.
func loadStatic() map[string]staticFile {
	staticOnce.Do(func() {
		staticFiles = map[string]staticFile{}
		_ = fs.WalkDir(web.Files, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			ctype, ok := staticTypes[path.Ext(p)]
			if !ok {
				return nil
			}
			b, err := fs.ReadFile(web.Files, p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			staticFiles["/"+p] = staticFile{body: b, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:12]) + `"`,
				immutable: strings.HasPrefix(p, "vendor/")}
			return nil
		})
	})
	return staticFiles
}

// StaticPaths lists the URL paths of the web app's files (tests).
func StaticPaths() []string {
	var out []string
	for p := range loadStatic() {
		out = append(out, p)
	}
	return out
}

// serveIndex serves the app's page for / (the only part of the app another
// site may link to).
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, "/index.html")
}

// serveStatic serves an embedded file by its URL path.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, r.URL.Path)
}

// serveFile serves one embedded file, or 404 for any path that is not
// one: with its content type and ETag, cached for a year when it is
// vendored (its version is in the path), revalidated on every load
// otherwise.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, p string) {
	f, ok := loadStatic()[p]
	if !ok || path.Clean(p) != p {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	if f.immutable {
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(f.body)
}
