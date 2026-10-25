package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/archit2501/plait/internal/addr"
	"github.com/archit2501/plait/internal/config"
	"github.com/archit2501/plait/internal/provider"
	"github.com/archit2501/plait/internal/state"
	"github.com/archit2501/plait/internal/value"
)

var tStr = value.Type{Kind: value.TypeString}

// mockProvider is a scriptable in-memory provider for engine tests.
type mockProvider struct {
	mu      sync.Mutex
	objects map[string]value.Value // id -> object
	byToken map[string]string      // create token -> id
	nextID  int
	ops     []string
	// failCreate makes creating an object with this name fail. If partial
	// is also set the object is created and returned alongside the error.
	failCreate map[string]bool
	partial    map[string]bool
	// lost makes a create fail after it committed, without returning it.
	lost map[string]bool
	// flipPlan makes PlanResource return a different known "label" on
	// every call, so the final plan at apply time is inconsistent.
	flipPlan bool
	plans    int
}

var mockSchema = provider.Schema{Resources: map[string]provider.ResourceSchema{
	"mock_thing": {Attributes: []provider.Attribute{
		{Name: "name", Type: tStr, Required: true},
		{Name: "value", Type: tStr, Optional: true},
		{Name: "zone", Type: tStr, Optional: true, ForceNew: true},
		{Name: "label", Type: tStr, Computed: true},
		{Name: "id", Type: tStr, Computed: true},
	}},
}}

func newMock() *mockProvider {
	return &mockProvider{objects: map[string]value.Value{}, byToken: map[string]string{}, failCreate: map[string]bool{},
		partial: map[string]bool{}, lost: map[string]bool{}}
}

func (m *mockProvider) Schema() provider.Schema                      { return mockSchema }
func (m *mockProvider) Configure(context.Context, value.Value) error { return nil }

func (m *mockProvider) PlanResource(_ context.Context, req provider.PlanRequest) (provider.PlanResponse, error) {
	resp := provider.PlanFromSchema(mockSchema.Resources[req.Type], req.Prior, req.Config)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plans++
	if m.flipPlan && req.Prior.IsNull() {
		resp.Planned = resp.Planned.With("label", value.String(fmt.Sprintf("v%d", m.plans)))
	}
	return resp, nil
}

func (m *mockProvider) ApplyResource(_ context.Context, req provider.ApplyRequest) (provider.ApplyResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case req.Planned.IsNull():
		id := provider.StringAttr(req.Prior, "id")
		m.ops = append(m.ops, "delete "+provider.StringAttr(req.Prior, "name"))
		delete(m.objects, id)
		return provider.ApplyResponse{New: value.Null}, nil
	case req.Prior.IsNull():
		name := provider.StringAttr(req.Planned, "name")
		m.ops = append(m.ops, "create "+name)
		if id, ok := m.byToken[req.CreateToken]; ok {
			if o, live := m.objects[id]; live {
				cp := o.AsMap()
				delete(cp, "token")
				return provider.ApplyResponse{New: value.Map(cp)}, nil // idempotent replay
			}
		}
		if m.failCreate[name] && !m.partial[name] {
			return provider.ApplyResponse{}, errors.New("create refused")
		}
		m.nextID++
		obj := nullUnknowns(req.Planned).With("id", value.String(fmt.Sprintf("m-%d", m.nextID)))
		if l, _ := req.Planned.Get("label"); !l.IsKnown() {
			obj = obj.With("label", value.String("auto"))
		}
		obj = obj.With("token", value.String(req.CreateToken))
		m.objects[provider.StringAttr(obj, "id")] = obj
		m.byToken[req.CreateToken] = provider.StringAttr(obj, "id")
		stored := obj.AsMap()
		delete(stored, "token")
		if m.lost[name] {
			return provider.ApplyResponse{}, errors.New("connection reset after commit")
		}
		if m.partial[name] {
			return provider.ApplyResponse{New: value.Map(stored)}, errors.New("created but not configured")
		}
		return provider.ApplyResponse{New: value.Map(stored)}, nil
	default:
		m.ops = append(m.ops, "update "+provider.StringAttr(req.Planned, "name"))
		m.objects[provider.StringAttr(req.Prior, "id")] = req.Planned
		return provider.ApplyResponse{New: req.Planned}, nil
	}
}

func (m *mockProvider) ReadResource(_ context.Context, req provider.ReadRequest) (provider.ReadResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.Pending {
		if o, ok := m.objects[m.byToken[req.CreateToken]]; ok {
			cp := o.AsMap()
			delete(cp, "token")
			return provider.ReadResponse{Current: value.Map(cp)}, nil
		}
		return provider.ReadResponse{Current: value.Null}, nil
	}
	o, ok := m.objects[provider.StringAttr(req.Prior, "id")]
	if !ok {
		return provider.ReadResponse{Current: value.Null}, nil
	}
	cp := o.AsMap()
	delete(cp, "token")
	return provider.ReadResponse{Current: value.Map(cp)}, nil
}

func mockEngine(t *testing.T, m *mockProvider, src string) *Engine {
	t.Helper()
	cfg, err := config.LoadFiles(t.TempDir(), map[string]string{"main.plait": src})
	if err != nil {
		t.Fatal(err)
	}
	vars, err := cfg.ResolveVariables(config.VarInputs{})
	if err != nil {
		t.Fatal(err)
	}
	e := New(cfg, vars, provider.Registry{"mock": func() provider.Provider { return m }})
	e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return e
}

func planApply(t *testing.T, e *Engine, st *state.State, opts PlanOptions) (*Plan, *ApplyResult) {
	t.Helper()
	p, err := e.Plan(context.Background(), st, opts)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Apply(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, res
}

const chain = `
resource "mock_thing" "a" { name = "a" }
resource "mock_thing" "b" {
  name  = "b"
  value = mock_thing.a.id
}
resource "mock_thing" "c" { name = "c" }
`

func TestApplyFailureSkipsDependentsOnly(t *testing.T) {
	m := newMock()
	m.failCreate["a"] = true
	e := mockEngine(t, m, chain)
	_, res := planApply(t, e, state.New(), PlanOptions{})
	if res.Added != 1 {
		t.Fatalf("added %d, want 1 (only c)", res.Added)
	}
	err := res.Err()
	if err == nil || !strings.Contains(err.Error(), "mock_thing.a (create): create refused") ||
		!strings.Contains(err.Error(), "mock_thing.b (create): skipped because mock_thing.a (create) failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	// a is left pending (outcome unknown); a refresh shows nothing was created.
	if si := res.State.Get(addr.Instance{Resource: addr.Resource{Type: "mock_thing", Name: "a"}}); si == nil || si.Current.Status != state.StatusPending {
		t.Fatalf("a should be pending, got %+v", si)
	}
	m.failCreate["a"] = false
	p, res2 := planApply(t, e, res.State, PlanOptions{})
	if len(p.Drift) != 1 || p.Drift[0].Kind != "abandoned" {
		t.Fatalf("drift: %+v", p.Drift)
	}
	if res2.Err() != nil || res2.Added != 2 {
		t.Fatalf("second apply: added %d err %v", res2.Added, res2.Err())
	}
}

func TestPartialCreateIsTaintedThenReplaced(t *testing.T) {
	m := newMock()
	m.failCreate["a"], m.partial["a"] = true, true
	e := mockEngine(t, m, `resource "mock_thing" "a" { name = "a" }`)
	_, res := planApply(t, e, state.New(), PlanOptions{})
	si := res.State.Instances["mock_thing.a"]
	if res.Err() == nil || si.Current.Status != state.StatusTainted {
		t.Fatalf("want tainted object, got %+v err %v", si.Current, res.Err())
	}
	m.failCreate["a"], m.partial["a"] = false, false
	p, res2 := planApply(t, e, res.State, PlanOptions{SkipRefresh: true})
	if p.Changes[0].Action != DeleteThenCreate || p.Changes[0].Reason != "it is tainted" {
		t.Fatalf("plan: %+v", p.Changes[0])
	}
	if res2.Err() != nil || res2.State.Instances["mock_thing.a"].Current.Status != state.StatusReady {
		t.Fatalf("replacement failed: %v", res2.Err())
	}
	if got := strings.Join(m.ops, ","); got != "create a,delete a,create a" {
		t.Fatalf("ops %s", got)
	}
}

func TestLostCreateIsRecoveredByToken(t *testing.T) {
	m := newMock()
	m.lost["a"] = true
	e := mockEngine(t, m, `resource "mock_thing" "a" { name = "a" }`)
	_, res := planApply(t, e, state.New(), PlanOptions{})
	if res.Err() == nil {
		t.Fatal("expected error")
	}
	m.lost["a"] = false
	p, err := e.Plan(context.Background(), res.State, PlanOptions{SkipRefresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Drift) != 1 || p.Drift[0].Kind != "recovered" || p.HasChanges() {
		t.Fatalf("expected recovery without changes, drift %+v changes %v", p.Drift, p.HasChanges())
	}
	if len(m.objects) != 1 {
		t.Fatalf("objects %d", len(m.objects))
	}
}

// TestAbandonedCreateReusesItsToken covers the race where an interrupted
// create is still in flight when the next run looks for it: the lookup
// misses, the late request then commits, and the re-create must not make a
// second object.
func TestAbandonedCreateReusesItsToken(t *testing.T) {
	m := newMock()
	m.failCreate["a"] = true // first attempt: nothing committed yet
	e := mockEngine(t, m, `resource "mock_thing" "a" { name = "a" }`)
	_, res := planApply(t, e, state.New(), PlanOptions{})
	token := res.State.Instances["mock_thing.a"].Current.CreateToken
	m.failCreate["a"] = false
	p, err := e.Plan(context.Background(), res.State, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Drift) != 1 || p.Drift[0].Kind != "abandoned" || p.Changes[0].CreateToken != token {
		t.Fatalf("token not carried over: drift %+v change %+v", p.Drift, p.Changes[0])
	}
	// The original request commits late, after the lookup missed it.
	m.objects["late"] = value.Map(map[string]value.Value{"id": value.String("late"), "name": value.String("a"),
		"value": value.Null, "zone": value.Null, "label": value.String("auto"), "token": value.String(token)})
	m.byToken[token] = "late"
	r2, err := e.Apply(context.Background(), p, nil)
	if err != nil || r2.Err() != nil {
		t.Fatal(err, r2.Err())
	}
	if len(m.objects) != 1 {
		t.Fatalf("duplicate object created: %d", len(m.objects))
	}
	if id, _ := r2.State.Instances["mock_thing.a"].Current.Attrs.Get("id"); id.AsString() != "late" {
		t.Fatalf("state should track the late object, got %s", id.GoString())
	}
}

func TestInconsistentFinalPlan(t *testing.T) {
	m := newMock()
	m.flipPlan = true
	e := mockEngine(t, m, `resource "mock_thing" "a" { name = "a" }`)
	_, res := planApply(t, e, state.New(), PlanOptions{})
	if err := res.Err(); err == nil || !strings.Contains(err.Error(), "inconsistent final plan") {
		t.Fatalf("want inconsistent final plan, got %v", err)
	}
	if len(m.ops) != 0 {
		t.Fatalf("provider must not be called, ops %v", m.ops)
	}
}

func TestPlanErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"unknown count", `
resource "mock_thing" "a" { name = "a" }
resource "mock_thing" "b" {
  count = length(mock_thing.a.id)
  name  = "b"
}`, "only known after apply"},
		{"negative count", `resource "mock_thing" "a" {
  count = -1
  name  = "a"
}`, "non-negative whole number"},
		{"for_each wrong type", `resource "mock_thing" "a" {
  for_each = 3
  name     = "a"
}`, "must be a map or a set of strings"},
		{"for_each list of numbers", `resource "mock_thing" "a" {
  for_each = [1]
  name     = "a"
}`, "requires known strings"},
		{"unknown provider", `resource "nope_thing" "a" { name = "a" }`, `no provider named "nope"`},
		{"unknown type", `resource "mock_other" "a" { name = "a" }`, `has no resource type "mock_other"`},
		{"bad argument", `resource "mock_thing" "a" {
  name  = "a"
  bogus = 1
}`, `unsupported argument "bogus"`},
		{"missing required", `resource "mock_thing" "a" { value = "x" }`, `missing required argument "name"`},
		{"computed set", `resource "mock_thing" "a" {
  name = "a"
  id   = "x"
}`, `"id" is computed`},
		{"ignore unknown attr", `resource "mock_thing" "a" {
  name = "a"
  lifecycle { ignore_changes = [nothing] }
}`, `unknown attribute "nothing"`},
		{"eval error", `resource "mock_thing" "a" { name = 1 / 0 }`, "division by zero"},
		{"output error", `
resource "mock_thing" "a" { name = "a" }
output "o" { value = mock_thing.a.nope }`, `no attribute "nope"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mockEngine(t, newMock(), tc.src)
			_, err := e.Plan(context.Background(), state.New(), PlanOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestPreventDestroy(t *testing.T) {
	m := newMock()
	src := `resource "mock_thing" "a" {
  name = "a"
  zone = "z1"
  lifecycle { prevent_destroy = true }
}`
	e := mockEngine(t, m, src)
	_, res := planApply(t, e, state.New(), PlanOptions{})
	if res.Err() != nil {
		t.Fatal(res.Err())
	}
	if _, err := e.Plan(context.Background(), res.State, PlanOptions{Destroy: true}); err == nil || !strings.Contains(err.Error(), "prevent_destroy") {
		t.Fatalf("destroy: %v", err)
	}
	e2 := mockEngine(t, m, strings.Replace(src, `"z1"`, `"z2"`, 1))
	if _, err := e2.Plan(context.Background(), res.State, PlanOptions{}); err == nil || !strings.Contains(err.Error(), "prevent_destroy") {
		t.Fatalf("replace: %v", err)
	}
	e3 := mockEngine(t, m, `resource "mock_thing" "a" {
  count = 0
  name  = "a"
  lifecycle { prevent_destroy = true }
}`)
	st := res.State.Clone()
	si := st.Instances["mock_thing.a"]
	st.Remove(si.Addr)
	si.Addr.Key = addr.IntK(0)
	st.Set(si)
	if _, err := e3.Plan(context.Background(), st, PlanOptions{SkipRefresh: true}); err == nil || !strings.Contains(err.Error(), "prevent_destroy") {
		t.Fatalf("orphan: %v", err)
	}
}

func TestMixedCreateBeforeDestroyIsRejected(t *testing.T) {
	m := newMock()
	src := `
resource "mock_thing" "a" {
  name = "a"
  zone = "%s"
}
resource "mock_thing" "b" {
  name = "b"
  zone = mock_thing.a.id
  lifecycle { create_before_destroy = true }
}`
	e := mockEngine(t, m, fmt.Sprintf(src, "z1"))
	_, res := planApply(t, e, state.New(), PlanOptions{})
	if res.Err() != nil {
		t.Fatal(res.Err())
	}
	e2 := mockEngine(t, m, fmt.Sprintf(src, "z2"))
	p, err := e2.Plan(context.Background(), res.State, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Apply(context.Background(), p, nil); err == nil || !strings.Contains(err.Error(), "create_before_destroy") {
		t.Fatalf("want ordering error, got %v", err)
	}
}

func TestApplyRefusesChangedConfigAndPersistFailures(t *testing.T) {
	m := newMock()
	e := mockEngine(t, m, `resource "mock_thing" "a" { name = "a" }`)
	p, err := e.Plan(context.Background(), state.New(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other := mockEngine(t, m, `resource "mock_thing" "a" { name = "b" }`)
	if _, err := other.Apply(context.Background(), p, nil); err == nil || !strings.Contains(err.Error(), "configuration has changed") {
		t.Fatalf("digest check: %v", err)
	}
	boom := errors.New("disk full")
	res, err := e.Apply(context.Background(), p, func(*state.State) error { return boom })
	if err == nil && (res == nil || res.Err() == nil) {
		t.Fatal("persist failure must surface")
	}
	if len(m.ops) != 0 {
		t.Fatalf("no create may be sent before its pending record is persisted; ops %v", m.ops)
	}
}

func TestUpdateAndOutputs(t *testing.T) {
	m := newMock()
	src := `
variable "v" { default = "one" }
resource "mock_thing" "a" {
  name  = "a"
  value = var.v
}
output "val" { value = mock_thing.a.value }
output "gone" { value = "x" }
`
	e := mockEngine(t, m, src)
	_, res := planApply(t, e, state.New(), PlanOptions{})
	if res.State.Outputs["val"].AsString() != "one" {
		t.Fatalf("outputs %v", res.State.Outputs)
	}
	e2 := mockEngine(t, m, strings.Replace(strings.Replace(src, `"one"`, `"two"`, 1), `output "gone" { value = "x" }`, "", 1))
	p, res2 := planApply(t, e2, res.State, PlanOptions{})
	if c := p.Changes[0]; c.Action != Update {
		t.Fatalf("action %s", c.Action)
	}
	if res2.Changed != 1 || res2.State.Outputs["val"].AsString() != "two" {
		t.Fatalf("changed %d outputs %v", res2.Changed, res2.State.Outputs)
	}
	if _, ok := res2.State.Outputs["gone"]; ok {
		t.Fatal("removed output still present")
	}
	var sb strings.Builder
	if err := Render(&sb, p, RenderOptions{ShowUnchanged: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`~ value = "one" -> "two"`, `  name  = "a"`, `- gone = "x" -> null`, `~ val  = "one" -> "two"`} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("render missing %q:\n%s", want, sb.String())
		}
	}
}

func TestDestroyPlanAndDeposedCleanup(t *testing.T) {
	m := newMock()
	e := mockEngine(t, m, chain)
	_, res := planApply(t, e, state.New(), PlanOptions{})
	p, res2 := planApply(t, e, res.State, PlanOptions{Destroy: true})
	if res2.Err() != nil || res2.Destroyed != 3 || !res2.State.Empty() {
		t.Fatalf("destroy: %v %d", res2.Err(), res2.Destroyed)
	}
	// b depends on a, so b must be deleted first.
	ops := strings.Join(m.ops[3:], ",")
	if strings.Index(ops, "delete b") > strings.Index(ops, "delete a") {
		t.Fatalf("delete order wrong: %s", ops)
	}
	if !p.Destroy || len(p.Outputs) != 0 {
		t.Fatalf("plan %+v", p)
	}
}

func TestConsistentAndNullUnknowns(t *testing.T) {
	planned := value.Map(map[string]value.Value{"a": value.String("x"), "b": value.Unknown,
		"c": value.List(value.Unknown)})
	if err := consistent(planned, value.Map(map[string]value.Value{"a": value.String("x"), "b": value.String("y")})); err != nil {
		t.Fatal(err)
	}
	if err := consistent(planned, value.Map(map[string]value.Value{"a": value.String("z")})); err == nil {
		t.Fatal("expected mismatch")
	}
	n := nullUnknowns(planned)
	if !n.IsWhollyKnown() {
		t.Fatal("unknowns remain")
	}
	if c, _ := n.Get("c"); c.GoString() != "[null]" {
		t.Fatalf("nested: %s", c.GoString())
	}
}

func TestEachValueAndAggregates(t *testing.T) {
	m := newMock()
	src := `
resource "mock_thing" "set" {
  for_each = toset(["x", "y", "x"])
  name     = "s-${each.key}"
  value    = each.value
}
resource "mock_thing" "summary" {
  name  = "summary"
  value = join(",", [for k, v in mock_thing.set : "${k}=${v.id}"])
}
output "ids" { value = { for k, v in mock_thing.set : k => v.id } }
`
	e := mockEngine(t, m, src)
	_, res := planApply(t, e, state.New(), PlanOptions{})
	if res.Err() != nil || res.Added != 3 {
		t.Fatalf("apply: %v added %d", res.Err(), res.Added)
	}
	sum := res.State.Instances["mock_thing.summary"].Current.Attrs
	v, _ := sum.Get("value")
	if !strings.HasPrefix(v.AsString(), "x=m-") || !strings.Contains(v.AsString(), ",y=m-") {
		t.Fatalf("summary value %s", v.GoString())
	}
	if ids := res.State.Outputs["ids"]; ids.Len() != 2 {
		t.Fatalf("ids output %s", ids.GoString())
	}
}
