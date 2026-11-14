// Package assets embeds the files shipped inside the devc binary: built-in
// harness manifests now, and later the seccomp profile, templates and hook
// configurations. It carries no security rules itself; the embedded files
// are trusted because they are part of the binary.
package assets

import "embed"

// Harness holds the built-in manifests as harness/<id>.toml.
//
//go:embed harness/*.toml
var Harness embed.FS
