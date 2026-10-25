package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/SlateGitOrg/plait/internal/state"
)

// benchConfig is a layered configuration: n resources in chains of depth 5,
// each referencing its predecessor.
func benchConfig(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i%5 == 0 {
			fmt.Fprintf(&b, "resource \"mock_thing\" \"r%d\" { name = \"r%d\" }\n", i, i)
			continue
		}
		fmt.Fprintf(&b, "resource \"mock_thing\" \"r%d\" {\n  name  = \"r%d\"\n  value = mock_thing.r%d.id\n}\n", i, i, i-1)
	}
	return b.String()
}

func benchmarkPlan(b *testing.B, n int) {
	m := newMock()
	e := mockEngine(&testing.T{}, m, benchConfig(n))
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Plan(ctx, state.New(), PlanOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkApply(b *testing.B, n int) {
	m := newMock()
	e := mockEngine(&testing.T{}, m, benchConfig(n))
	ctx := context.Background()
	p, err := e.Plan(ctx, state.New(), PlanOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := e.Apply(ctx, p, nil)
		if err != nil || res.Err() != nil {
			b.Fatal(err, res.Err())
		}
	}
}

func BenchmarkPlan100(b *testing.B)   { benchmarkPlan(b, 100) }
func BenchmarkPlan1000(b *testing.B)  { benchmarkPlan(b, 1000) }
func BenchmarkApply100(b *testing.B)  { benchmarkApply(b, 100) }
func BenchmarkApply1000(b *testing.B) { benchmarkApply(b, 1000) }
