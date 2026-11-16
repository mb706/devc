package main

// This file computes where code runs (which actors' entry points reach
// which functions) and searches paths in the call graph.

import (
	"sort"
	"strings"
)

// reachNote is shown wherever the app says where code runs.
const reachNote = "Where code runs is an over-approximation: a function is listed for an actor when the call graph " +
	"reaches it from that actor's entry points (the run functions of the commands that start it, and other entry " +
	"points the model names). The graph has an edge for every call or mention of a function or method, and for a " +
	"call through an interface of this module an edge to that method of every type of the module that implements " +
	"it. Code in a function literal counts as code of the function around it. So a function listed may never run in " +
	"that actor; one not listed does not run there through a call the graph can see (function values held in fields " +
	"or passed through interfaces of other modules are not followed). Test code is left out."

// commandActors assigns every command to the actor that names it or its
// nearest parent: a command in an actor's list stands for itself and its
// subcommands, except those another actor names.
func commandActors(actors []Actor, cmds map[string]*commandInfo) map[string]string {
	named := map[string]string{}
	for _, a := range actors {
		for _, c := range a.Commands {
			named[c] = a.ID
		}
	}
	out := map[string]string{}
	for path := range cmds {
		for p := path; p != ""; {
			if a, ok := named[p]; ok {
				out[path] = a
				break
			}
			i := strings.LastIndex(p, " ")
			if i < 0 {
				break
			}
			p = p[:i]
		}
	}
	return out
}

// computeReach marks every function with the actors that reach it and
// returns the shares per package.
func (w *world) computeReach(actors []Actor, cmds map[string]*commandInfo) ([]ActorReach, map[string]map[string]float64) {
	var reach []ActorReach
	reached := map[string]map[string]bool{} // actor -> refs
	assigned := commandActors(actors, cmds)
	for _, a := range actors {
		if len(a.Commands) == 0 && len(a.Entry) == 0 {
			continue
		}
		var seeds []string
		for _, path := range keysOf(cmds) {
			if assigned[path] == a.ID {
				seeds = append(seeds, cmds[path].seeds...)
			}
		}
		for _, e := range a.Entry {
			if w.syms[e] != nil {
				seeds = append(seeds, e)
			}
		}
		set := w.reachable(seeds)
		reached[a.ID] = set
		n := 0
		for r := range set {
			if s := w.syms[r]; s != nil && s.isFunc() {
				n++
			}
		}
		reach = append(reach, ActorReach{Actor: a.ID, Seeds: seeds, Functions: n})
	}
	for _, s := range w.symList {
		if !s.isFunc() {
			continue
		}
		for _, a := range reach {
			if reached[a.Actor][s.Ref] {
				s.Actors = append(s.Actors, a.Actor)
			}
		}
	}
	shares := map[string]map[string]float64{}
	total := map[string]int{}
	per := map[string]map[string]int{}
	for _, s := range w.symList {
		if !s.isFunc() || s.Test {
			continue
		}
		n := s.EndLine - s.Line + 1
		total[s.Package] += n
		for _, a := range s.Actors {
			if per[s.Package] == nil {
				per[s.Package] = map[string]int{}
			}
			per[s.Package][a] += n
		}
	}
	for pkg, m := range per {
		shares[pkg] = map[string]float64{}
		for a, n := range m {
			shares[pkg][a] = float64(int(float64(n)/float64(total[pkg])*1000+0.5)) / 1000
		}
	}
	return reach, shares
}

// reachable returns everything the call graph reaches from seeds, test
// code left out.
func (w *world) reachable(seeds []string) map[string]bool {
	seen := map[string]bool{}
	queue := []string{}
	for _, s := range seeds {
		if !seen[s] {
			seen[s] = true
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for to := range w.edges[cur] {
			if seen[to] {
				continue
			}
			if s := w.syms[to]; s == nil || s.Test {
				continue
			}
			seen[to] = true
			queue = append(queue, to)
		}
	}
	return seen
}

// pathResult is a path found in the call graph.
type pathResult struct {
	path []string
	dyn  []int // indexes in path whose incoming edge is dynamic
}

// Bounds of a path search: queries look further than the flow checks,
// where a long path through the over-approximated graph would more likely
// be one that never happens than a real connection.
const (
	maxPathDepth = 14
	flowDepth    = 8
	maxVisited   = 200000
)

// findPath searches the shortest path from any source to target. A target
// that is not a function (a type, field, constant) is reached when a
// function on the way uses it. Test code is skipped unless a source is
// test code.
func (w *world) findPath(sources []string, target string) (pathResult, bool) {
	return w.findPathDepth(sources, target, maxPathDepth)
}

// findPathDepth is findPath with a bound on the path's length in edges.
func (w *world) findPathDepth(sources []string, target string, maxDepth int) (pathResult, bool) {
	ts := w.syms[target]
	if ts == nil {
		return pathResult{}, false
	}
	funcTarget := ts.isFunc() || ts.Kind == "var"
	allowTest := false
	parent := map[string]string{}
	depth := map[string]int{}
	var queue []string
	for _, s := range sources {
		if w.syms[s] == nil {
			continue
		}
		if s == target {
			return pathResult{path: []string{target}}, true
		}
		if w.syms[s].Test {
			allowTest = true
		}
		if _, ok := depth[s]; !ok {
			depth[s] = 0
			parent[s] = ""
			queue = append(queue, s)
		}
	}
	build := func(end string, extra string) pathResult {
		var p []string
		for n := end; n != ""; n = parent[n] {
			p = append([]string{n}, p...)
		}
		if extra != "" {
			p = append(p, extra)
		}
		var dyn []int
		for i := 1; i < len(p); i++ {
			if e := w.edges[p[i-1]][p[i]]; e != nil && e.dynamic {
				dyn = append(dyn, i)
			}
		}
		return pathResult{path: p, dyn: dyn}
	}
	visited := 0
	for len(queue) > 0 && visited < maxVisited {
		cur := queue[0]
		queue = queue[1:]
		visited++
		if !funcTarget && w.usesIn[cur][target] {
			return build(cur, target), true
		}
		if depth[cur] >= maxDepth {
			continue
		}
		tos := make([]string, 0, len(w.edges[cur]))
		for to := range w.edges[cur] {
			tos = append(tos, to)
		}
		sort.Strings(tos)
		// Static edges first, so a path prefers them.
		sort.SliceStable(tos, func(i, j int) bool {
			return !w.edges[cur][tos[i]].dynamic && w.edges[cur][tos[j]].dynamic
		})
		for _, to := range tos {
			if _, ok := depth[to]; ok {
				continue
			}
			if s := w.syms[to]; s == nil || (s.Test && !allowTest) {
				continue
			}
			depth[to] = depth[cur] + 1
			parent[to] = cur
			if to == target {
				return build(to, ""), true
			}
			queue = append(queue, to)
		}
	}
	return pathResult{}, false
}
