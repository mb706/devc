package main

// This file describes files: line counts, file and package comments,
// highlighting tokens, the files that are not Go, and the chunks that carry
// sources to the app.

import (
	"bytes"
	"go/ast"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// goFileFacts are the line counts and tokens of one Go file.
type goFileFacts struct {
	lines  LineCounts
	tokens []int // absolute triples in bytes: start, length, class
}

// scanGo tokenizes a Go file for highlighting and counts its lines.
func scanGo(src []byte) goFileFacts {
	fset := token.NewFileSet()
	tf := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(tf, src, func(token.Position, string) {}, scanner.ScanComments)
	nlines := bytes.Count(src, []byte("\n"))
	if len(src) > 0 && src[len(src)-1] != '\n' {
		nlines++
	}
	code := make([]bool, nlines+2)
	comment := make([]bool, nlines+2)
	var facts goFileFacts
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		off := tf.Offset(pos)
		length := len(lit)
		class := 0
		switch {
		case tok.IsKeyword():
			class, length = 1, len(tok.String())
		case tok == token.STRING || tok == token.CHAR:
			class = 2
		case tok == token.COMMENT:
			class = 3
		case tok == token.INT || tok == token.FLOAT || tok == token.IMAG:
			class = 4
		}
		if length == 0 {
			length = len(tok.String())
		}
		if off+length > len(src) {
			length = len(src) - off
		}
		first := tf.Line(pos)
		last := first + bytes.Count(src[off:off+length], []byte("\n"))
		for l := first; l <= last && l < len(code); l++ {
			if class == 3 {
				comment[l] = true
			} else {
				code[l] = true
			}
		}
		if class != 0 {
			facts.tokens = append(facts.tokens, off, length, class)
		}
	}
	for l := 1; l <= nlines; l++ {
		switch {
		case code[l]:
			facts.lines.Code++
		case comment[l]:
			facts.lines.Comment++
		default:
			facts.lines.Blank++
		}
	}
	return facts
}

// fileComment returns a Go file's file comment: a comment group above the
// package clause, separated from it by a blank line, that is not the
// package comment and not only directives (//go:build, // +build).
func fileComment(f *fileInfo) string {
	pkgLine := f.line(f.AST.Package)
	var best string
	for _, cg := range f.AST.Comments {
		if cg.Pos() >= f.AST.Package {
			break
		}
		if cg == f.AST.Doc || f.line(cg.End()) >= pkgLine-1 {
			continue
		}
		if isConstraint(cg) {
			continue
		}
		if t := strings.TrimSpace(cg.Text()); t != "" {
			best = cg.Text()
		}
	}
	return best
}

// isConstraint reports whether a comment group holds only build
// constraints or other directives.
func isConstraint(cg *ast.CommentGroup) bool {
	for _, c := range cg.List {
		t := c.Text
		if !strings.HasPrefix(t, "//go:") && !strings.HasPrefix(t, "// +build") {
			return false
		}
	}
	return true
}

// otherFileRoots are the places outside Go packages that the site shows.
var otherFileRoots = []string{"web", "assets", "examples", "test", "dev", "go.mod"}

// skipOther reports whether a path is left out of the other files.
func skipOther(rel string) bool {
	return strings.HasPrefix(rel, "web/vendor/") || rel == "web/vendor" ||
		strings.HasPrefix(rel, "legacy/") || strings.HasPrefix(rel, "bin/") || strings.HasPrefix(rel, "explorer/")
}

// otherFile is a file that is not Go.
type otherFile struct {
	File
	text string
}

// collectOtherFiles reads the files that are not Go below otherFileRoots.
func collectOtherFiles(root string) ([]*otherFile, error) {
	var out []*otherFile
	for _, top := range otherFileRoots {
		start := filepath.Join(root, top)
		if _, err := os.Lstat(start); err != nil {
			continue
		}
		err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
			if d.IsDir() {
				if skipOther(rel + "/") {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || strings.HasSuffix(rel, ".go") || skipOther(rel) {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			of := &otherFile{File: File{Path: rel, Bytes: len(b), Lang: langOf(rel, b)}}
			of.Test = strings.HasPrefix(rel, "test/") || strings.Contains(rel, "/testdata/")
			if of.Lang != "binary" {
				of.text = string(b)
				for _, l := range readLines(b) {
					if strings.TrimSpace(l) == "" {
						of.Lines.Blank++
					} else {
						of.Lines.Code++
					}
				}
			}
			out = append(out, of)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// langOf guesses a file's language from its name and content.
func langOf(rel string, b []byte) string {
	if len(b) > 2<<20 || bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 || !utf8.Valid(b) {
		return "binary"
	}
	base := path.Base(rel)
	switch strings.ToLower(path.Ext(rel)) {
	case ".js", ".ts", ".mjs":
		return "javascript"
	case ".css":
		return "css"
	case ".html", ".htm", ".svg", ".xml":
		return "html"
	case ".sh", ".bash":
		return "shell"
	case ".toml":
		return "toml"
	case ".json", ".webmanifest":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".md":
		return "markdown"
	case ".tmpl":
		return "template"
	case ".png", ".jpg", ".jpeg", ".gif", ".ico", ".woff", ".woff2":
		return "binary"
	}
	if strings.HasPrefix(base, "Containerfile") || strings.HasPrefix(base, "Dockerfile") {
		return "dockerfile"
	}
	if bytes.HasPrefix(b, []byte("#!")) {
		first := string(b[:bytes.IndexByte(append(b, '\n'), '\n')])
		if strings.Contains(first, "sh") {
			return "shell"
		}
	}
	return "text"
}

// chunkID returns the chunk of a Go package or of a directory's other files.
func chunkID(goPkg bool, dir string) string {
	if dir == "" {
		dir = "."
	}
	if goPkg {
		return "go~" + strings.ReplaceAll(dir, "/", "~")
	}
	return "dir~" + strings.ReplaceAll(dir, "/", "~")
}

// chunkPath is the data file of a chunk, relative to index.html.
func chunkPath(id string) string { return "data/chunks/" + id + ".js" }

// sourceFile builds the annotated text of a Go file.
func (w *world) sourceFile(f *fileInfo, facts goFileFacts) SourceFile {
	sf := SourceFile{Path: f.Path, Source: string(f.Src)}
	prev := 0
	for i := 0; i+2 < len(facts.tokens); i += 3 {
		start := f.utf16(facts.tokens[i])
		end := f.utf16(facts.tokens[i] + facts.tokens[i+1])
		sf.Tokens = append(sf.Tokens, start-prev, end-start, facts.tokens[i+2])
		prev = start
	}
	links := w.links[f]
	sort.SliceStable(links, func(i, j int) bool { return links[i].off < links[j].off })
	targets := map[string]int{}
	prev = 0
	lastOff := -1
	for _, l := range links {
		if l.off == lastOff {
			continue // an embedded field's name is linked once
		}
		lastOff = l.off
		idx, ok := targets[l.target]
		if !ok {
			idx = len(sf.Targets)
			targets[l.target] = idx
			sf.Targets = append(sf.Targets, l.target)
		}
		start := f.utf16(l.off)
		end := f.utf16(l.off + l.length)
		sf.Links = append(sf.Links, start-prev, end-start, idx)
		prev = start
	}
	return sf
}

// buildFilesAndPackages fills Core.Files and Core.Packages and returns the
// chunks with their sources (used-by and call data are added later).
func (w *world) buildFilesAndPackages(others []*otherFile) ([]File, []Package, map[string]*Chunk, map[*fileInfo]goFileFacts) {
	chunks := map[string]*Chunk{}
	facts := map[*fileInfo]goFileFacts{}
	var files []File
	pkgFiles := map[string][]string{}
	for _, f := range w.prog.files {
		ff := scanGo(f.Src)
		facts[f] = ff
		id := chunkID(true, f.Pkg.Path)
		file := File{Path: f.Path, Package: f.Pkg.Path, Lang: "go", Test: f.Test, Ignored: f.Ignored,
			Lines: ff.lines, Bytes: len(f.Src), Chunk: chunkPath(id)}
		file.Doc = fileComment(f)
		files = append(files, file)
		pkgFiles[f.Pkg.Path] = append(pkgFiles[f.Pkg.Path], f.Path)
		c := chunks[id]
		if c == nil {
			c = &Chunk{ID: id, Package: f.Pkg.Path}
			chunks[id] = c
		}
		c.Files = append(c.Files, w.sourceFile(f, ff))
	}
	for _, of := range others {
		file := of.File
		if of.Lang != "binary" {
			id := chunkID(false, path.Dir(of.Path))
			file.Chunk = chunkPath(id)
			c := chunks[id]
			if c == nil {
				c = &Chunk{ID: id}
				chunks[id] = c
			}
			c.Files = append(c.Files, SourceFile{Path: of.Path, Source: of.text})
		}
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	importedBy := map[string][]string{}
	for _, p := range w.prog.pkgs {
		for _, imp := range p.Imports {
			importedBy[imp] = append(importedBy[imp], p.Path)
		}
	}
	var pkgs []Package
	for _, p := range w.prog.pkgs {
		pk := Package{Path: p.Path, ImportPath: p.ImportPath, Name: p.Name, Imports: p.Imports,
			TestImports: p.TestImports, ImportedBy: importedBy[p.Path], XTest: p.XTest, ForPackage: p.ForPackage,
			Chunk: chunkPath(chunkID(true, p.Path))}
		sort.Strings(pk.ImportedBy)
		pk.Files = pkgFiles[p.Path]
		sort.Strings(pk.Files)
		nonTest := 0
		for _, f := range append(append([]*fileInfo{}, p.Files...), p.Ignored...) {
			lc := facts[f].lines
			if f.Test {
				addLines(&pk.TestLines, lc)
			} else if !f.Ignored {
				addLines(&pk.Lines, lc)
				nonTest++
			}
		}
		pk.TestOnly = nonTest == 0 && !p.XTest
		// The package comment: from a non-test file first, in file order.
		for _, test := range []bool{false, true} {
			for _, f := range p.Files {
				if f.Test == test && f.AST.Doc != nil && pk.Doc == "" {
					pk.Doc, pk.DocFile = f.AST.Doc.Text(), f.Path
				}
			}
		}
		pkgs = append(pkgs, pk)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Path < pkgs[j].Path })
	return files, pkgs, chunks, facts
}

func addLines(dst *LineCounts, src LineCounts) {
	dst.Code += src.Code
	dst.Comment += src.Comment
	dst.Blank += src.Blank
}

// fillChunkGraph adds the used-by lists and call edges to the packages'
// chunks.
func (w *world) fillChunkGraph(chunks map[string]*Chunk) int {
	total := 0
	chunkOf := func(s *symInfo) *Chunk { return chunks[chunkID(true, s.Package)] }
	for ref, uses := range w.usedBy {
		s := w.syms[ref]
		c := chunkOf(s)
		if c == nil {
			continue
		}
		sort.SliceStable(uses, func(i, j int) bool {
			if uses[i].File != uses[j].File {
				return uses[i].File < uses[j].File
			}
			return uses[i].Line < uses[j].Line
		})
		var out []Use
		for _, u := range uses {
			if n := len(out); n > 0 && out[n-1].File == u.File && out[n-1].Line == u.Line && out[n-1].In == u.In && out[n-1].Kind == u.Kind {
				continue
			}
			out = append(out, u.Use)
		}
		if c.UsedBy == nil {
			c.UsedBy = map[string][]Use{}
		}
		c.UsedBy[ref] = out
		s.Uses = len(out)
		total += len(out)
	}
	for from, tos := range w.edges {
		s := w.syms[from]
		if s == nil {
			continue
		}
		c := chunkOf(s)
		if c == nil {
			continue
		}
		var calls []Call
		for to, e := range tos {
			calls = append(calls, Call{Ref: to, Line: e.line, Dynamic: e.dynamic, Via: e.via})
			if e.dynamic {
				if tc := chunkOf(w.syms[to]); tc != nil {
					if tc.DynamicCallers == nil {
						tc.DynamicCallers = map[string][]Call{}
					}
					tc.DynamicCallers[to] = append(tc.DynamicCallers[to], Call{Ref: from, Line: e.line, Dynamic: true, Via: e.via})
				}
			}
		}
		sortCalls(calls)
		if c.Calls == nil {
			c.Calls = map[string][]Call{}
		}
		c.Calls[from] = calls
	}
	for _, c := range chunks {
		for _, calls := range c.DynamicCallers {
			sortCalls(calls)
		}
	}
	return total
}

func sortCalls(calls []Call) {
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].Line != calls[j].Line {
			return calls[i].Line < calls[j].Line
		}
		return calls[i].Ref < calls[j].Ref
	})
}
