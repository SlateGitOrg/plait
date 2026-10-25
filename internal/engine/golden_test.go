package engine_test

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/archit2501/plait/internal/config"
	"github.com/archit2501/plait/internal/engine"
	"github.com/archit2501/plait/internal/provider"
	"github.com/archit2501/plait/internal/provider/cloudprov"
	"github.com/archit2501/plait/internal/provider/localfs"
	"github.com/archit2501/plait/internal/state"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestGoldenPlans plans each testdata/golden/<case> configuration against
// its optional state.json (without refreshing, so no provider API is
// contacted) and compares the rendered plan, the apply step graph and a
// JSON round trip of the plan with plan.golden.
func TestGoldenPlans(t *testing.T) {
	cases, err := filepath.Glob("testdata/golden/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no golden cases")
	}
	for _, dir := range cases {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			got := renderCase(t, dir)
			goldenPath := filepath.Join(dir, "plan.golden")
			if *update {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/engine -run TestGoldenPlans -update)", err)
			}
			if got != string(want) {
				t.Errorf("plan output differs from %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
			}
		})
	}
}

func goldenRegistry(dir string) provider.Registry {
	return provider.Registry{
		localfs.Name:   func() provider.Provider { return localfs.NewAt(dir) },
		cloudprov.Name: cloudprov.New,
	}
}

func renderCase(t *testing.T, dir string) string {
	t.Helper()
	cfg, err := config.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	vars, err := cfg.ResolveVariables(config.VarInputs{})
	if err != nil {
		t.Fatal(err)
	}
	st := state.New()
	if data, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		if err := json.Unmarshal(data, st); err != nil {
			t.Fatal(err)
		}
	}
	opts := engine.PlanOptions{SkipRefresh: true}
	if data, err := os.ReadFile(filepath.Join(dir, "options")); err == nil && strings.TrimSpace(string(data)) == "destroy" {
		opts.Destroy = true
	}
	e := engine.New(cfg, vars, goldenRegistry(t.TempDir()))
	p, err := e.Plan(context.Background(), st, opts)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := engine.Render(&b, p, engine.RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	steps, edges, err := engine.ApplyGraph(p)
	if err != nil {
		t.Fatal(err)
	}
	b.WriteString("\n--- apply steps ---\n")
	for _, s := range steps {
		b.WriteString(s + "\n")
	}
	b.WriteString("--- ordering (step: runs after) ---\n")
	for _, e := range edges {
		b.WriteString(e[0] + ": " + e[1] + "\n")
	}
	// A saved plan must render identically after a JSON round trip.
	data, err := engine.MarshalPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := engine.UnmarshalPlan(data)
	if err != nil {
		t.Fatal(err)
	}
	var again, first strings.Builder
	_ = engine.Render(&first, p, engine.RenderOptions{})
	_ = engine.Render(&again, back, engine.RenderOptions{})
	if first.String() != again.String() {
		t.Errorf("plan JSON round trip changed the rendering:\n%s\nvs\n%s", first.String(), again.String())
	}
	return b.String()
}
