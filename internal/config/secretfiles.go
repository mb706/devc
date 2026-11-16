package config

import (
	"path"
	"strings"
)

// SecretStatePatterns maps home.secret_files of a manifest (paths below
// /home/dev) to patterns relative to the harness's realm state directory
// durable/realms/<realm>/harness/<id>/ (work order C17): for strategy
// "shared" the home is state "home/" and each home.also directory
// "also/<dir>/"; for "private" each home.shared subdirectory <sub> is
// state "<sub>/". An entry outside every shared location maps to nothing
// (validation reports it). The archive applies the result in one place
// (archive.Excluded).
func (h *Harness) SecretStatePatterns() []string {
	var out []string
	for _, f := range h.Home.SecretFiles {
		out = append(out, secretStatePaths(&h.Home, f)...)
	}
	return out
}

// secretStatePaths maps one secret_files entry.
func secretStatePaths(hm *HarnessHome, f string) []string {
	var out []string
	under := func(dir, stateDir string) {
		if rel, ok := patternBelow(dir, f); ok {
			out = append(out, path.Join(stateDir, rel))
		}
	}
	switch hm.Strategy {
	case "shared":
		under(hm.Dir, "home")
		for _, a := range hm.Also {
			under(a, path.Join("also", a))
		}
	case "private":
		for _, sub := range hm.Shared {
			under(path.Join(hm.Dir, sub), sub)
		}
	}
	return out
}

// patternBelow returns the part of pattern f below directory dir (both
// relative to /home/dev). f's leading components must equal dir's
// (patterns are not expanded there); f naming dir itself yields ".".
func patternBelow(dir, f string) (string, bool) {
	if f == dir {
		return ".", true
	}
	rest, ok := strings.CutPrefix(f, dir+"/")
	return rest, ok && rest != ""
}

// validSecretPattern accepts path.Match syntax without character classes
// or escapes, which differ between implementations.
func validSecretPattern(f string) bool {
	if strings.ContainsAny(f, `[]\`) {
		return false
	}
	for _, c := range strings.Split(f, "/") {
		if _, err := path.Match(c, ""); err != nil {
			return false
		}
	}
	return true
}

// ArchiveMaxFile is [sync] max_file_size of the realm in bytes.
func (r *Realm) ArchiveMaxFile() int64 {
	return sizeOr(r.Sync.MaxFileSize, 256<<20)
}

// UploadRate is [sync] upload_rate of config.toml in bytes per second.
func (g *Global) UploadRate() int64 {
	return sizeOr(g.Sync.UploadRate, 4<<20)
}
