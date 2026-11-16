package main

// This file extracts the tables of entry points that interfaces.toml
// names: RPC methods, event types, routes, configuration keys and
// environment variables.

import (
	"go/ast"
	"go/constant"
	"go/types"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// defaultEnvPattern is used when no env surface gives a pattern.
const defaultEnvPattern = `DEVC_[A-Z0-9_]+`

// constSets returns, for each rpc and events surface, the string constants
// of its package with its prefix.
func (w *world) constsOf(s Surface) []*symInfo {
	pkg := strings.TrimPrefix(s.Consts, "pkg:")
	var out []*symInfo
	for _, sym := range w.symList {
		if sym.Kind != "const" || sym.Package != pkg || sym.Test || !strings.HasPrefix(sym.Name, s.Prefix) {
			continue
		}
		c, ok := sym.obj.(*types.Const)
		if !ok || c.Val().Kind() != constant.String {
			continue
		}
		out = append(out, sym)
	}
	return out
}

// prepareWatch registers the constants whose uses are classified during
// the walk, and the environment pattern.
func (w *world) prepareWatch(surfaces []Surface) {
	pattern := defaultEnvPattern
	for _, s := range surfaces {
		switch s.Kind {
		case "rpc", "events":
			for _, c := range w.constsOf(s) {
				if w.watch[c.Ref] == nil {
					w.watch[c.Ref] = &watched{}
				}
			}
		case "env":
			if s.Pattern != "" {
				pattern = s.Pattern
			}
		}
	}
	if re, err := regexp.Compile(pattern); err == nil {
		w.envRE = re
	}
}

// rpcTable builds the RPC methods of all rpc surfaces.
func (w *world) rpcTable(surfaces []Surface) []RPCMethod {
	byName := map[string]*RPCMethod{}
	var names []string
	for _, s := range surfaces {
		if s.Kind != "rpc" {
			continue
		}
		in, listedIn := map[string]bool{}, map[string]bool{}
		for _, h := range s.HandlersIn {
			in[h] = true
		}
		for _, l := range s.ListedIn {
			listedIn[l] = true
		}
		for _, c := range w.constsOf(s) {
			name := constant.StringVal(c.obj.(*types.Const).Val())
			m := byName[name]
			if m == nil {
				wt := w.watch[c.Ref]
				m = &RPCMethod{Name: name, Const: c.Ref, Doc: c.Doc, Handlers: wt.handlers, Callers: wt.callers,
					Listed: wt.listed, Tests: wt.tests}
				byName[name] = m
				names = append(names, name)
			}
			for _, h := range m.Handlers {
				if in[h.In] {
					m.Surfaces = appendUnique(m.Surfaces, s.ID)
					break
				}
			}
			for _, l := range m.Listed {
				if listedIn[l.In] {
					m.Surfaces = appendUnique(m.Surfaces, s.ID)
					break
				}
			}
		}
	}
	sort.Strings(names)
	out := make([]RPCMethod, 0, len(names))
	for _, n := range names {
		out = append(out, *byName[n])
	}
	return out
}

// eventTable builds the event types of all events surfaces.
func (w *world) eventTable(surfaces []Surface) []EventType {
	var out []EventType
	seen := map[string]bool{}
	for _, s := range surfaces {
		if s.Kind != "events" {
			continue
		}
		for _, c := range w.constsOf(s) {
			name := constant.StringVal(c.obj.(*types.Const).Val())
			if seen[name] {
				continue
			}
			seen[name] = true
			wt := w.watch[c.Ref]
			out = append(out, EventType{Name: name, Const: c.Ref, Doc: c.Doc, Writers: wt.callers,
				Readers: wt.handlers, Other: wt.listed, Tests: wt.tests, Surface: s.ID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// routeTable reads the route tables of all http surfaces. Problems are
// returned as messages for --check.
func (w *world) routeTable(surfaces []Surface) ([]Route, []string) {
	var out []Route
	var problems []string
	for _, s := range surfaces {
		if s.Kind != "http" {
			continue
		}
		decl := w.declOf[s.Table]
		sym := w.syms[s.Table]
		if decl == nil || sym == nil {
			problems = append(problems, "table "+s.Table+" is not a function of the module")
			continue
		}
		f := sym.file
		info := f.Pkg.Info
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			t := info.TypeOf(lit)
			if t == nil {
				return true
			}
			var elem types.Type
			switch u := t.Underlying().(type) {
			case *types.Slice:
				elem = u.Elem()
			case *types.Array:
				elem = u.Elem()
			default:
				return true
			}
			st, ok := elem.Underlying().(*types.Struct)
			if !ok {
				return true
			}
			for _, e := range lit.Elts {
				el, ok := e.(*ast.CompositeLit)
				if !ok {
					continue
				}
				r := Route{File: f.Path, Line: f.line(el.Pos()), Surface: s.ID}
				values := map[string]ast.Expr{}
				for i, x := range el.Elts {
					if kv, ok := x.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							values[id.Name] = kv.Value
						}
					} else if i < st.NumFields() {
						values[st.Field(i).Name()] = x
					}
				}
				str := func(role string) string {
					x := values[s.Fields[role]]
					if x == nil {
						return ""
					}
					if tv, ok := info.Types[x]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
						return constant.StringVal(tv.Value)
					}
					return ""
				}
				named := func(role string) (string, string) {
					x := values[s.Fields[role]]
					var id *ast.Ident
					switch v := x.(type) {
					case *ast.Ident:
						id = v
					case *ast.SelectorExpr:
						id = v.Sel
					}
					if id == nil {
						return "", ""
					}
					return id.Name, w.refOf(info.Uses[id])
				}
				r.Method, r.Path = str("method"), str("path")
				r.Access, r.AccessRef = named("access")
				r.CrossSite, r.CrossSiteRef = named("xsite")
				if x := values[s.Fields["handler"]]; x != nil {
					ast.Inspect(x, func(n ast.Node) bool {
						if r.Handler != "" {
							return false
						}
						if id, ok := n.(*ast.Ident); ok {
							if fn, ok := info.Uses[id].(*types.Func); ok {
								r.Handler = w.refOf(fn)
							}
						}
						return true
					})
				}
				r.Key = r.Method + " " + r.Path
				out = append(out, r)
			}
			return false
		})
	}
	return out, problems
}

// configTable follows the toml-tagged fields of each config surface's root
// type.
func (w *world) configTable(surfaces []Surface) ([]ConfigKey, []string) {
	var out []ConfigKey
	var problems []string
	for _, s := range surfaces {
		if s.Kind != "config" {
			continue
		}
		sym := w.syms[s.Root]
		if sym == nil || sym.Kind != "type" {
			problems = append(problems, "root "+s.Root+" is not a type of the module")
			continue
		}
		st, ok := sym.obj.Type().Underlying().(*types.Struct)
		if !ok {
			problems = append(problems, "root "+s.Root+" is not a struct type")
			continue
		}
		seen := map[types.Type]bool{sym.obj.Type(): true}
		w.walkConfig(s, st, s.Key, "", seen, &out)
	}
	return out, problems
}

func (w *world) walkConfig(s Surface, st *types.Struct, prefix, parent string, seen map[types.Type]bool, out *[]ConfigKey) {
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		tag := reflect.StructTag(st.Tag(i)).Get("toml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || !f.Exported() {
			continue
		}
		if f.Embedded() && name == "" {
			if inner, ok := deref(f.Type()).Underlying().(*types.Struct); ok {
				w.walkConfig(s, inner, prefix, parent, seen, out)
			}
			continue
		}
		if name == "" {
			name = f.Name()
		}
		key := prefix + "." + name
		ref := w.refOf(f)
		k := ConfigKey{Key: key, Parent: parent, Surface: s.ID, Kind: "value", Field: ref,
			Type: types.TypeString(f.Type(), func(p *types.Package) string { return p.Name() })}
		if sym := w.syms[ref]; sym != nil {
			k.Doc = sym.Doc
		}
		t := deref(f.Type())
		var child *types.Struct
		childPrefix := key
		switch u := t.Underlying().(type) {
		case *types.Struct:
			k.Kind, child = "table", u
		case *types.Slice:
			if es, ok := deref(u.Elem()).Underlying().(*types.Struct); ok {
				k.Kind, child = "tables", es
				t = deref(u.Elem())
			}
		case *types.Map:
			k.Kind = "map"
			if es, ok := deref(u.Elem()).Underlying().(*types.Struct); ok {
				child, childPrefix = es, key+".<name>"
				t = deref(u.Elem())
			}
		}
		*out = append(*out, k)
		if child != nil && !seen[t] {
			seen[t] = true
			w.walkConfig(s, child, childPrefix, key, seen, out)
			delete(seen, t)
		}
	}
}

func deref(t types.Type) types.Type {
	for {
		p, ok := t.Underlying().(*types.Pointer)
		if !ok {
			return types.Unalias(t)
		}
		t = p.Elem()
	}
}

// envTable gathers the environment variables found in Go string literals
// and in the other files' text.
func (w *world) envTable(others []*otherFile) []EnvVar {
	byName := map[string]*EnvVar{}
	get := func(n string) *EnvVar {
		v := byName[n]
		if v == nil {
			v = &EnvVar{Name: n}
			byName[n] = v
		}
		return v
	}
	for n, sites := range w.envUses {
		get(n).Uses = append(get(n).Uses, sites...)
	}
	if w.envRE != nil {
		for _, of := range others {
			if of.text == "" {
				continue
			}
			for i, l := range strings.Split(of.text, "\n") {
				for _, n := range w.envRE.FindAllString(l, -1) {
					get(n).Uses = append(get(n).Uses, Site{File: of.Path, Line: i + 1})
				}
			}
		}
	}
	for _, s := range w.symList {
		if s.Kind != "const" || s.obj == nil {
			continue
		}
		if c, ok := s.obj.(*types.Const); ok && c.Val().Kind() == constant.String {
			if v := byName[constant.StringVal(c.Val())]; v != nil {
				v.Consts = append(v.Consts, s.Ref)
			}
		}
	}
	var out []EnvVar
	for _, v := range byName {
		sort.Slice(v.Uses, func(i, j int) bool {
			if v.Uses[i].File != v.Uses[j].File {
				return v.Uses[i].File < v.Uses[j].File
			}
			return v.Uses[i].Line < v.Uses[j].Line
		})
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
