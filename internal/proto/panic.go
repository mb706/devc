// This file describes recovered panics for logs, without what a panic's
// value may carry.

package proto

import (
	"fmt"
	"runtime"
	"strings"
)

// PanicText describes a recovered panic for a log (finding X6 2). A
// runtime error (an index out of range, a nil dereference, a failed type
// assertion) gives its message, which names indices and types but no
// data; any other value gives only its type, since a value the code
// panicked with could hold what it was working on, a secret among it.
func PanicText(r any) string {
	if e, ok := r.(runtime.Error); ok {
		return e.Error()
	}
	return fmt.Sprintf("a value of type %T", r)
}

// maxPanicFrames bounds the frames PanicSite lists.
const maxPanicFrames = 8

// PanicSite lists where the panic being handled happened, innermost
// first, as "function file:line" separated by " < ", without arguments.
// It must be called (directly or through one helper) from a function the
// panicking goroutine deferred, while the panic unwinds: the frames below
// runtime.gopanic and the runtime's own panic helpers are the code that
// panicked.
func PanicSite() string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var out []string
	seen := false
	for {
		f, more := frames.Next()
		switch {
		case !seen:
			seen = f.Function == "runtime.gopanic"
		case strings.HasPrefix(f.Function, "runtime.") && len(out) == 0:
			// goPanicIndex and the like, between gopanic and the code
		case len(out) < maxPanicFrames:
			out = append(out, fmt.Sprintf("%s %s:%d", shortFunc(f.Function), shortFile(f.File), f.Line))
		}
		if !more {
			break
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return strings.Join(out, " < ")
}

// shortFunc drops the module path: "daemon.(*boxWorker).serveGit".
func shortFunc(fn string) string {
	if i := strings.LastIndexByte(fn, '/'); i >= 0 {
		return fn[i+1:]
	}
	return fn
}

// shortFile keeps the last directory and the file name.
func shortFile(file string) string {
	parts := strings.Split(file, "/")
	if len(parts) > 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return file
}
