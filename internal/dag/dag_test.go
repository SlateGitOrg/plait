package dag

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTopoSortDeterministic(t *testing.T) {
	g := New()
	g.AddEdge("app", "db")
	g.AddEdge("app", "net")
	g.AddEdge("db", "net")
	g.Add("zeta")
	g.Add("alpha")
	order, err := g.TopoSort()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "alpha,net,db,app,zeta" {
		t.Fatalf("order %s", got)
	}
	if !g.Has("db") || g.Has("nope") || g.Len() != 5 {
		t.Fatal("membership")
	}
	if fmt.Sprint(g.Edges()) != "[[app db] [app net] [db net]]" {
		t.Fatalf("edges %v", g.Edges())
	}
	if fmt.Sprint(g.Dependents("net")) != "[app db]" {
		t.Fatal("dependents")
	}
}

func TestCycleDetection(t *testing.T) {
	g := New()
	g.AddEdge("a", "b")
	g.AddEdge("b", "c")
	g.AddEdge("c", "a")
	g.AddEdge("c", "d")
	g.AddEdge("x", "x")
	g.AddEdge("y", "z")
	_, err := g.TopoSort()
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("expected cycle error, got %v", err)
	}
	want := "dependency cycle: a -> b -> c -> a; x -> x"
	if err.Error() != want {
		t.Fatalf("got %q want %q", err.Error(), want)
	}
	if _, err := g.Walk(context.Background(), 2, func(context.Context, string) error { return nil }); err == nil {
		t.Fatal("walk must refuse cyclic graphs")
	}
}

// Property: for random DAGs (edges only from higher to lower index), the
// topological order always puts dependencies first, and Walk visits each
// node exactly once after its dependencies.
func TestRandomDAGProperties(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for iter := 0; iter < 50; iter++ {
		n := 2 + r.Intn(40)
		g := New()
		for i := 0; i < n; i++ {
			g.Add(fmt.Sprintf("n%02d", i))
		}
		for i := 0; i < n; i++ {
			for j := 0; j < i; j++ {
				if r.Float64() < 0.15 {
					g.AddEdge(fmt.Sprintf("n%02d", i), fmt.Sprintf("n%02d", j))
				}
			}
		}
		order, err := g.TopoSort()
		if err != nil {
			t.Fatal(err)
		}
		pos := map[string]int{}
		for i, n := range order {
			pos[n] = i
		}
		for _, e := range g.Edges() {
			if pos[e[0]] < pos[e[1]] {
				t.Fatalf("edge %v violated", e)
			}
		}
		var mu sync.Mutex
		visited := map[string]bool{}
		errs, err := g.Walk(context.Background(), 4, func(_ context.Context, node string) error {
			mu.Lock()
			defer mu.Unlock()
			for _, d := range g.DependsOn(node) {
				if !visited[d] {
					return fmt.Errorf("%s visited before dependency %s", node, d)
				}
			}
			if visited[node] {
				return fmt.Errorf("%s visited twice", node)
			}
			visited[node] = true
			return nil
		})
		if err != nil || len(errs) != 0 || len(visited) != n {
			t.Fatalf("walk: %v %v visited=%d/%d", err, errs, len(visited), n)
		}
	}
}

func TestWalkParallelismBound(t *testing.T) {
	g := New()
	for i := 0; i < 12; i++ {
		g.Add(fmt.Sprintf("n%d", i))
	}
	var cur, peak int32
	_, err := g.Walk(context.Background(), 3, func(context.Context, string) error {
		c := atomic.AddInt32(&cur, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if c <= p || atomic.CompareAndSwapInt32(&peak, p, c) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak != 3 {
		t.Fatalf("peak concurrency %d, want 3", peak)
	}
}

func TestWalkSkipsDependentsOfFailures(t *testing.T) {
	g := New()
	g.AddEdge("app", "db")
	g.AddEdge("web", "app")
	g.AddEdge("cache", "net")
	g.Add("db")
	boom := errors.New("boom")
	var visited sync.Map
	errs, err := g.Walk(context.Background(), 0, func(_ context.Context, n string) error {
		visited.Store(n, true)
		if n == "db" {
			return boom
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(errs["db"], boom) {
		t.Fatalf("db error: %v", errs["db"])
	}
	for _, n := range []string{"app", "web"} {
		var se *SkipError
		if !errors.As(errs[n], &se) || se.Dep != "db" || !errors.Is(errs[n], ErrUpstream) {
			t.Fatalf("%s should be skipped due to db: %v", n, errs[n])
		}
		if _, ran := visited.Load(n); ran {
			t.Fatalf("%s must not run", n)
		}
	}
	if !strings.Contains(errs["web"].Error(), "db failed") {
		t.Fatal(errs["web"])
	}
	for _, n := range []string{"cache", "net"} {
		if errs[n] != nil {
			t.Fatalf("independent branch %s affected: %v", n, errs[n])
		}
	}
}

func TestWalkCancelled(t *testing.T) {
	g := New()
	g.AddEdge("b", "a")
	g.AddEdge("c", "b")
	ctx, cancel := context.WithCancel(context.Background())
	errs, err := g.Walk(ctx, 1, func(context.Context, string) error {
		cancel()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(errs["b"], context.Canceled) {
		t.Fatalf("b: %v", errs["b"])
	}
	if !errors.Is(errs["c"], ErrUpstream) {
		t.Fatalf("c: %v", errs["c"])
	}
}

func BenchmarkTopoSort10k(b *testing.B) {
	g := New()
	for i := 0; i < 10000; i++ {
		g.Add(fmt.Sprintf("n%05d", i))
		if i > 0 {
			g.AddEdge(fmt.Sprintf("n%05d", i), fmt.Sprintf("n%05d", i/2))
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.TopoSort(); err != nil {
			b.Fatal(err)
		}
	}
}
