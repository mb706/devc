package main

// This file builds devc's real command tree and walks it.

import (
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mb706/devc/internal/cli"
)

// commandInfo is a Command with the seeds of its run function.
type commandInfo struct {
	Command
	// seeds are where the run function's code starts in the call graph:
	// what the function literal mentions, or the declared function.
	seeds []string
}

// buildCommandTree builds the tree with every devc variable unset and the
// configuration, state and runtime directories, HOME and the XDG
// directories pointing at an empty temporary directory, so that nothing of
// the user's is read.
func buildCommandTree() (*cobra.Command, error) {
	tmp, err := os.MkdirTemp("", "explorer-cmds-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	env := map[string]string{}
	for _, d := range []string{"config", "state", "run", "home", "xdg-config", "xdg-data", "xdg-state", "xdg-cache", "xdg-run"} {
		p := filepath.Join(tmp, d)
		if err := os.Mkdir(p, 0o700); err != nil {
			return nil, err
		}
		env[d] = p
	}
	vars := map[string]string{
		"DEVC_CONFIG_DIR": env["config"], "DEVC_STATE_DIR": env["state"], "DEVC_RUNTIME_DIR": env["run"],
		"HOME": env["home"], "XDG_CONFIG_HOME": env["xdg-config"], "XDG_DATA_HOME": env["xdg-data"],
		"XDG_STATE_HOME": env["xdg-state"], "XDG_CACHE_HOME": env["xdg-cache"], "XDG_RUNTIME_DIR": env["xdg-run"],
	}
	// The process environment too, in case a constructor reads it directly.
	saved := os.Environ()
	defer func() {
		os.Clearenv()
		for _, kv := range saved {
			if k, v, ok := strings.Cut(kv, "="); ok {
				os.Setenv(k, v)
			}
		}
	}()
	for _, kv := range saved {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "DEVC_") {
			os.Unsetenv(k)
		}
	}
	for k, v := range vars {
		os.Setenv(k, v)
	}
	getenv := func(k string) string { return vars[k] }
	return cli.CommandTree(getenv), nil
}

// extractCommands walks the command tree.
func (w *world) extractCommands(root *cobra.Command) []*commandInfo {
	var out []*commandInfo
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		ci := &commandInfo{Command: Command{Path: c.CommandPath(), Use: c.Use, Short: c.Short, Long: c.Long,
			Example: c.Example, Aliases: c.Aliases, Hidden: c.Hidden, Deprecated: c.Deprecated}}
		if c.HasParent() {
			ci.Parent = c.Parent().CommandPath()
		}
		persistent := c.PersistentFlags()
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			ci.Flags = append(ci.Flags, Flag{Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(),
				Default: f.DefValue, Usage: f.Usage, Persistent: persistent.Lookup(f.Name) != nil, Hidden: f.Hidden})
		})
		var fn any
		switch {
		case c.RunE != nil:
			fn = c.RunE
		case c.Run != nil:
			fn = c.Run
		}
		if fn != nil {
			w.resolveRun(ci, reflect.ValueOf(fn).Pointer())
		}
		out = append(out, ci)
		subs := c.Commands()
		sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
		for _, s := range subs {
			ci.Subcommands = append(ci.Subcommands, s.CommandPath())
		}
		for _, s := range subs {
			walk(s)
		}
	}
	walk(root)
	return out
}

// funcNameRE splits a runtime function name: package path, then the rest
// ("newBoxCmd.func1", "(*app).runX-fm", "groupRun").
var funcNameRE = regexp.MustCompile(`^(.*/[^.]*|[^./]*)\.(.*)$`)

// resolveRun maps a run function's program counter to the declared function
// it belongs to.
func (w *world) resolveRun(ci *commandInfo, pc uintptr) {
	rf := runtime.FuncForPC(pc)
	if rf == nil {
		return
	}
	name := rf.Name()
	file, line := rf.FileLine(rf.Entry())
	rel := ""
	if r, ok := strings.CutPrefix(file, w.prog.root+"/"); ok {
		rel = r
	} else if r, ok := strings.CutPrefix(file, w.prog.modPath+"/"); ok {
		rel = r
	}
	m := funcNameRE.FindStringSubmatch(name)
	if m == nil {
		return
	}
	pkgPath, rest := m[1], m[2]
	literal := regexp.MustCompile(`\.func\d+`).MatchString(rest)
	rest = strings.TrimSuffix(rest, "-fm")
	if i := strings.Index(rest, ".func"); i >= 0 {
		rest = rest[:i]
	}
	// The declared function from the name: "f", "(*T).m" or "T.m".
	declared := ""
	if strings.HasPrefix(pkgPath, w.prog.modPath) {
		dir := w.prog.dirOf(pkgPath)
		r := strings.NewReplacer("(*", "", ")", "").Replace(rest)
		declared = dir + "." + r
		if w.syms[declared] == nil {
			declared = ""
		}
	}
	ci.Run = declared
	ci.RunLiteral = literal
	if rel != "" && !strings.HasPrefix(file, "<") {
		ci.RunFile, ci.RunLine = rel, line
	} else if s := w.syms[declared]; s != nil {
		ci.RunFile, ci.RunLine = s.File, s.Line
	}
	if ci.Run == "" && ci.RunFile != "" {
		ci.Run = w.enclosingFunc(ci.RunFile, ci.RunLine)
	}
	ci.seeds = []string{ci.Run}
	if literal && ci.RunFile != "" {
		if seeds := w.literalSeeds(ci.RunFile, ci.RunLine); len(seeds) > 0 {
			ci.seeds = seeds
		}
	}
	if ci.Run == "" {
		ci.seeds = nil
	}
}

// enclosingFunc returns the innermost function or method declared around a
// line of a file.
func (w *world) enclosingFunc(file string, line int) string {
	best := ""
	bestLen := 1 << 30
	for _, s := range w.symList {
		if s.File == file && s.isFunc() && s.Line <= line && line <= s.EndLine && s.EndLine-s.Line < bestLen {
			best, bestLen = s.Ref, s.EndLine-s.Line
		}
	}
	return best
}

// literalSeeds returns the functions, methods and dispatch variables that
// the function literal starting on a line mentions, so that a command's
// reach starts from its own closure rather than from everything in the
// function that builds it. A local variable of the enclosing function that
// is assigned a function literal (send := func(…) {…}) and that the
// literal mentions is followed into that literal: commands that share a
// helper closure reach what it calls.
func (w *world) literalSeeds(file string, line int) []string {
	var f *fileInfo
	for _, g := range w.prog.files {
		if g.Path == file {
			f = g
			break
		}
	}
	if f == nil {
		return nil
	}
	var lit *ast.FuncLit
	var decl ast.Node
	for _, d := range f.AST.Decls {
		ast.Inspect(d, func(n ast.Node) bool {
			if lit != nil {
				return false
			}
			if fl, ok := n.(*ast.FuncLit); ok && f.line(fl.Pos()) == line {
				lit, decl = fl, d
				return false
			}
			return true
		})
		if lit != nil {
			break
		}
	}
	if lit == nil {
		return nil
	}
	info := f.Pkg.Info
	closures := localClosures(decl, info)
	seen := map[string]bool{}
	followed := map[*ast.FuncLit]bool{lit: true}
	var out []string
	var walk func(body ast.Node)
	walk = func(body ast.Node) {
		ast.Inspect(body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			obj := info.Uses[id]
			for _, cl := range closures[obj] {
				if !followed[cl] {
					followed[cl] = true
					walk(cl.Body)
				}
			}
			ref := w.refOf(obj)
			if ref == "" || seen[ref] {
				return true
			}
			s := w.syms[ref]
			if s.isFunc() || (s.Kind == "var" && len(w.edges[ref]) > 0) {
				seen[ref] = true
				out = append(out, ref)
			}
			if s.Kind == "imethod" {
				for _, impl := range w.dyn[s.obj] {
					if !seen[impl] {
						seen[impl] = true
						out = append(out, impl)
					}
				}
			}
			return true
		})
	}
	walk(lit.Body)
	return out
}

// localClosures maps the local variables of a declaration that are
// assigned function literals (v := func…, v = func…, var v = func…) to
// those literals.
func localClosures(decl ast.Node, info *types.Info) map[types.Object][]*ast.FuncLit {
	out := map[types.Object][]*ast.FuncLit{}
	add := func(id *ast.Ident, e ast.Expr) {
		fl, ok := ast.Unparen(e).(*ast.FuncLit)
		if !ok || id == nil {
			return
		}
		obj := info.Defs[id]
		if obj == nil {
			obj = info.Uses[id]
		}
		if v, ok := obj.(*types.Var); ok && !v.IsField() && v.Parent() != v.Pkg().Scope() {
			out[v] = append(out[v], fl)
		}
	}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == len(n.Rhs) {
				for i, l := range n.Lhs {
					id, _ := l.(*ast.Ident)
					add(id, n.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			if len(n.Names) == len(n.Values) {
				for i, id := range n.Names {
					add(id, n.Values[i])
				}
			}
		}
		return true
	})
	return out
}
