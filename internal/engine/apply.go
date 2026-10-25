package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/archit2501/plait/internal/addr"
	"github.com/archit2501/plait/internal/dag"
	"github.com/archit2501/plait/internal/eval"
	"github.com/archit2501/plait/internal/provider"
	"github.com/archit2501/plait/internal/state"
	"github.com/archit2501/plait/internal/value"
)

// Persister durably records the working state. Apply calls it after every
// state mutation, including just before each create is sent (the object is
// recorded as pending with its idempotency token), so a crash at any point
// leaves a state that refresh can reconcile.
type Persister func(*state.State) error

// ApplyResult summarizes an apply.
type ApplyResult struct {
	Added, Changed, Destroyed int
	// Failures maps a step name ("ADDR (create)") to its error. Steps
	// skipped because a dependency failed are included.
	Failures map[string]error
	State    *state.State
}

// Err returns the combined failures, or nil.
func (r *ApplyResult) Err() error {
	if len(r.Failures) == 0 {
		return nil
	}
	return joinNodeErrors(r.Failures)
}

func joinNodeErrors(m map[string]error) error {
	keys := sortedKeys(m)
	errs := make([]error, 0, len(keys))
	for _, k := range keys {
		var se *dag.SkipError
		if errors.As(m[k], &se) {
			errs = append(errs, fmt.Errorf("%s: skipped because %s failed", k, se.Dep))
			continue
		}
		errs = append(errs, fmt.Errorf("%s: %w", k, m[k]))
	}
	return errors.Join(errs...)
}

// step kinds in the apply graph.
type stepKind uint8

const (
	stepApply          stepKind = iota // create or update the current object
	stepDestroy                        // delete the current object
	stepDestroyDeposed                 // delete a deposed object from an earlier run
	stepDestroyReplaced                // delete the object deposed by create_before_destroy
)

type step struct {
	kind   stepKind
	change *ResourceChange
}

func (s step) name() string {
	a := s.change.Addr.String()
	switch s.kind {
	case stepApply:
		if s.change.Action == Update {
			return a + " (update)"
		}
		return a + " (create)"
	case stepDestroy:
		return a + " (destroy)"
	case stepDestroyDeposed:
		return a + " (destroy deposed)"
	}
	return a + " (destroy replaced)"
}

// applyGraph builds the instance-level step graph for a plan.
//
// Ordering rules (A depends on D):
//   - create/update A after create/update D;
//   - delete D after delete A (dependents go first);
//   - delete-then-create: create A after deleting the old A;
//   - create_before_destroy: delete the old A after creating the new A and
//     after every dependent of A has been updated to point at it.
func applyGraph(p *Plan) (*dag.Graph, map[string]step, error) {
	g := dag.New()
	steps := map[string]step{}
	add := func(s step) string {
		n := s.name()
		steps[n] = s
		g.Add(n)
		return n
	}
	applyNode := map[addr.Instance]string{}
	destroyNodes := map[addr.Instance][]string{}
	replacedNode := map[addr.Instance]string{}
	deposedNode := map[addr.Instance]string{}
	for _, c := range p.Changes {
		switch {
		case c.Action == NoOp:
		case c.Deposed:
			n := add(step{stepDestroyDeposed, c})
			deposedNode[c.Addr] = n
			destroyNodes[c.Addr] = append(destroyNodes[c.Addr], n)
		case c.Action == Delete:
			destroyNodes[c.Addr] = append(destroyNodes[c.Addr], add(step{stepDestroy, c}))
		case c.Action == Create || c.Action == Update:
			applyNode[c.Addr] = add(step{stepApply, c})
		case c.Action == DeleteThenCreate:
			d := add(step{stepDestroy, c})
			destroyNodes[c.Addr] = append(destroyNodes[c.Addr], d)
			a := add(step{stepApply, c})
			applyNode[c.Addr] = a
			g.AddEdge(a, d)
		case c.Action == CreateThenDelete:
			a := add(step{stepApply, c})
			applyNode[c.Addr] = a
			d := add(step{stepDestroyReplaced, c})
			replacedNode[c.Addr] = d
			destroyNodes[c.Addr] = append(destroyNodes[c.Addr], d)
			g.AddEdge(d, a)
		}
	}
	dependsOn := func(c *ResourceChange, ra addr.Resource) bool {
		return c.Addr.Resource != ra && slices.Contains(c.Dependencies, ra.String())
	}
	for _, c := range p.Changes {
		if a, ok := applyNode[c.Addr]; ok && !c.Deposed {
			for _, d := range p.Changes {
				if da, ok := applyNode[d.Addr]; ok && !d.Deposed && dependsOn(c, d.Addr.Resource) {
					g.AddEdge(a, da)
				}
			}
			if dn, ok := deposedNode[c.Addr]; ok && c.Action == CreateThenDelete {
				g.AddEdge(a, dn) // clear the old deposed slot before deposing again
			}
		}
		if rn, ok := replacedNode[c.Addr]; ok && !c.Deposed {
			for _, x := range p.Changes {
				if xa, ok := applyNode[x.Addr]; ok && !x.Deposed && dependsOn(x, c.Addr.Resource) {
					g.AddEdge(rn, xa)
				}
			}
		}
	}
	for _, c := range p.Changes {
		for _, dn := range destroyNodes[c.Addr] {
			for _, y := range p.Changes {
				if !dependsOn(y, c.Addr.Resource) {
					continue
				}
				for _, yn := range destroyNodes[y.Addr] {
					g.AddEdge(dn, yn)
				}
			}
		}
	}
	if err := g.Validate(); err != nil {
		return nil, nil, fmt.Errorf("the plan cannot be ordered (%w); when replacing an object with "+
			"create_before_destroy, set it on the objects it depends on as well", err)
	}
	return g, steps, nil
}

// applier holds the mutable working state of one apply.
type applier struct {
	e       *Engine
	plan    *Plan
	persist Persister

	mu     sync.Mutex
	ws     *state.State
	counts struct{ added, changed, destroyed int }
}

// Apply executes a plan. It walks the step graph with bounded parallelism;
// when a step fails, its dependents are skipped while unrelated steps
// continue. The returned result is non-nil whenever the walk ran, even if
// some steps failed; a non-nil error means apply could not start or could
// not persist state.
func (e *Engine) Apply(ctx context.Context, plan *Plan, persist Persister) (*ApplyResult, error) {
	if plan.ConfigDigest != e.Config.Digest {
		return nil, errors.New("the configuration has changed since this plan was created; run plan again")
	}
	g, steps, err := applyGraph(plan)
	if err != nil {
		return nil, err
	}
	if persist == nil {
		persist = func(*state.State) error { return nil }
	}
	a := &applier{e: e, plan: plan, persist: persist, ws: plan.PriorState.Clone()}
	e.log().Debug("apply starting", "steps", g.Len(), "parallelism", e.parallelism())
	failures, err := g.Walk(ctx, e.parallelism(), func(ctx context.Context, n string) error {
		return a.run(ctx, steps[n])
	})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updateOutputs()
	if err := a.persist(a.ws); err != nil {
		return nil, fmt.Errorf("persisting final state: %w", err)
	}
	return &ApplyResult{Added: a.counts.added, Changed: a.counts.changed, Destroyed: a.counts.destroyed,
		Failures: failures, State: a.ws.Clone()}, nil
}

func (a *applier) run(ctx context.Context, s step) error {
	switch s.kind {
	case stepApply:
		if s.change.Action == Update {
			return a.update(ctx, s.change)
		}
		return a.create(ctx, s.change)
	case stepDestroy:
		return a.destroy(ctx, s.change, false)
	default:
		return a.destroy(ctx, s.change, true)
	}
}

// resourceValue reads the aggregate value of a resource from the working
// state. Instances that are not (yet) ready are unknown.
func (a *applier) resourceValue(ra addr.Resource) (value.Value, error) {
	r := a.e.Config.Resources[ra.String()]
	keys, ok := a.plan.Expanded[ra.String()]
	if r == nil || !ok {
		return value.Null, fmt.Errorf("internal error: no expansion recorded for %s", ra)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return aggregate(r, keys, func(k addr.Key) value.Value {
		si := a.ws.Get(addr.Instance{Resource: ra, Key: k})
		if si == nil || si.Current == nil || si.Current.Status != state.StatusReady {
			return value.Unknown
		}
		return si.Current.Attrs
	}), nil
}

// finalPlan re-evaluates the instance configuration now that every
// dependency has been applied, asks the provider to plan again, and checks
// the result against the reviewed plan.
func (a *applier) finalPlan(ctx context.Context, c *ResourceChange, prior value.Value) (provider.Provider, value.Value, error) {
	r := a.e.Config.Resources[c.Addr.Resource.String()]
	if r == nil {
		return nil, value.Null, fmt.Errorf("resource %s is not in the configuration", c.Addr.Resource)
	}
	p, rs, err := a.e.resourceSchema(ctx, r.Addr)
	if err != nil {
		return nil, value.Null, err
	}
	ev := newEvaluator(a.e, a.resourceValue)
	cfg, err := a.e.finalConfig(ev, rs, r, instance{Key: c.Addr.Key, Each: c.Each}, c.Before)
	if err != nil {
		return nil, value.Null, err
	}
	if !cfg.IsWhollyKnown() {
		return nil, value.Null, errors.New("configuration still contains unknown values at apply time")
	}
	resp, err := p.PlanResource(ctx, provider.PlanRequest{Type: r.Addr.Type, Prior: prior, Config: cfg})
	if err != nil {
		return nil, value.Null, err
	}
	if !prior.IsNull() && len(resp.RequiresReplace) > 0 {
		return nil, value.Null, fmt.Errorf("provider produced an inconsistent final plan: %s now requires replacement",
			strings.Join(resp.RequiresReplace, ", "))
	}
	if err := consistent(c.After, resp.Planned); err != nil {
		return nil, value.Null, fmt.Errorf("provider produced an inconsistent final plan: %w", err)
	}
	return p, resp.Planned, nil
}

// consistent checks that every value known in the reviewed plan is
// unchanged in the final plan (or result).
func consistent(planned, final value.Value) error {
	for _, k := range planned.Keys() {
		pv, _ := planned.Get(k)
		if !pv.IsWhollyKnown() {
			continue
		}
		fv, _ := final.Get(k)
		if !fv.IsKnown() {
			continue
		}
		if !pv.Equal(fv) {
			return fmt.Errorf("attribute %q was planned as %s but is now %s", k, pv.GoString(), fv.GoString())
		}
	}
	return nil
}

func (a *applier) create(ctx context.Context, c *ResourceChange) error {
	p, planned, err := a.finalPlan(ctx, c, value.Null)
	if err != nil {
		return err
	}
	token := state.NewToken()
	a.mu.Lock()
	si := a.ws.Get(c.Addr)
	if si == nil {
		si = &state.Instance{Addr: c.Addr, Provider: c.Provider}
	}
	if c.Action == CreateThenDelete && si.Current != nil {
		si.Deposed = si.Current
	}
	si.Current = &state.Object{Status: state.StatusPending, Attrs: nullUnknowns(planned), CreateToken: token}
	si.Dependencies = c.Dependencies
	a.ws.Set(si)
	err = a.persist(a.ws)
	a.mu.Unlock()
	if err != nil {
		return fmt.Errorf("recording pending create: %w", err)
	}
	ev := Event{Addr: c.Addr, Op: OpCreate}
	a.e.Hooks.pre(ev)
	start := time.Now()
	resp, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: c.Addr.Type, Prior: value.Null, Planned: planned, CreateToken: token})
	if err == nil && (resp.New.IsNull() || !resp.New.IsWhollyKnown()) {
		err = errors.New("provider returned an incomplete object after create")
		resp.New = value.Null
	}
	a.e.Hooks.post(ev, resp.New, err, start)
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case err != nil && !resp.New.IsNull():
		si.Current = &state.Object{Status: state.StatusTainted, Attrs: resp.New}
	case err != nil:
		// The outcome is unknown (the request may have committed); the
		// pending object stays so the next refresh can find it by token.
	default:
		si.Current = &state.Object{Status: state.StatusReady, Attrs: resp.New}
		a.counts.added++
	}
	if perr := a.persist(a.ws); perr != nil {
		return errors.Join(err, fmt.Errorf("persisting state: %w", perr))
	}
	if err != nil {
		return err
	}
	if cerr := consistent(planned, resp.New); cerr != nil {
		return fmt.Errorf("provider produced an unexpected new value: %w", cerr)
	}
	return nil
}

func (a *applier) update(ctx context.Context, c *ResourceChange) error {
	a.mu.Lock()
	si := a.ws.Get(c.Addr)
	a.mu.Unlock()
	if si == nil || si.Current == nil {
		return errors.New("object to update is missing from state")
	}
	prior := si.Current.Attrs
	p, planned, err := a.finalPlan(ctx, c, prior)
	if err != nil {
		return err
	}
	ev := Event{Addr: c.Addr, Op: OpUpdate}
	a.e.Hooks.pre(ev)
	start := time.Now()
	resp, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: c.Addr.Type, Prior: prior, Planned: planned})
	if err == nil && (resp.New.IsNull() || !resp.New.IsWhollyKnown()) {
		err = errors.New("provider returned an incomplete object after update")
		resp.New = value.Null
	}
	a.e.Hooks.post(ev, resp.New, err, start)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !resp.New.IsNull() && resp.New.IsWhollyKnown() {
		si.Current = &state.Object{Status: si.Current.Status, Attrs: resp.New}
	}
	si.Dependencies = c.Dependencies
	if err == nil {
		a.counts.changed++
	}
	if perr := a.persist(a.ws); perr != nil {
		return errors.Join(err, fmt.Errorf("persisting state: %w", perr))
	}
	if err != nil {
		return err
	}
	if cerr := consistent(planned, resp.New); cerr != nil {
		return fmt.Errorf("provider produced an unexpected new value: %w", cerr)
	}
	return nil
}

func (a *applier) destroy(ctx context.Context, c *ResourceChange, deposed bool) error {
	a.mu.Lock()
	si := a.ws.Get(c.Addr)
	var obj *state.Object
	if si != nil {
		obj = si.Current
		if deposed {
			obj = si.Deposed
		}
	}
	a.mu.Unlock()
	if obj == nil {
		return nil // already gone (e.g. removed by an earlier, interrupted run)
	}
	p, err := a.e.Provider(ctx, si.Provider)
	if err != nil {
		return err
	}
	ev := Event{Addr: c.Addr, Op: OpDestroy, Deposed: deposed}
	a.e.Hooks.pre(ev)
	start := time.Now()
	if obj.Status == state.StatusPending {
		err = errors.New("object has an unresolved pending create; run refresh first")
	} else {
		_, err = p.ApplyResource(ctx, provider.ApplyRequest{Type: c.Addr.Type, Prior: obj.Attrs, Planned: value.Null})
	}
	a.e.Hooks.post(ev, value.Null, err, start)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if deposed {
		si.Deposed = nil
	} else {
		si.Current = nil
	}
	if si.Current == nil && si.Deposed == nil {
		a.ws.Remove(c.Addr)
	}
	a.counts.destroyed++
	if err := a.persist(a.ws); err != nil {
		return fmt.Errorf("persisting state: %w", err)
	}
	return nil
}

// updateOutputs recomputes output values from the working state. Outputs
// that cannot be evaluated (because something they reference failed) keep
// their previous value. Caller holds a.mu.
func (a *applier) updateOutputs() {
	if a.plan.Destroy {
		a.ws.Outputs = map[string]value.Value{}
		return
	}
	// resourceValue takes the lock itself, so evaluate against a snapshot.
	snap := a.ws.Clone()
	rv := func(ra addr.Resource) (value.Value, error) {
		r := a.e.Config.Resources[ra.String()]
		keys := a.plan.Expanded[ra.String()]
		return aggregate(r, keys, func(k addr.Key) value.Value {
			si := snap.Get(addr.Instance{Resource: ra, Key: k})
			if si == nil || si.Current == nil || si.Current.Status != state.StatusReady {
				return value.Unknown
			}
			return si.Current.Attrs
		}), nil
	}
	ev := newEvaluator(a.e, rv)
	out := map[string]value.Value{}
	for _, name := range sortedKeys(a.e.Config.Outputs) {
		o := a.e.Config.Outputs[name]
		ctx, err := ev.context(nil, o.Expr)
		var v value.Value
		if err == nil {
			v, err = eval.Eval(o.Expr, ctx)
		}
		if err != nil || !v.IsWhollyKnown() {
			if prev, ok := a.ws.Outputs[name]; ok {
				out[name] = prev
			}
			a.e.log().Warn("output not updated", "output", name, "err", err)
			continue
		}
		out[name] = v
	}
	a.ws.Outputs = out
}

// sortedSteps is used by tests and the graph command to list steps.
func sortedSteps(steps map[string]step) []string {
	out := make([]string, 0, len(steps))
	for n := range steps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ApplyGraph returns the step graph a plan would execute, in DOT-friendly
// form: step names and edges ("from" depends on "to").
func ApplyGraph(p *Plan) (steps []string, edges [][2]string, err error) {
	g, st, err := applyGraph(p)
	if err != nil {
		return nil, nil, err
	}
	return sortedSteps(st), g.Edges(), nil
}
