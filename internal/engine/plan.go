package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/archit2501/plait/internal/addr"
	"github.com/archit2501/plait/internal/config"
	"github.com/archit2501/plait/internal/dag"
	"github.com/archit2501/plait/internal/eval"
	"github.com/archit2501/plait/internal/provider"
	"github.com/archit2501/plait/internal/state"
	"github.com/archit2501/plait/internal/value"
)

// Action classifies a planned change.
type Action string

// Change actions.
const (
	NoOp             Action = "no-op"
	Create           Action = "create"
	Update           Action = "update"
	DeleteThenCreate Action = "delete-then-create"
	CreateThenDelete Action = "create-then-delete"
	Delete           Action = "delete"
)

// IsReplace reports whether a is one of the two replace actions.
func (a Action) IsReplace() bool { return a == DeleteThenCreate || a == CreateThenDelete }

// ResourceChange is the planned change for one resource instance object.
type ResourceChange struct {
	Addr addr.Instance `json:"-"`
	// Deposed marks a change to the deposed object left behind by an
	// interrupted create_before_destroy replacement.
	Deposed bool   `json:"deposed,omitempty"`
	Action  Action `json:"action"`
	// Reason explains deletes and replacements that are not caused by an
	// attribute change.
	Reason string      `json:"reason,omitempty"`
	Before value.Value `json:"before"`
	After  value.Value `json:"after"`
	// RequiresReplace lists the attributes that force replacement.
	RequiresReplace []string `json:"requires_replace,omitempty"`
	// Each is each.value for for_each instances; apply re-evaluates the
	// configuration with it.
	Each value.Value `json:"each,omitempty"`
	// Dependencies are the resource addresses this instance depends on.
	Dependencies []string `json:"dependencies,omitempty"`
	// Provider is the provider name managing the object.
	Provider string `json:"provider"`
}

// OutputChange is the planned change to one output value.
type OutputChange struct {
	Name   string      `json:"name"`
	Action Action      `json:"action"`
	Before value.Value `json:"before"`
	After  value.Value `json:"after"`
}

// Drift is a difference between the prior state and the refreshed state.
type Drift struct {
	Addr    addr.Instance
	Deposed bool
	Kind    string // "deleted", "changed", "recovered", "abandoned"
	Before  value.Value
	After   value.Value
}

// Plan is the full set of changes needed to converge.
type Plan struct {
	Destroy bool
	// ConfigDigest identifies the configuration the plan was made from.
	ConfigDigest string
	Variables    map[string]value.Value
	// PriorState is the refreshed state the plan was computed against.
	// Apply starts from it.
	PriorState *state.State
	Changes    []*ResourceChange
	Outputs    []*OutputChange
	Drift      []Drift
	// Expanded records, per resource address, the instance keys the
	// configuration expands to, so apply can rebuild aggregate values.
	Expanded map[string][]addr.Key
}

// HasChanges reports whether applying the plan would change anything.
func (p *Plan) HasChanges() bool {
	for _, c := range p.Changes {
		if c.Action != NoOp {
			return true
		}
	}
	for _, o := range p.Outputs {
		if o.Action != NoOp {
			return true
		}
	}
	return false
}

// Counts returns the number of objects to add, change and destroy.
func (p *Plan) Counts() (add, change, destroy int) {
	for _, c := range p.Changes {
		switch c.Action {
		case Create:
			add++
		case Update:
			change++
		case Delete:
			destroy++
		case DeleteThenCreate, CreateThenDelete:
			add++
			destroy++
		}
	}
	return add, change, destroy
}

// Change returns the change for an instance's current object, or nil.
func (p *Plan) Change(a addr.Instance) *ResourceChange {
	for _, c := range p.Changes {
		if c.Addr == a && !c.Deposed {
			return c
		}
	}
	return nil
}

// PlanOptions controls planning.
type PlanOptions struct {
	// Destroy plans the deletion of every tracked object.
	Destroy bool
	// SkipRefresh plans against the stored state without asking providers
	// for the real state. Interrupted creates are still resolved.
	SkipRefresh bool
}

// Plan computes the changes needed to make prior match the configuration.
// prior is not modified.
func (e *Engine) Plan(ctx context.Context, prior *state.State, opts PlanOptions) (*Plan, error) {
	st := prior.Clone()
	drift, err := e.refresh(ctx, st, !opts.SkipRefresh)
	if err != nil {
		return nil, err
	}
	p := &Plan{Destroy: opts.Destroy, ConfigDigest: e.Config.Digest, Variables: e.Vars,
		PriorState: st, Drift: drift, Expanded: map[string][]addr.Key{}}
	if opts.Destroy {
		if err := e.planDestroy(p, st); err != nil {
			return nil, err
		}
	} else if err := e.planConverge(ctx, p, st); err != nil {
		return nil, err
	}
	sort.SliceStable(p.Changes, func(i, j int) bool {
		a, b := p.Changes[i], p.Changes[j]
		if a.Addr != b.Addr {
			return addr.Less(a.Addr, b.Addr)
		}
		return !a.Deposed && b.Deposed
	})
	e.log().Debug("plan complete", "changes", len(p.Changes), "destroy", opts.Destroy)
	return p, nil
}

func (e *Engine) planDestroy(p *Plan, st *state.State) error {
	for _, inst := range st.Sorted() {
		r := e.Config.Resources[inst.Addr.Resource.String()]
		for _, obj := range []struct {
			o       *state.Object
			deposed bool
		}{{inst.Current, false}, {inst.Deposed, true}} {
			if obj.o == nil {
				continue
			}
			if r != nil && r.Lifecycle.PreventDestroy && !obj.deposed {
				return fmt.Errorf("%s has lifecycle.prevent_destroy set and cannot be destroyed", inst.Addr)
			}
			p.Changes = append(p.Changes, &ResourceChange{Addr: inst.Addr, Deposed: obj.deposed, Action: Delete,
				Before: obj.o.Attrs, After: value.Null,
				Dependencies: inst.Dependencies, Provider: inst.Provider})
		}
	}
	for _, name := range sortedKeys(st.Outputs) {
		p.Outputs = append(p.Outputs, &OutputChange{Name: name, Action: Delete, Before: st.Outputs[name], After: value.Null})
	}
	return nil
}

func (e *Engine) planConverge(ctx context.Context, p *Plan, st *state.State) error {
	g, err := e.Config.Graph()
	if err != nil {
		return err
	}
	order, err := g.TopoSort()
	if err != nil {
		return err
	}
	planned := map[string]value.Value{} // instance address -> planned object
	ev := newEvaluator(e, func(ra addr.Resource) (value.Value, error) {
		r := e.Config.Resources[ra.String()]
		keys, ok := p.Expanded[ra.String()]
		if !ok {
			return value.Null, fmt.Errorf("internal error: %s referenced before it was planned", ra)
		}
		return aggregate(r, keys, func(k addr.Key) value.Value {
			return planned[addr.Instance{Resource: ra, Key: k}.String()]
		}), nil
	})
	inConfig := map[string]bool{}
	for _, node := range order {
		r, ok := e.Config.Resources[node]
		if !ok {
			continue // variables, locals, providers and outputs are evaluated on demand
		}
		insts, err := ev.expand(r)
		if err != nil {
			return err
		}
		deps, err := e.resourceDeps(r)
		if err != nil {
			return err
		}
		keys := make([]addr.Key, len(insts))
		for i, inst := range insts {
			keys[i] = inst.Key
			ia := addr.Instance{Resource: r.Addr, Key: inst.Key}
			inConfig[ia.String()] = true
			ch, err := e.planInstance(ctx, ev, r, inst, st.Get(ia))
			if err != nil {
				return fmt.Errorf("%s: %w", ia, err)
			}
			ch.Dependencies = deps
			p.Changes = append(p.Changes, ch)
			if ch.Action == NoOp {
				planned[ia.String()] = ch.Before
			} else {
				planned[ia.String()] = ch.After
			}
		}
		p.Expanded[r.Addr.String()] = keys
	}
	// Anything tracked but no longer configured is deleted, as is every
	// deposed object.
	for _, inst := range st.Sorted() {
		r := e.Config.Resources[inst.Addr.Resource.String()]
		if inst.Current != nil && !inConfig[inst.Addr.String()] {
			reason := "it is no longer in the configuration"
			if r != nil {
				reason = "its key is no longer produced by count/for_each"
				if r.Lifecycle.PreventDestroy {
					return fmt.Errorf("%s has lifecycle.prevent_destroy set but %s", inst.Addr, reason)
				}
			}
			p.Changes = append(p.Changes, &ResourceChange{Addr: inst.Addr, Action: Delete, Reason: reason,
				Before: inst.Current.Attrs, After: value.Null, Dependencies: inst.Dependencies, Provider: inst.Provider})
		}
		if inst.Deposed != nil {
			p.Changes = append(p.Changes, &ResourceChange{Addr: inst.Addr, Deposed: true, Action: Delete,
				Reason: "it is a deposed object left by an interrupted replacement",
				Before: inst.Deposed.Attrs, After: value.Null, Dependencies: inst.Dependencies, Provider: inst.Provider})
		}
	}
	return e.planOutputs(p, st, ev)
}

func (e *Engine) planOutputs(p *Plan, st *state.State, ev *evaluator) error {
	names := make([]string, 0, len(e.Config.Outputs))
	for n := range e.Config.Outputs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		o := e.Config.Outputs[n]
		ctx, err := ev.context(nil, o.Expr)
		if err != nil {
			return err
		}
		v, err := eval.Eval(o.Expr, ctx)
		if err != nil {
			return fmt.Errorf("output %q: %w", n, err)
		}
		before, had := st.Outputs[n]
		oc := &OutputChange{Name: n, Before: value.Null, After: v}
		switch {
		case !had:
			oc.Action = Create
		case before.Equal(v):
			oc.Action, oc.Before = NoOp, before
		default:
			oc.Action, oc.Before = Update, before
		}
		p.Outputs = append(p.Outputs, oc)
	}
	for _, n := range sortedKeys(st.Outputs) {
		if _, ok := e.Config.Outputs[n]; !ok {
			p.Outputs = append(p.Outputs, &OutputChange{Name: n, Action: Delete, Before: st.Outputs[n], After: value.Null})
		}
	}
	return nil
}

// finalConfig evaluates and validates an instance's configuration and
// applies ignore_changes against prior.
func (e *Engine) finalConfig(ev *evaluator, rs provider.ResourceSchema, r *config.Resource, inst instance, prior value.Value) (value.Value, error) {
	raw, err := ev.instanceConfig(r, inst)
	if err != nil {
		return value.Null, err
	}
	ia := addr.Instance{Resource: r.Addr, Key: inst.Key}
	cfg, err := provider.ValidateConfig(rs, ia.String(), raw)
	if err != nil {
		return value.Null, err
	}
	for _, name := range r.Lifecycle.IgnoreChanges {
		if _, ok := rs.Attr(name); !ok {
			return value.Null, fmt.Errorf("lifecycle.ignore_changes names unknown attribute %q", name)
		}
		if prior.IsNull() {
			continue
		}
		pv, _ := prior.Get(name)
		cfg = cfg.With(name, pv)
	}
	return cfg, nil
}

func (e *Engine) planInstance(ctx context.Context, ev *evaluator, r *config.Resource, inst instance, si *state.Instance) (*ResourceChange, error) {
	p, rs, err := e.resourceSchema(ctx, r.Addr)
	if err != nil {
		return nil, err
	}
	ia := addr.Instance{Resource: r.Addr, Key: inst.Key}
	ch := &ResourceChange{Addr: ia, Each: inst.Each, Before: value.Null, Provider: r.Addr.Provider()}
	var prior *state.Object
	if si != nil {
		prior = si.Current
	}
	priorAttrs := value.Null
	if prior != nil {
		priorAttrs = prior.Attrs
	}
	cfg, err := e.finalConfig(ev, rs, r, inst, priorAttrs)
	if err != nil {
		return nil, err
	}
	replaceAction := DeleteThenCreate
	if r.Lifecycle.CreateBeforeDestroy {
		replaceAction = CreateThenDelete
	}
	if prior == nil {
		resp, err := p.PlanResource(ctx, provider.PlanRequest{Type: r.Addr.Type, Prior: value.Null, Config: cfg})
		if err != nil {
			return nil, err
		}
		ch.Action, ch.After = Create, resp.Planned
		return ch, nil
	}
	ch.Before = prior.Attrs
	if prior.Status == state.StatusTainted {
		resp, err := p.PlanResource(ctx, provider.PlanRequest{Type: r.Addr.Type, Prior: value.Null, Config: cfg})
		if err != nil {
			return nil, err
		}
		ch.Action, ch.After, ch.Reason = replaceAction, resp.Planned, "it is tainted"
		return ch, e.checkPreventDestroy(r, ch)
	}
	resp, err := p.PlanResource(ctx, provider.PlanRequest{Type: r.Addr.Type, Prior: prior.Attrs, Config: cfg})
	if err != nil {
		return nil, err
	}
	if len(resp.RequiresReplace) > 0 {
		// A replacement is a brand-new object: plan it as a create so the
		// provider decides which values are known.
		fresh, err := p.PlanResource(ctx, provider.PlanRequest{Type: r.Addr.Type, Prior: value.Null, Config: cfg})
		if err != nil {
			return nil, err
		}
		ch.Action, ch.After = replaceAction, fresh.Planned
		ch.RequiresReplace = append([]string(nil), resp.RequiresReplace...)
		sort.Strings(ch.RequiresReplace)
		return ch, e.checkPreventDestroy(r, ch)
	}
	ch.After = resp.Planned
	if resp.Planned.Equal(prior.Attrs) {
		ch.Action = NoOp
	} else {
		ch.Action = Update
	}
	return ch, nil
}

func (e *Engine) checkPreventDestroy(r *config.Resource, ch *ResourceChange) error {
	if r.Lifecycle.PreventDestroy {
		return fmt.Errorf("lifecycle.prevent_destroy is set, but the plan would replace this object")
	}
	return nil
}

// Refresh returns a copy of prior updated with the real state of every
// tracked object, plus the differences found.
func (e *Engine) Refresh(ctx context.Context, prior *state.State) (*state.State, []Drift, error) {
	st := prior.Clone()
	drift, err := e.refresh(ctx, st, true)
	if err != nil {
		return nil, nil, err
	}
	return st, drift, nil
}

// refresh updates st in place from providers. When full is false only
// pending (interrupted) creates are resolved.
func (e *Engine) refresh(ctx context.Context, st *state.State, full bool) ([]Drift, error) {
	type job struct {
		inst    *state.Instance
		deposed bool
	}
	var jobs []job
	for _, inst := range st.Sorted() {
		if inst.Current != nil && (full || inst.Current.Status == state.StatusPending) {
			jobs = append(jobs, job{inst, false})
		}
		if inst.Deposed != nil && full {
			jobs = append(jobs, job{inst, true})
		}
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	g := dag.New()
	byName := map[string]job{}
	for i, j := range jobs {
		name := fmt.Sprintf("%06d", i)
		g.Add(name)
		byName[name] = j
	}
	results := make(map[string]*Drift)
	newObjs := make(map[string]*state.Object)
	var resMu sync.Mutex
	failures, err := g.Walk(ctx, e.parallelism(), func(ctx context.Context, name string) error {
		j := byName[name]
		obj := j.inst.Current
		if j.deposed {
			obj = j.inst.Deposed
		}
		p, err := e.Provider(ctx, j.inst.Provider)
		if err != nil {
			return err
		}
		ev := Event{Addr: j.inst.Addr, Op: OpRefresh, Deposed: j.deposed}
		e.Hooks.pre(ev)
		start := time.Now()
		pending := obj.Status == state.StatusPending
		resp, err := p.ReadResource(ctx, provider.ReadRequest{Type: j.inst.Addr.Type, Prior: obj.Attrs,
			Pending: pending, CreateToken: obj.CreateToken})
		e.Hooks.post(ev, resp.Current, err, start)
		if err != nil {
			return fmt.Errorf("refreshing %s: %w", j.inst.Addr, err)
		}
		var d *Drift
		var next *state.Object
		switch {
		case pending && resp.Current.IsNull():
			d = &Drift{Addr: j.inst.Addr, Kind: "abandoned", Before: obj.Attrs, After: value.Null}
		case pending:
			next = &state.Object{Status: state.StatusReady, Attrs: resp.Current}
			d = &Drift{Addr: j.inst.Addr, Kind: "recovered", Before: obj.Attrs, After: resp.Current}
		case resp.Current.IsNull():
			d = &Drift{Addr: j.inst.Addr, Deposed: j.deposed, Kind: "deleted", Before: obj.Attrs, After: value.Null}
		default:
			next = &state.Object{Status: obj.Status, Attrs: resp.Current}
			if !resp.Current.Equal(obj.Attrs) {
				d = &Drift{Addr: j.inst.Addr, Deposed: j.deposed, Kind: "changed", Before: obj.Attrs, After: resp.Current}
			}
		}
		resMu.Lock()
		results[name] = d
		newObjs[name] = next
		resMu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(failures) > 0 {
		return nil, joinNodeErrors(failures)
	}
	var drift []Drift
	for i, j := range jobs {
		name := fmt.Sprintf("%06d", i)
		if j.deposed {
			j.inst.Deposed = newObjs[name]
		} else {
			j.inst.Current = newObjs[name]
		}
		if j.inst.Current == nil && j.inst.Deposed == nil {
			st.Remove(j.inst.Addr)
		}
		if d := results[name]; d != nil {
			drift = append(drift, *d)
			e.log().Info("drift detected", "addr", d.Addr.String(), "kind", d.Kind)
		}
	}
	return drift, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
