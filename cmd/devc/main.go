// Command devc is the single devc binary. It dispatches on the name it was
// invoked under: "devc" runs the CLI; "git-remote-devc" and harness shim
// names are other roles of the same binary, filled in by later chunks.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mb706/devc/internal/cli"
)

func main() {
	os.Exit(dispatch(filepath.Base(os.Args[0])))
}

func dispatch(name string) int {
	switch {
	case name == "devc" || strings.HasPrefix(name, "devc-"):
		// devc-linux-amd64 and similar release names run the CLI too.
		return cli.Run()
	case name == "git-remote-devc":
		fmt.Fprintln(os.Stderr, "git-remote-devc: not implemented")
		return 2
	default:
		fmt.Fprintf(os.Stderr, "devc: invoked as %q: not implemented (harness shims arrive in a later version)\n", name)
		return 2
	}
}
