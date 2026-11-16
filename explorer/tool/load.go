package main

// This file loads the module: go list finds the packages, their files and
// the export data of everything they import; the module's own packages are
// parsed and type-checked from source in one universe, so that an object
// is the same wherever it is used.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// listedPackage is the part of `go list -json` the tool reads.
type listedPackage struct {
	ImportPath     string
	Dir            string
	Name           string
	ForTest        string
	Export         string
	Standard       bool
	GoFiles        []string
	TestGoFiles    []string
	XTestGoFiles   []string
	IgnoredGoFiles []string
	Imports        []string
	TestImports    []string
	XTestImports   []string
	Module         *struct{ Path string }
	Error          *struct{ Err string }
}

// program is the loaded module.
type program struct {
	fset    *token.FileSet
	root    string // absolute repository root
	modPath string
	// pkgs are the module's packages in type-checking order (imports
	// first); external test packages come last.
	pkgs   []*pkgInfo
	byPath map[string]*pkgInfo // by Path ("internal/box")
	files  []*fileInfo         // every Go file, sorted by path
	// typeErrors are the type checker's complaints.
	typeErrors []string
}

// pkgInfo is one package of the module.
type pkgInfo struct {
	Path       string // directory relative to root; "<dir>_test" for an external test package
	ImportPath string
	Name       string
	XTest      bool
	ForPackage string
	Files      []*fileInfo // type-checked files (non-test and test)
	Ignored    []*fileInfo // files excluded by build constraints: parsed only
	// Imports are module package Paths imported by non-test files,
	// TestImports by test files only.
	Imports     []string
	TestImports []string
	Types       *types.Package
	Info        *types.Info
	listed      *listedPackage
}

// fileInfo is one Go file.
type fileInfo struct {
	Path    string // relative to root
	Pkg     *pkgInfo
	AST     *ast.File
	Src     []byte
	Test    bool
	Ignored bool
	tf      *token.File
	u16     []int32 // byte offset to UTF-16 offset; nil for ASCII files
}

// line returns the 1-based line of a position in the file.
func (f *fileInfo) line(p token.Pos) int { return f.tf.Line(p) }

// offset returns the byte offset of a position in the file.
func (f *fileInfo) offset(p token.Pos) int { return f.tf.Offset(p) }

// utf16 converts a byte offset to UTF-16 code units.
func (f *fileInfo) utf16(off int) int {
	if f.u16 == nil {
		return off
	}
	return int(f.u16[off])
}

// lineText returns the text of a line, without its newline.
func (f *fileInfo) lineText(line int) string {
	if line < 1 || line > f.tf.LineCount() {
		return ""
	}
	start := f.tf.Offset(f.tf.LineStart(line))
	end := len(f.Src)
	if line < f.tf.LineCount() {
		end = f.tf.Offset(f.tf.LineStart(line+1)) - 1
	}
	return string(f.Src[start:end])
}

// findRoot returns the directory holding go.mod at or above dir, and the
// module path.
func findRoot(dir string) (string, string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, "go.mod"))
		if err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
					return d, strings.Trim(strings.TrimSpace(rest), `"`), nil
				}
			}
			return "", "", fmt.Errorf("%s/go.mod has no module line", d)
		}
		if filepath.Dir(d) == d {
			return "", "", errors.New("no go.mod found; run the tool inside the repository")
		}
	}
}

// goList runs go list in root and decodes its stream of JSON objects.
func goList(root string, args ...string) ([]*listedPackage, error) {
	cmd := exec.Command("go", append([]string{"list", "-e", "-json"}, args...)...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var pkgs []*listedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list: decoding its output: %w", err)
		}
		pkgs = append(pkgs, &p)
	}
	return pkgs, nil
}

// skipDir reports whether a package directory is left out: the explorer
// does not describe itself.
func skipDir(rel string) bool {
	return rel == "explorer" || strings.HasPrefix(rel, "explorer/")
}

// loadProgram lists, parses and type-checks the module at root.
func loadProgram(root, modPath string) (*program, error) {
	listed, err := goList(root, "-deps", "-test", "-export", "-tags", "integration", "./...")
	if err != nil {
		return nil, err
	}
	return buildProgram(root, modPath, listed)
}

// buildProgram parses and type-checks the module packages among listed.
// External packages are imported from their export data.
func buildProgram(root, modPath string, listed []*listedPackage) (*program, error) {
	prog := &program{fset: token.NewFileSet(), root: root, modPath: modPath, byPath: map[string]*pkgInfo{}}
	exports := map[string]string{}
	var mods []*listedPackage
	for _, lp := range listed {
		if lp.ForTest != "" || strings.HasSuffix(lp.ImportPath, ".test") {
			continue
		}
		if lp.Module != nil && lp.Module.Path == modPath {
			rel := relDir(root, lp.Dir)
			if !skipDir(rel) {
				mods = append(mods, lp)
			}
			continue
		}
		if lp.Export != "" {
			exports[lp.ImportPath] = lp.Export
		}
	}
	isMod := map[string]bool{}
	for _, lp := range mods {
		isMod[lp.ImportPath] = true
	}
	modRel := func(paths []string) []string {
		var out []string
		for _, p := range paths {
			if isMod[p] {
				out = append(out, prog.dirOf(p))
			}
		}
		sort.Strings(out)
		return out
	}

	var xtests []*pkgInfo
	for _, lp := range mods {
		rel := relDir(root, lp.Dir)
		p := &pkgInfo{Path: rel, ImportPath: lp.ImportPath, Name: lp.Name, listed: lp}
		p.Imports = modRel(lp.Imports)
		prodImports := map[string]bool{}
		for _, i := range p.Imports {
			prodImports[i] = true
		}
		for _, i := range modRel(lp.TestImports) {
			if !prodImports[i] && i != rel {
				p.TestImports = append(p.TestImports, i)
			}
		}
		for _, name := range lp.GoFiles {
			f, err := prog.parse(rel, name, false)
			if err != nil {
				return nil, err
			}
			f.Pkg = p
			p.Files = append(p.Files, f)
		}
		for _, name := range lp.TestGoFiles {
			f, err := prog.parse(rel, name, true)
			if err != nil {
				return nil, err
			}
			f.Pkg = p
			p.Files = append(p.Files, f)
		}
		for _, name := range lp.IgnoredGoFiles {
			if !strings.HasSuffix(name, ".go") {
				continue
			}
			f, err := prog.parse(rel, name, strings.HasSuffix(name, "_test.go"))
			if err != nil {
				// A file for another platform may not even parse with
				// this toolchain's tags; show it without annotations.
				continue
			}
			f.Pkg, f.Ignored = p, true
			p.Ignored = append(p.Ignored, f)
		}
		prog.byPath[rel] = p
		if len(lp.XTestGoFiles) > 0 {
			x := &pkgInfo{Path: rel + "_test", ImportPath: lp.ImportPath + "_test", Name: lp.Name + "_test",
				XTest: true, ForPackage: rel}
			x.TestImports = modRel(lp.XTestImports)
			for _, name := range lp.XTestGoFiles {
				f, err := prog.parse(rel, name, true)
				if err != nil {
					return nil, err
				}
				f.Pkg = x
				x.Files = append(x.Files, f)
			}
			xtests = append(xtests, x)
		}
	}

	// Type-checking order: a package after everything its files (tests
	// included) import from the module. In-package tests cannot import a
	// package that imports the package under test, so there is no cycle.
	order, err := topoOrder(prog.byPath)
	if err != nil {
		return nil, err
	}
	prog.pkgs = append(order, xtests...)
	for _, x := range xtests {
		prog.byPath[x.Path] = x
	}

	ext := importer.ForCompiler(prog.fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %q", path)
		}
		return os.Open(f)
	})
	imp := &moduleImporter{prog: prog, ext: ext}
	for _, p := range prog.pkgs {
		p.Info = &types.Info{
			Types:      map[ast.Expr]types.TypeAndValue{},
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
		conf := types.Config{
			Importer: imp,
			Sizes:    types.SizesFor("gc", "amd64"),
			Error: func(err error) {
				prog.typeErrors = append(prog.typeErrors, prog.relError(err.Error()))
			},
		}
		files := make([]*ast.File, len(p.Files))
		for i, f := range p.Files {
			files[i] = f.AST
		}
		p.Types, _ = conf.Check(p.ImportPath, prog.fset, files, p.Info)
	}
	for _, p := range prog.pkgs {
		prog.files = append(prog.files, p.Files...)
		prog.files = append(prog.files, p.Ignored...)
	}
	sort.Slice(prog.files, func(i, j int) bool { return prog.files[i].Path < prog.files[j].Path })
	return prog, nil
}

// relError makes the file names in a type error relative to the root.
func (prog *program) relError(s string) string {
	return strings.ReplaceAll(s, prog.root+string(filepath.Separator), "")
}

// dirOf returns the Path of a module import path.
func (prog *program) dirOf(importPath string) string {
	if importPath == prog.modPath {
		return "."
	}
	return strings.TrimPrefix(importPath, prog.modPath+"/")
}

// relDir returns dir relative to root, with forward slashes.
func relDir(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return filepath.ToSlash(rel)
}

// parse reads and parses one Go file.
func (prog *program) parse(dir, name string, test bool) (*fileInfo, error) {
	rel := name
	if dir != "." {
		rel = dir + "/" + name
	}
	src, err := os.ReadFile(filepath.Join(prog.root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	af, err := parser.ParseFile(prog.fset, filepath.Join(prog.root, filepath.FromSlash(rel)), src,
		parser.ParseComments|parser.SkipObjectResolution)
	if af == nil {
		return nil, fmt.Errorf("%s: %v", rel, err)
	}
	if err != nil {
		prog.typeErrors = append(prog.typeErrors, prog.relError(err.Error()))
	}
	f := &fileInfo{Path: rel, AST: af, Src: src, Test: test}
	f.tf = prog.fset.File(af.Pos())
	f.u16 = utf16Table(src)
	return f, nil
}

// utf16Table maps byte offsets to UTF-16 offsets; nil when src is ASCII.
func utf16Table(src []byte) []int32 {
	ascii := true
	for _, b := range src {
		if b >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return nil
	}
	t := make([]int32, len(src)+1)
	n := int32(0)
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		// Continuation bytes map to the position of their rune.
		for k := 0; k < size; k++ {
			t[i+k] = n
		}
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
		i += size
	}
	t[len(src)] = n
	return t
}

// topoOrder orders packages so that each comes after the module packages
// its files import.
func topoOrder(byPath map[string]*pkgInfo) ([]*pkgInfo, error) {
	var paths []string
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	state := map[string]int{} // 1 visiting, 2 done
	var out []*pkgInfo
	var visit func(string, []string) error
	visit = func(p string, stack []string) error {
		switch state[p] {
		case 1:
			return fmt.Errorf("import cycle: %s", strings.Join(append(stack, p), " -> "))
		case 2:
			return nil
		}
		state[p] = 1
		pk := byPath[p]
		for _, d := range append(append([]string{}, pk.Imports...), pk.TestImports...) {
			if _, ok := byPath[d]; ok {
				if err := visit(d, append(stack, p)); err != nil {
					return err
				}
			}
		}
		state[p] = 2
		out = append(out, pk)
		return nil
	}
	for _, p := range paths {
		if err := visit(p, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// moduleImporter resolves module packages to the ones type-checked from
// source and everything else from export data.
type moduleImporter struct {
	prog *program
	ext  types.Importer
}

func (m *moduleImporter) Import(path string) (*types.Package, error) {
	if strings.HasPrefix(path, m.prog.modPath+"/") || path == m.prog.modPath {
		if p, ok := m.prog.byPath[m.prog.dirOf(path)]; ok && !p.XTest {
			if p.Types == nil {
				return nil, fmt.Errorf("package %s is not checked yet", path)
			}
			return p.Types, nil
		}
	}
	return m.ext.Import(path)
}

// gitInfo returns HEAD's hash and whether the tree is dirty.
func gitInfo(root string) (string, bool) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", false
	}
	commit := strings.TrimSpace(string(out))
	st, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	dirty := err == nil && len(bytes.TrimSpace(st)) > 0
	return commit, dirty
}

// readLines splits text into lines without newlines.
func readLines(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}
