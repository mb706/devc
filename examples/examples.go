// Package examples embeds the example configuration, which `devc init
// --example` writes to a new configuration directory and the tests copy.
package examples

import "embed"

// Config holds the directory config/ (dotfiles included).
//
//go:embed all:config
var Config embed.FS
