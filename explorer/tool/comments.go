package main

// This file is the comments-only subcommand: it checks that the Go code
// changed since a commit only in its comments (what content chunks may
// change).

import (
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runCommentsOnly(args []string, stdout, stderr io.Writer) int {
	rev := "HEAD"
	switch len(args) {
	case 0:
	case 1:
		rev = args[0]
	default:
		fmt.Fprintln(stderr, "usage: explore comments-only [commit]")
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "explore: %v\n", err)
		return 1
	}
	root, _, err := findRoot(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "explore: %v\n", err)
		return 1
	}
	git := func(a ...string) ([]byte, error) {
		return exec.Command("git", append([]string{"-C", root}, a...)...).Output()
	}
	changed, err := git("diff", "--name-only", rev, "--", "*.go")
	if err != nil {
		fmt.Fprintf(stderr, "explore: git diff: %v\n", err)
		return 1
	}
	untracked, _ := git("ls-files", "--others", "--exclude-standard", "--", "*.go")
	files := append(strings.Fields(string(changed)), strings.Fields(string(untracked))...)
	bad := 0
	for _, f := range files {
		old, oldErr := git("show", rev+":"+f)
		cur, curErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		switch {
		case oldErr != nil && curErr == nil:
			fmt.Fprintf(stdout, "%s: a new Go file\n", f)
			bad++
			continue
		case oldErr == nil && curErr != nil:
			fmt.Fprintf(stdout, "%s: removed\n", f)
			bad++
			continue
		case oldErr != nil:
			continue
		}
		if line, same := sameCode(old, cur); !same {
			fmt.Fprintf(stdout, "%s: line %d: the code changed, not only comments\n", f, line)
			bad++
		}
	}
	fmt.Fprintf(stdout, "explore: %d Go files differ from %s; %d changed more than comments\n", len(files), rev, bad)
	if bad > 0 {
		return 1
	}
	return 0
}

// codeToken is one token of Go code, comments left out.
type codeToken struct {
	tok  token.Token
	lit  string
	line int
}

// codeTokens scans Go source without comments.
func codeTokens(src []byte) []codeToken {
	fset := token.NewFileSet()
	tf := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(tf, src, func(token.Position, string) {}, 0)
	var out []codeToken
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON {
			lit = ";"
		}
		out = append(out, codeToken{tok: tok, lit: lit, line: tf.Line(pos)})
	}
	return out
}

// sameCode compares two versions of a file without their comments; when
// they differ it returns the line in cur where they start to.
func sameCode(old, cur []byte) (int, bool) {
	a, b := codeTokens(old), codeTokens(cur)
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i].tok != b[i].tok || a[i].lit != b[i].lit {
			return b[i].line, false
		}
	}
	if len(a) != len(b) {
		line := bytes.Count(cur, []byte("\n")) + 1
		if len(b) > len(a) {
			line = b[len(a)].line
		}
		return line, false
	}
	return 0, true
}
