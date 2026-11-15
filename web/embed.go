// Package web holds the hub's web app (work order C12): plain ES modules
// and CSS without a build step, icons, the manifest and service worker of
// the installable app, and the vendored libraries (vendor/README.md). The
// hub embeds these files and serves them (internal/hub/static.go); check.sh
// is a development script and is not embedded.
package web

import "embed"

// Files are the files the hub serves, by their path below web/.
//
//go:embed index.html sw.js manifest.webmanifest app/*.js app/*.css icons/*.svg icons/*.png
//go:embed vendor/*/*.js vendor/*/*.mjs vendor/*/*.css
var Files embed.FS
