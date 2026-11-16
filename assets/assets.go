// Package assets embeds the files shipped inside the devc binary: built-in
// harness manifests and their templates, built-in skills, the base seccomp profile and the
// Containerfile of the auxiliary installer image. It carries no security
// rules itself; the embedded files are trusted because they are part of the
// binary.
package assets

import "embed"

// Harness holds the built-in manifests as harness/<id>.toml.
//
//go:embed harness/*.toml
var Harness embed.FS

// HarnessTemplates holds the templates named by the built-in manifests
// (seeds and baseline settings) and the sandbox description, as
// harness/templates/<name>.
//
//go:embed harness/templates/*.tmpl
var HarnessTemplates embed.FS

// Skills holds the built-in skills as skills/<name>/… (work order C16:
// dep-source, which tells agents how to use devc src). The toolbox carries
// them, and every box sees them among its shares.
//
//go:embed skills
var Skills embed.FS

// SeccompBase is the default seccomp profile of containers/common at the
// release named in seccomp/SOURCE.txt. The box profile is derived from it
// by internal/toolbox.
//
//go:embed seccomp/default.json
var SeccompBase []byte

// InstallerContainerfile builds localhost/devc-aux/installer.
//
//go:embed installer/Containerfile
var InstallerContainerfile []byte
