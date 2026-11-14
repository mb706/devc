// Package version holds build identification set at link time by ./dev.
// It carries no security rules.
package version

import (
	"fmt"
	"runtime"
)

// Version is overridden with -ldflags "-X github.com/mb706/devc/internal/version.Version=…".
var Version = "dev"

// String is the one-line form printed by `devc version`.
func String() string {
	return fmt.Sprintf("devc %s %s %s/%s", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
