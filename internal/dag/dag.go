// Package dag implements the dependency graph used for planning and apply:
// deterministic topological ordering, cycle detection with readable cycle
// paths (Tarjan's SCC algorithm), and a bounded-parallel walk that skips
// the dependents of failed nodes.
package dag

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Graph is a directed graph of string node IDs. An edge from A to B means
// "A depends on B": B must be visited before A.
type Graph struct {
	nodes map[string]struct{}
	deps  map[string]map[string]struct{} // node -> nodes it depends on
	rdeps map[string]map[string]struct{} // node -> nodes depending on it
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{
		nodes: map[string]struct{}{},
		deps:  map[string]map[string]struct{}{},
		rdeps: map[string]map[string]struct{}{},
	}
}

// Add adds a node; adding an existing node is a no-op.
func (g *Graph) Add(n string) {
	if _, ok := g.nodes[n]; ok {
		return
	}
	g.nodes[n] = struct{}{}
	g.deps[n] = map[string]struct{}{}
	g.rdeps[n] = map[string]struct{}{}
}

// Has reports whether n is in the graph.
func (g *Graph) Has(n string) bool {
	_, ok := g.nodes[n]
	return ok
}

// AddEdge records that from depends on to, adding either node if missing.
func (g *Graph) AddEdge(from, to string) {
	g.Add(from)
	g.Add(to)
	g.deps[from][to] = struct{}{}
	g.rdeps[to][from] = struct{}{}
}

// Len returns the number of nodes.
func (g *Graph) Len() int { return len(g.nodes) }

// Nodes returns all nodes in sorted order.
func (g *Graph) Nodes() []string { return sortedKeys(g.nodes) }

// DependsOn returns the direct dependencies of n, sorted.
func (g *Graph) DependsOn(n string) []string { return sortedKeys(g.deps[n]) }

// Dependents returns the nodes that directly depend on n, sorted.
func (g *Graph) Dependents(n string) []string { return sortedKeys(g.rdeps[n]) }

// Edges returns every edge as [from, to] pairs, sorted.
func (g *Graph) Edges() [][2]string {
	var out [][2]string
	for _, from := range g.Nodes() {
		for _, to := range g.DependsOn(from) {
			out = append(out, [2]string{from, to})
		}
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CycleError reports dependency cycles. Each cycle is a path whose last
// element repeats the first.
type CycleError struct {
	Cycles [][]string
}

func (e *CycleError) Error() string {
	parts := make([]string, len(e.Cycles))
	for i, c := range e.Cycles {
		parts[i] = strings.Join(c, " -> ")
	}
	return "dependency cycle: " + strings.Join(parts, "; ")
}

// Cycles returns one representative cycle per strongly connected component
// that contains a cycle (including self-loops). It runs Tarjan's algorithm
// in O(V+E).
func (g *Graph) Cycles() [][]string {
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var sccs [][]string
	next := 0

	var strongconnect func(v string)
	strongconnect = func(v string) {
		index[v] = next
		low[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range g.DependsOn(v) {
			if _, seen := index[w]; !seen {
				strongconnect(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			_, selfLoop := g.deps[v][v]
			if len(comp) > 1 || selfLoop {
				sccs = append(sccs, comp)
			}
		}
	}
	for _, n := range g.Nodes() {
		if _, seen := index[n]; !seen {
			strongconnect(n)
		}
	}
	var cycles [][]string
	for _, comp := range sccs {
		cycles = append(cycles, g.cyclePath(comp))
	}
	sort.Slice(cycles, func(i, j int) bool { return cycles[i][0] < cycles[j][0] })
	return cycles
}

// cyclePath finds a concrete cycle through the smallest node of an SCC by
// breadth-first search restricted to the component.
func (g *Graph) cyclePath(comp []string) []string {
	in := map[string]bool{}
	for _, n := range comp {
		in[n] = true
	}
	sort.Strings(comp)
	start := comp[0]
	if _, self := g.deps[start][start]; self {
		return []string{start, start}
	}
	parent := map[string]string{}
	queue := []string{start}
	visited := map[string]bool{start: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nb := range g.DependsOn(cur) {
			if !in[nb] {
				continue
			}
			if nb == start {
				path := []string{start}
				for n := cur; n != start; n = parent[n] {
					path = append(path, n)
				}
				// path is start, cur, ..., (reverse order back to start)
				out := []string{start}
				for i := len(path) - 1; i >= 1; i-- {
					out = append(out, path[i])
				}
				return append(out, start)
			}
			if !visited[nb] {
				visited[nb] = true
				parent[nb] = cur
				queue = append(queue, nb)
			}
		}
	}
	return append(comp, start)
}

// Validate returns a *CycleError if the graph has cycles.
func (g *Graph) Validate() error {
	if c := g.Cycles(); len(c) > 0 {
		return &CycleError{Cycles: c}
	}
	return nil
}

type stringHeap []string

func (h stringHeap) Len() int           { return len(h) }
func (h stringHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h stringHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *stringHeap) Push(x any)        { *h = append(*h, x.(string)) }
func (h *stringHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// TopoSort returns nodes so that every node comes after its dependencies.
// Ties are broken lexically, so the order is deterministic. Complexity is
// O((V+E) log V).
func (g *Graph) TopoSort() ([]string, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	remaining := make(map[string]int, len(g.nodes))
	h := &stringHeap{}
	for n := range g.nodes {
		remaining[n] = len(g.deps[n])
		if remaining[n] == 0 {
			*h = append(*h, n)
		}
	}
	heap.Init(h)
	out := make([]string, 0, len(g.nodes))
	for h.Len() > 0 {
		n := heap.Pop(h).(string)
		out = append(out, n)
		for d := range g.rdeps[n] {
			remaining[d]--
			if remaining[d] == 0 {
				heap.Push(h, d)
			}
		}
	}
	return out, nil
}

// ErrUpstream is wrapped by errors reported for nodes skipped because a
// dependency failed.
var ErrUpstream = errors.New("skipped: a dependency failed")

// SkipError reports that a node was not visited because Dep failed.
type SkipError struct{ Dep string }

func (e *SkipError) Error() string { return fmt.Sprintf("skipped because %s failed", e.Dep) }

// Unwrap lets errors.Is(err, ErrUpstream) match.
func (e *SkipError) Unwrap() error { return ErrUpstream }

// Walk visits every node after all of its dependencies, running up to
// parallelism visits concurrently. When a visit fails, its transitive
// dependents are not visited and are reported with a *SkipError; unrelated
// branches continue. If ctx is cancelled, nodes that have not started are
// reported with ctx.Err(). The returned map holds only failed or skipped
// nodes. The graph must be acyclic.
func (g *Graph) Walk(ctx context.Context, parallelism int, visit func(ctx context.Context, node string) error) (map[string]error, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	if parallelism < 1 {
		parallelism = 1
	}
	remaining := make(map[string]int, len(g.nodes))
	failedDep := map[string]string{}
	results := map[string]error{}
	var ready []string
	for n := range g.nodes {
		remaining[n] = len(g.deps[n])
		if remaining[n] == 0 {
			ready = append(ready, n)
		}
	}
	type done struct {
		node string
		err  error
	}
	doneCh := make(chan done)
	inFlight := 0
	finished := 0
	var wg sync.WaitGroup

	// complete records a finished (or skipped) node and releases dependents.
	var complete func(n string, err error)
	complete = func(n string, err error) {
		finished++
		if err != nil {
			results[n] = err
		}
		for _, d := range g.Dependents(n) {
			if err != nil {
				if _, already := failedDep[d]; !already {
					cause := n
					var se *SkipError
					if errors.As(err, &se) {
						cause = se.Dep
					}
					failedDep[d] = cause
				}
			}
			remaining[d]--
			if remaining[d] == 0 {
				if cause, bad := failedDep[d]; bad {
					complete(d, &SkipError{Dep: cause})
				} else {
					ready = append(ready, d)
				}
			}
		}
	}

	for finished < len(g.nodes) {
		sort.Strings(ready)
		for len(ready) > 0 && inFlight < parallelism {
			n := ready[0]
			ready = ready[1:]
			if err := ctx.Err(); err != nil {
				complete(n, err)
				continue
			}
			inFlight++
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				doneCh <- done{node: n, err: visit(ctx, n)}
			}(n)
		}
		if inFlight == 0 {
			continue // everything left was completed synchronously above
		}
		d := <-doneCh
		inFlight--
		complete(d.node, d.err)
	}
	wg.Wait()
	return results, nil
}
