// Package assets embeds the files shipped inside the devc binary: built-in
// harness manifests, the base seccomp profile, and later templates and hook
// configurations. It carries no security rules itself; the embedded files
// are trusted because they are part of the binary.
package assets

import "embed"

// Harness holds the built-in manifests as harness/<id>.toml.
//
//go:embed harness/*.toml
var Harness embed.FS

// SeccompBase is the default seccomp profile of containers/common at the
// release named in seccomp/SOURCE.txt. The box profile is derived from it
// by internal/toolbox.
//
//go:embed seccomp/default.json
var SeccompBase []byte
