// Command fakeagent stands in for a coding agent in tests. It is installed
// into the toolbox through a manifest with install.method = "local" and
// started through its shim like a real harness.
//
//	fakeagent --version              print a version
//	fakeagent [--no-end] -p '<command>'  run one command and exit
//	fakeagent [--no-end]             print a prompt, read commands until "exit"
//
// Commands: env [NAME…], say <text>, write <path> <text>, sh <command>,
// sleep <seconds>, touch-state <relative path>, ask <question> (waits for
// the next line as the answer), die [code] (exits at once, without an end
// event), exit.
//
// Like a real harness it reports what it does through devc's hook
// receiver (`devc box hook fake <event>`, JSON {"type", "data"} on stdin):
// a harness and an idle state event at start; for each input line a prompt
// and "working", then a message and "done"; for ask a question and
// "blocked"; session.end at exit, unless --no-end is given. The receiver
// is FAKEAGENT_HOOK, by default /opt/devc/bin/devc; hook failures are
// ignored.
//
// The agent's own "config directory" comes from FAKEAGENT_DIR, which its
// manifest sets through home.env, so tests can tell the home strategies
// apart. Values of variables that look like secrets are printed as their
// SHA-256, never in clear.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const version = "fakeagent 1.0.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// noEnd suppresses the session.end event (--no-end).
var noEnd bool

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	noEnd = false
	if len(args) > 0 && args[0] == "--no-end" {
		noEnd, args = true, args[1:]
	}
	switch {
	case len(args) == 1 && args[0] == "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case len(args) == 2 && args[0] == "-p":
		started()
		code, done := turn(args[1], nil, stdout, stderr)
		if !done {
			end("exit")
		}
		return code
	case len(args) == 0:
		return interactive(stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "usage: fakeagent [--no-end] [--version | -p <command>]\n")
	return 2
}

// hook reports one event through devc's hook receiver.
func hook(typ string, data any) {
	exe := os.Getenv("FAKEAGENT_HOOK")
	if exe == "" {
		exe = "/opt/devc/bin/devc"
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm()&0o111 == 0 {
		return
	}
	in, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	cmd := exec.Command(exe, "box", "hook", "fake", typ)
	cmd.Stdin = bytes.NewReader(in)
	_ = cmd.Run()
}

func started() {
	cwd, _ := os.Getwd()
	hook("harness", map[string]any{"id": fmt.Sprintf("fake-%d", os.Getpid()), "cwd": cwd, "source": "startup"})
	hook("state", map[string]any{"state": "idle"})
}

func end(reason string) {
	if !noEnd {
		hook("session.end", map[string]any{"reason": reason})
	}
}

func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// turn handles one input line as a harness turn: prompt and "working",
// the command, then the message and "done". ask also reports a question,
// "blocked", and takes the next line from next as the answer.
func turn(line string, next func() (string, bool), stdout, stderr io.Writer) (int, bool) {
	verb, rest, _ := strings.Cut(line, " ")
	switch verb {
	case "exit":
		end("exit")
		return 0, true
	case "die":
		code, _ := strconv.Atoi(strings.TrimSpace(rest))
		return code, true
	}
	sum := sha256.Sum256([]byte(line))
	hook("prompt", map[string]any{"sha256": hex.EncodeToString(sum[:]), "length": utf8.RuneCountInString(line), "text": firstRunes(line, 2000)})
	hook("state", map[string]any{"state": "working"})
	var buf bytes.Buffer
	out, errw := io.MultiWriter(stdout, &buf), io.MultiWriter(stderr, &buf)
	code := 0
	if verb == "ask" {
		question := strings.TrimSpace(rest)
		fmt.Fprintf(stdout, "question: %s\n", question)
		hook("question", map[string]any{"id": fmt.Sprintf("q%d", time.Now().UnixNano()), "text": firstRunes(question, 4000)})
		hook("state", map[string]any{"state": "blocked", "kind": "question"})
		answer, ok := "", false
		if next != nil {
			fmt.Fprint(stdout, "answer> ")
			answer, ok = next()
		}
		if !ok {
			return 0, false
		}
		hook("state", map[string]any{"state": "working"})
		fmt.Fprintf(out, "answer: %s\n", answer)
	} else {
		code, _ = command(line, out, errw)
	}
	msg := strings.TrimSpace(buf.String())
	if msg == "" {
		msg = "ok: " + verb
	}
	hook("message", map[string]any{"text": firstRunes(msg, 4000)})
	hook("state", map[string]any{"state": "done"})
	return code, false
}

func interactive(stdin io.Reader, stdout, stderr io.Writer) int {
	started()
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	next := func() (string, bool) {
		if !sc.Scan() {
			return "", false
		}
		return strings.TrimSpace(sc.Text()), true
	}
	code := 0
	for {
		fmt.Fprint(stdout, "fake> ")
		line, ok := next()
		if !ok {
			fmt.Fprintln(stdout)
			end("eof")
			return code
		}
		if line == "" {
			continue
		}
		var done bool
		code, done = turn(line, next, stdout, stderr)
		if done {
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
