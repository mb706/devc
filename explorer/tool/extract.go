package main

// This file extracts the symbols of the module, every use of them, the call
// graph and which types implement which interfaces.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// world holds everything extracted from the program.
type world struct {
	prog    *program
	syms    map[string]*symInfo
	symList []*symInfo
	objRef  map[types.Object]string
	// declOf maps a function's Ref to its declaration (for the route table
	// and command closures).
	declOf map[string]*ast.FuncDecl

	usedBy map[string][]useRec // by target Ref
	links  map[*fileInfo][]linkRec
	// edges are the call graph: caller Ref -> callee Ref -> edge.
	edges map[string]map[string]*edgeRec
	// usesIn maps a declaration's Ref to the symbols it uses (for steps
	// whose code is not a function).
	usesIn  map[string]map[string]bool
	varUses map[string]map[string]int // declaration -> package variables it mentions -> line

	impls []Implementation
	// dyn maps an interface method to the methods that implement it.
	dyn map[types.Object][]string

	// watch maps the Ref of a constant whose uses are classified (RPC
	// methods, event types) to the sites found.
	watch map[string]*watched
	// envRE finds environment variable names in string literals.
	envRE   *regexp.Regexp
	envUses map[string][]Site
}

// symInfo is a Symbol with what the tool needs while working.
type symInfo struct {
	Symbol
	obj  types.Object
	file *fileInfo
}

// isFunc reports whether the symbol is a function or method with a body.
func (s *symInfo) isFunc() bool { return s.Kind == "func" || s.Kind == "method" }

// useRec is a use with its target.
type useRec struct {
	Use
	target string
}

// linkRec is one link in a source file, in bytes.
type linkRec struct {
	off, length int
	target      string
}

// edgeRec is one edge of the call graph.
type edgeRec struct {
	line    int
	dynamic bool
	via     string
}

// watched collects the classified sites of one constant.
type watched struct {
	handlers, callers, listed, tests []Site
}

func newWorld(prog *program) *world {
	return &world{
		prog:    prog,
		syms:    map[string]*symInfo{},
		objRef:  map[types.Object]string{},
		declOf:  map[string]*ast.FuncDecl{},
		usedBy:  map[string][]useRec{},
		links:   map[*fileInfo][]linkRec{},
		edges:   map[string]map[string]*edgeRec{},
		usesIn:  map[string]map[string]bool{},
		varUses: map[string]map[string]int{},
		dyn:     map[types.Object][]string{},
		watch:   map[string]*watched{},
		envUses: map[string][]Site{},
	}
}

// refOf returns the Ref of a module object, or "".
func (w *world) refOf(obj types.Object) string {
	switch o := obj.(type) {
	case *types.Func:
		obj = o.Origin()
	case *types.Var:
		obj = o.Origin()
	}
	return w.objRef[obj]
}

// ----- symbols

// collectSymbols records every declaration of the module.
func (w *world) collectSymbols() {
	for _, p := range w.prog.pkgs {
		for _, f := range p.Files {
			for _, d := range f.AST.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					w.addFunc(p, f, d)
				case *ast.GenDecl:
					w.addGen(p, f, d)
				}
			}
		}
	}
	sort.Slice(w.symList, func(i, j int) bool { return w.symList[i].Ref < w.symList[j].Ref })
}

// add records a symbol; a Ref taken already gets "#2", "#3".
func (w *world) add(s *symInfo) *symInfo {
	ref := s.Ref
	for n := 2; w.syms[ref] != nil; n++ {
		ref = fmt.Sprintf("%s#%d", s.Ref, n)
	}
	s.Ref = ref
	s.Exported = token.IsExported(s.Name)
	s.Test = s.file.Test
	s.File = s.file.Path
	s.Package = s.file.Pkg.Path
	w.syms[ref] = s
	w.symList = append(w.symList, s)
	if s.obj != nil {
		if _, dup := w.objRef[s.obj]; !dup {
			w.objRef[s.obj] = ref
		}
	}
	return s
}

func (w *world) span(f *fileInfo, n ast.Node) (int, int) {
	return f.line(n.Pos()), f.line(n.End())
}

func (w *world) addFunc(p *pkgInfo, f *fileInfo, d *ast.FuncDecl) {
	if d.Name.Name == "_" {
		return
	}
	s := &symInfo{file: f, obj: p.Info.Defs[d.Name]}
	s.Name = d.Name.Name
	s.Kind = "func"
	s.Ref = p.Path + "." + d.Name.Name
	if d.Recv != nil && len(d.Recv.List) > 0 {
		recv := recvTypeName(d.Recv.List[0].Type)
		s.Kind = "method"
		s.Parent = p.Path + "." + recv
		s.Ref = s.Parent + "." + d.Name.Name
	}
	fd := *d
	fd.Body, fd.Doc = nil, nil
	s.Signature = w.print(&fd)
	s.Doc = d.Doc.Text()
	s.Line, s.EndLine = w.span(f, d)
	s = w.add(s)
	w.declOf[s.Ref] = d
}

// recvTypeName returns the type name of a receiver expression.
func recvTypeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return "?"
		}
	}
}

func (w *world) addGen(p *pkgInfo, f *fileInfo, d *ast.GenDecl) {
	grouped := d.Lparen.IsValid()
	docOf := func(doc, comment *ast.CommentGroup) string {
		switch {
		case doc != nil:
			return doc.Text()
		case !grouped:
			if d.Doc != nil {
				return d.Doc.Text()
			}
			return comment.Text()
		case comment != nil:
			return comment.Text()
		}
		return d.Doc.Text()
	}
	for _, spec := range d.Specs {
		switch sp := spec.(type) {
		case *ast.TypeSpec:
			if sp.Name.Name == "_" {
				continue
			}
			s := &symInfo{file: f, obj: p.Info.Defs[sp.Name]}
			s.Name, s.Kind = sp.Name.Name, "type"
			s.Ref = p.Path + "." + sp.Name.Name
			s.Doc = docOf(sp.Doc, sp.Comment)
			s.Line, s.EndLine = w.span(f, sp)
			s.TypeKind = typeKind(sp, s.obj)
			s.Signature = w.typeSignature(sp)
			s = w.add(s)
			switch t := sp.Type.(type) {
			case *ast.StructType:
				w.addFields(p, f, t, s.Ref)
			case *ast.InterfaceType:
				w.addIfaceMethods(p, f, t, s.Ref)
			}
		case *ast.ValueSpec:
			kind := "var"
			if d.Tok == token.CONST {
				kind = "const"
			}
			for i, name := range sp.Names {
				if name.Name == "_" {
					continue
				}
				s := &symInfo{file: f, obj: p.Info.Defs[name]}
				s.Name, s.Kind = name.Name, kind
				s.Ref = p.Path + "." + name.Name
				s.Doc = docOf(sp.Doc, sp.Comment)
				s.Line, s.EndLine = w.span(f, sp)
				s.Signature = w.valueSignature(kind, name.Name, sp, i, s.obj)
				if c, ok := s.obj.(*types.Const); ok {
					s.Value = clip(c.Val().ExactString(), 300)
				}
				s = w.add(s)
				if st := anonStruct(sp.Type); st != nil {
					w.addFields(p, f, st, s.Ref)
				}
			}
		}
	}
}

// typeKind says what a declared type is.
func typeKind(sp *ast.TypeSpec, obj types.Object) string {
	if sp.Assign.IsValid() {
		return "alias"
	}
	switch t := sp.Type.(type) {
	case *ast.StructType:
		return "struct"
	case *ast.InterfaceType:
		return "interface"
	case *ast.FuncType:
		return "func"
	case *ast.MapType:
		return "map"
	case *ast.ArrayType:
		if t.Len == nil {
			return "slice"
		}
		return "array"
	case *ast.ChanType:
		return "chan"
	case *ast.StarExpr:
		return "pointer"
	}
	if obj != nil {
		if _, ok := obj.Type().Underlying().(*types.Basic); ok {
			return "basic"
		}
	}
	return "named"
}

// anonStruct returns the anonymous struct type inside a field or variable
// type (directly, or as the element of a pointer, slice, array or map).
func anonStruct(e ast.Expr) *ast.StructType {
	for e != nil {
		switch t := e.(type) {
		case *ast.StructType:
			return t
		case *ast.StarExpr:
			e = t.X
		case *ast.ArrayType:
			e = t.Elt
		case *ast.MapType:
			e = t.Value
		case *ast.ParenExpr:
			e = t.X
		default:
			return nil
		}
	}
	return nil
}

// embeddedIdent returns the identifier that names an embedded field.
func embeddedIdent(e ast.Expr) *ast.Ident {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.SelectorExpr:
			return t.Sel
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t
		default:
			return nil
		}
	}
}

func (w *world) addFields(p *pkgInfo, f *fileInfo, st *ast.StructType, owner string) {
	for _, fld := range st.Fields.List {
		doc := fld.Doc.Text()
		if doc == "" {
			doc = fld.Comment.Text()
		}
		names := fld.Names
		if len(names) == 0 {
			if id := embeddedIdent(fld.Type); id != nil {
				names = []*ast.Ident{id}
			}
		}
		for _, n := range names {
			if n.Name == "_" {
				continue
			}
			s := &symInfo{file: f, obj: p.Info.Defs[n]}
			s.Name, s.Kind, s.Parent = n.Name, "field", owner
			s.Ref = owner + "." + n.Name
			s.Doc = doc
			s.Line, s.EndLine = w.span(f, fld)
			s.Signature = w.fieldSignature(fld, n, len(fld.Names) == 0)
			s = w.add(s)
			if inner := anonStruct(fld.Type); inner != nil {
				w.addFields(p, f, inner, s.Ref)
			}
		}
	}
}

func (w *world) addIfaceMethods(p *pkgInfo, f *fileInfo, it *ast.InterfaceType, owner string) {
	for _, m := range it.Methods.List {
		if len(m.Names) == 0 {
			continue // an embedded interface or a type constraint
		}
		doc := m.Doc.Text()
		if doc == "" {
			doc = m.Comment.Text()
		}
		for _, n := range m.Names {
			s := &symInfo{file: f, obj: p.Info.Defs[n]}
			s.Name, s.Kind, s.Parent = n.Name, "imethod", owner
			s.Ref = owner + "." + n.Name
			s.Doc = doc
			s.Line, s.EndLine = w.span(f, m)
			if ft, ok := m.Type.(*ast.FuncType); ok {
				s.Signature = n.Name + strings.TrimPrefix(w.print(ft), "func")
			}
			w.add(s)
		}
	}
}

// print renders a node as source on one line.
func (w *world) print(n any) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, w.prog.fset, n); err != nil {
		return ""
	}
	return oneLine(b.String())
}

var lineBreakRE = regexp.MustCompile(`\n[ \t]*`)

// oneLine joins a printed node's lines.
func oneLine(s string) string {
	s = lineBreakRE.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "( ", "(")
	s = strings.ReplaceAll(s, ", )", ")")
	s = strings.ReplaceAll(s, "{ ", "{")
	return s
}

func (w *world) typeSignature(sp *ast.TypeSpec) string {
	tparams := ""
	if sp.TypeParams != nil {
		tparams = "[" + w.printFieldList(sp.TypeParams) + "]"
	}
	switch sp.Type.(type) {
	case *ast.StructType:
		return "type " + sp.Name.Name + tparams + " struct"
	case *ast.InterfaceType:
		return "type " + sp.Name.Name + tparams + " interface"
	}
	cp := *sp
	cp.Doc, cp.Comment = nil, nil
	return clip(w.print(&ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{&cp}}), 300)
}

func (w *world) printFieldList(fl *ast.FieldList) string {
	var parts []string
	for _, f := range fl.List {
		var names []string
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
		parts = append(parts, strings.TrimSpace(strings.Join(names, ", ")+" "+w.print(f.Type)))
	}
	return strings.Join(parts, ", ")
}

func (w *world) valueSignature(kind, name string, sp *ast.ValueSpec, i int, obj types.Object) string {
	s := kind + " " + name
	if sp.Type != nil {
		t := sp.Type
		if anonStruct(t) != nil {
			s += " " + clip(w.print(t), 60)
		} else {
			s += " " + w.print(t)
		}
	} else if obj != nil && kind == "const" {
		if named, ok := obj.Type().(*types.Named); ok {
			s += " " + types.TypeString(named, types.RelativeTo(obj.Pkg()))
		}
	}
	switch {
	case i < len(sp.Values):
		s += " = " + clip(w.print(sp.Values[i]), 100)
	case kind == "const" && obj != nil:
		if c, ok := obj.(*types.Const); ok {
			s += " = " + clip(c.Val().ExactString(), 100)
		}
	}
	return s
}

func (w *world) fieldSignature(fld *ast.Field, n *ast.Ident, embedded bool) string {
	var s string
	switch {
	case embedded:
		s = w.print(fld.Type)
	case anonStruct(fld.Type) != nil:
		s = n.Name + " " + clip(w.print(fld.Type), 60)
	default:
		s = n.Name + " " + w.print(fld.Type)
	}
	if fld.Tag != nil {
		s += " " + fld.Tag.Value
	}
	return s
}

// clip shortens s to at most n runes, ending in "…".
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// ----- implementations

// computeImplementations finds which module types implement which module
// interfaces, and the methods behind each interface method.
func (w *world) computeImplementations() {
	var ifaces, concrete []*symInfo
	for _, s := range w.symList {
		if s.Kind != "type" || s.obj == nil {
			continue
		}
		tn, ok := s.obj.(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok || named.TypeParams().Len() > 0 {
			continue
		}
		if it, ok := named.Underlying().(*types.Interface); ok {
			if it.NumMethods() > 0 {
				ifaces = append(ifaces, s)
			}
			continue
		}
		concrete = append(concrete, s)
	}
	names := map[*symInfo]map[string]bool{}
	sets := map[*symInfo][2]*types.MethodSet{}
	for _, t := range concrete {
		T := t.obj.Type()
		ms := [2]*types.MethodSet{types.NewMethodSet(T), types.NewMethodSet(types.NewPointer(T))}
		sets[t] = ms
		n := map[string]bool{}
		for i := 0; i < ms[1].Len(); i++ {
			n[ms[1].At(i).Obj().Name()] = true
		}
		names[t] = n
	}
	for _, is := range ifaces {
		I := is.obj.Type().Underlying().(*types.Interface)
	types:
		for _, t := range concrete {
			for i := 0; i < I.NumMethods(); i++ {
				if !names[t][I.Method(i).Name()] {
					continue types
				}
			}
			T := t.obj.Type()
			var ms *types.MethodSet
			pointer := false
			switch {
			case types.Implements(T, I):
				ms = sets[t][0]
			case types.Implements(types.NewPointer(T), I):
				ms, pointer = sets[t][1], true
			default:
				continue
			}
			w.impls = append(w.impls, Implementation{Type: t.Ref, Interface: is.Ref, Pointer: pointer})
			for i := 0; i < I.NumMethods(); i++ {
				m := I.Method(i)
				sel := ms.Lookup(m.Pkg(), m.Name())
				if sel == nil {
					continue
				}
				if r := w.refOf(sel.Obj()); r != "" {
					w.dyn[m] = appendUnique(w.dyn[m], r)
				}
			}
		}
	}
	sort.Slice(w.impls, func(i, j int) bool {
		if w.impls[i].Interface != w.impls[j].Interface {
			return w.impls[i].Interface < w.impls[j].Interface
		}
		return w.impls[i].Type < w.impls[j].Type
	})
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// ----- uses and the call graph

// walkUses visits every identifier of every type-checked file.
func (w *world) walkUses() {
	for _, f := range w.prog.files {
		if f.Ignored {
			continue
		}
		w.walkFile(f)
	}
	// A declaration that mentions a package variable whose initializer
	// mentions functions reaches them through it (dispatch tables).
	for from, vars := range w.varUses {
		for v, line := range vars {
			if len(w.edges[v]) > 0 && from != v {
				w.addEdge(from, v, line, false, "")
			}
		}
	}
}

// calleeIdent returns the identifier that names the function of a call.
func calleeIdent(e ast.Expr) *ast.Ident {
	for {
		switch t := e.(type) {
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.SelectorExpr:
			return t.Sel
		case *ast.Ident:
			return t
		default:
			return nil
		}
	}
}

func (w *world) walkFile(f *fileInfo) {
	info := f.Pkg.Info
	callees := map[*ast.Ident]bool{}
	fieldKeys := map[*ast.Ident]bool{}
	ast.Inspect(f.AST, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if id := calleeIdent(n.Fun); id != nil {
				callees[id] = true
			}
		case *ast.CompositeLit:
			for _, e := range n.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						if v, ok := info.Uses[id].(*types.Var); ok && v.IsField() {
							fieldKeys[id] = true
						}
					}
				}
			}
		}
		return true
	})
	visit := func(node ast.Node, in string) {
		var stack []ast.Node
		ast.Inspect(node, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch n := n.(type) {
			case *ast.Ident:
				w.ident(f, n, in, stack, callees, fieldKeys)
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					w.stringLit(f, n, in)
				}
			}
			return true
		})
	}
	// The package clause's name is not a use.
	for _, d := range f.AST.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			in := ""
			if obj := info.Defs[d.Name]; obj != nil {
				in = w.refOf(obj)
			}
			visit(d, in)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				in := ""
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					in = w.refOf(info.Defs[sp.Name])
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						if r := w.refOf(info.Defs[n]); r != "" {
							in = r
							break
						}
					}
				case *ast.ImportSpec:
					w.importSpec(f, sp)
					continue
				}
				visit(spec, in)
			}
		}
	}
}

// importSpec links an import of a module package to the package.
func (w *world) importSpec(f *fileInfo, sp *ast.ImportSpec) {
	path := strings.Trim(sp.Path.Value, "\"`")
	if path != w.prog.modPath && !strings.HasPrefix(path, w.prog.modPath+"/") {
		return
	}
	dir := w.prog.dirOf(path)
	if _, ok := w.prog.byPath[dir]; !ok {
		return
	}
	off := f.offset(sp.Path.Pos()) + 1
	w.links[f] = append(w.links[f], linkRec{off: off, length: len(path), target: "pkg:" + dir})
	if sp.Name != nil && sp.Name.Name != "_" && sp.Name.Name != "." {
		w.links[f] = append(w.links[f], linkRec{off: f.offset(sp.Name.Pos()), length: len(sp.Name.Name), target: "pkg:" + dir})
	}
}

func (w *world) ident(f *fileInfo, id *ast.Ident, in string, stack []ast.Node, callees, fieldKeys map[*ast.Ident]bool) {
	info := f.Pkg.Info
	obj := info.Uses[id]
	isDef := false
	if obj == nil {
		obj = info.Defs[id]
		isDef = true
	}
	if obj == nil {
		return
	}
	if pn, ok := obj.(*types.PkgName); ok {
		path := pn.Imported().Path()
		if path == w.prog.modPath || strings.HasPrefix(path, w.prog.modPath+"/") {
			if dir := w.prog.dirOf(path); w.prog.byPath[dir] != nil && !isDef {
				w.links[f] = append(w.links[f], linkRec{off: f.offset(id.Pos()), length: len(id.Name), target: "pkg:" + dir})
			}
		}
		return
	}
	target := w.refOf(obj)
	if target == "" {
		return
	}
	w.links[f] = append(w.links[f], linkRec{off: f.offset(id.Pos()), length: len(id.Name), target: target})
	if isDef {
		return
	}
	line := f.line(id.Pos())
	ts := w.syms[target]
	kind := "ref"
	switch {
	case callees[id] && ts.isFunc(), callees[id] && ts.Kind == "imethod":
		kind = "call"
	case ts.Kind == "type":
		kind = "type"
	case fieldKeys[id]:
		kind = "field"
	}
	w.usedBy[target] = append(w.usedBy[target], useRec{target: target, Use: Use{
		File: f.Path, Line: line, In: in, Kind: kind, Test: f.Test, Text: clip(strings.TrimSpace(f.lineText(line)), 160),
	}})
	if in != "" && in != target {
		if w.usesIn[in] == nil {
			w.usesIn[in] = map[string]bool{}
		}
		w.usesIn[in][target] = true
		switch {
		case ts.isFunc():
			w.addEdge(in, target, line, false, "")
		case ts.Kind == "imethod":
			for _, impl := range w.dyn[ts.obj] {
				w.addEdge(in, impl, line, true, target)
			}
		case ts.Kind == "var":
			if w.varUses[in] == nil {
				w.varUses[in] = map[string]int{}
			}
			if _, ok := w.varUses[in][target]; !ok {
				w.varUses[in][target] = line
			}
		}
	}
	if wt := w.watch[target]; wt != nil {
		site := Site{File: f.Path, Line: line, In: in}
		kind, handler := w.classify(stack, info, in)
		switch {
		case f.Test:
			wt.tests = append(wt.tests, site)
		case kind == "handler":
			site.Handler = handler
			wt.handlers = append(wt.handlers, site)
		case kind == "call":
			wt.callers = append(wt.callers, site)
		default:
			wt.listed = append(wt.listed, site)
		}
	}
}

// addEdge records a call graph edge; the first line wins, a static edge
// wins over a dynamic one.
func (w *world) addEdge(from, to string, line int, dynamic bool, via string) {
	m := w.edges[from]
	if m == nil {
		m = map[string]*edgeRec{}
		w.edges[from] = m
	}
	if e, ok := m[to]; ok {
		if e.dynamic && !dynamic {
			e.dynamic, e.via = false, ""
		}
		return
	}
	m[to] = &edgeRec{line: line, dynamic: dynamic, via: via}
}

// stringLit records environment variable names in a string literal.
func (w *world) stringLit(f *fileInfo, lit *ast.BasicLit, in string) {
	if w.envRE == nil {
		return
	}
	for _, name := range w.envRE.FindAllString(lit.Value, -1) {
		w.envUses[name] = append(w.envUses[name], Site{File: f.Path, Line: f.line(lit.Pos()), In: in})
	}
}

// classify says what a use of a watched constant does: "handler" (a key
// of a map literal or of an index assignment, a case, a comparison),
// "call" (an argument of a call, the value of a struct literal's field) or
// "listed". For a handler it also returns the handling code.
func (w *world) classify(stack []ast.Node, info *types.Info, in string) (string, string) {
	i := len(stack) - 1
	e := stack[i]
	if i > 0 {
		if sel, ok := stack[i-1].(*ast.SelectorExpr); ok && sel.Sel == e {
			e, i = sel, i-1
		}
	}
	for i > 0 {
		p, ok := stack[i-1].(*ast.ParenExpr)
		if !ok {
			break
		}
		e, i = p, i-1
	}
	if i == 0 {
		return "listed", ""
	}
	at := func(k int) ast.Node {
		if k < 0 {
			return nil
		}
		return stack[k]
	}
	switch p := stack[i-1].(type) {
	case *ast.KeyValueExpr:
		lit, _ := at(i - 2).(*ast.CompositeLit)
		if p.Key == e {
			if lit != nil && isMap(info.TypeOf(lit)) {
				return "handler", w.handlerOf(p.Value, info, in)
			}
			return "listed", ""
		}
		if lit != nil && isStruct(info.TypeOf(lit)) {
			return "call", ""
		}
		return "listed", ""
	case *ast.CaseClause:
		return "handler", in
	case *ast.BinaryExpr:
		if p.Op == token.EQL || p.Op == token.NEQ {
			return "handler", in
		}
	case *ast.IndexExpr:
		if p.Index == e {
			if as, ok := at(i - 2).(*ast.AssignStmt); ok {
				for k, lhs := range as.Lhs {
					if lhs == p && k < len(as.Rhs) {
						return "handler", w.handlerOf(as.Rhs[k], info, in)
					}
				}
			}
		}
	case *ast.CallExpr:
		if p.Fun == e {
			return "listed", ""
		}
		// An argument of a call that builds a handler being registered.
		if kv, ok := at(i - 2).(*ast.KeyValueExpr); ok && kv.Value == p {
			if lit, ok := at(i - 3).(*ast.CompositeLit); ok && isMap(info.TypeOf(lit)) {
				return "listed", ""
			}
		}
		return "call", ""
	}
	return "listed", ""
}

// handlerOf resolves the handler of a registration: a function or method
// value, the function called to build the handler, or in.
func (w *world) handlerOf(e ast.Expr, info *types.Info, in string) string {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = p.X
	}
	var id *ast.Ident
	switch t := e.(type) {
	case *ast.Ident:
		id = t
	case *ast.SelectorExpr:
		id = t.Sel
	case *ast.CallExpr:
		id = calleeIdent(t.Fun)
	}
	if id != nil {
		if fn, ok := info.Uses[id].(*types.Func); ok {
			if r := w.refOf(fn); r != "" {
				return r
			}
		}
	}
	return in
}

func isMap(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Map)
	return ok
}

func isStruct(t types.Type) bool {
	if t == nil {
		return false
	}
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	_, ok := t.Underlying().(*types.Struct)
	return ok
}
