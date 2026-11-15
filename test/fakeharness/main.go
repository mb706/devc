// Command fakeagent stands in for a coding agent in tests. It is installed
// into the toolbox through a manifest with install.method = "local" and
// started through its shim like a real harness.
//
//	fakeagent --version          print a version
//	fakeagent -p '<command>'     run one command and exit
//	fakeagent                    print a prompt, read commands until "exit"
//
// Commands: env [NAME…], say <text>, write <path> <text>, sh <command>,
// sleep <seconds>, touch-state <relative path>, exit.
//
// The agent's own "config directory" comes from FAKEAGENT_DIR, which its
// manifest sets through home.env, so tests can tell the home strategies
// apart. Values of variables that look like secrets are printed as their
// SHA-256, never in clear.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const version = "fakeagent 1.0.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && args[0] == "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case len(args) == 2 && args[0] == "-p":
		// C04: call the hook receiver with a start event here.
		code, _ := command(args[1], stdout, stderr)
		// C04: report the end of the session here.
		return code
	case len(args) == 0:
		return interactive(stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "usage: fakeagent [--version | -p <command>]\n")
	return 2
}

func interactive(stdin io.Reader, stdout, stderr io.Writer) int {
	// C04: call the hook receiver with a start event and report "idle" here.
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	code := 0
	for {
		fmt.Fprint(stdout, "fake> ")
		if !sc.Scan() {
			fmt.Fprintln(stdout)
			return code
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// C04: report the prompt and "working" here, and the message and
		// "done" after the command; "ask <text>" will report a question.
		var done bool
		code, done = command(line, stdout, stderr)
		if done {
			// C04: write session.end here.
			return code
		}
	}
}

// command runs one command line and returns its exit code and whether the
// session should end.
func command(line string, stdout, stderr io.Writer) (int, bool) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	rest = strings.TrimSpace(rest)
	switch verb {
	case "exit":
		return 0, true
	case "say":
		fmt.Fprintln(stdout, rest)
	case "env":
		printEnv(strings.Fields(rest), stdout)
	case "write":
		p, text, ok := strings.Cut(rest, " ")
		if !ok || p == "" {
			fmt.Fprintln(stderr, "usage: write <path> <text>")
			return 2, false
		}
		if err := os.WriteFile(p, []byte(text+"\n"), 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, false
		}
	case "sh":
		cmd := exec.Command("/bin/sh", "-c", rest)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), false
			}
			fmt.Fprintln(stderr, err)
			return 1, false
		}
	case "sleep":
		s, err := strconv.ParseFloat(rest, 64)
		if err != nil || s < 0 {
			fmt.Fprintln(stderr, "usage: sleep <seconds>")
			return 2, false
		}
		time.Sleep(time.Duration(s * float64(time.Second)))
	case "touch-state":
		dir := os.Getenv("FAKEAGENT_DIR")
		if dir == "" || rest == "" || filepath.IsAbs(rest) || strings.Contains(rest, "..") {
			fmt.Fprintln(stderr, "usage: touch-state <relative path> (FAKEAGENT_DIR must be set)")
			return 2, false
		}
		p := filepath.Join(dir, rest)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, false
		}
		if err := os.WriteFile(p, []byte(os.Getenv("DEVC_PROJECT")+"\n"), 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, false
		}
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", verb)
		return 2, false
	}
	return 0, false
}

// defaultEnv is what "env" prints without arguments.
var defaultEnv = []string{"DEVC_SESSION", "DEVC_HARNESS", "DEVC_WORKTREE", "DEVC_REALM", "DEVC_PROJECT", "FAKEAGENT_DIR", "HOME", "FAKE_TOKEN"}

func secretLike(name string) bool {
	for _, s := range strings.Split(os.Getenv("FAKEAGENT_SECRETS"), ",") {
		if s != "" && s == name {
			return true
		}
	}
	u := strings.ToUpper(name)
	for _, w := range []string{"TOKEN", "SECRET", "KEY", "PASSWORD", "GRANT"} {
		if strings.Contains(u, w) {
			return true
		}
	}
	return false
}

func printEnv(names []string, w io.Writer) {
	if len(names) == 0 {
		names = defaultEnv
		if exe, err := os.Executable(); err == nil {
			fmt.Fprintf(w, "FAKEAGENT_EXE=%s\n", exe)
		}
	}
	for _, n := range names {
		v, ok := os.LookupEnv(n)
		switch {
		case !ok:
			fmt.Fprintf(w, "%s=<unset>\n", n)
		case secretLike(n):
			sum := sha256.Sum256([]byte(v))
			fmt.Fprintf(w, "%s=sha256:%s\n", n, hex.EncodeToString(sum[:]))
		default:
			fmt.Fprintf(w, "%s=%s\n", n, v)
		}
	}
}
