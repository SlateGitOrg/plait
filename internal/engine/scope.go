package engine

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/archit2501/plait/internal/addr"
	"github.com/archit2501/plait/internal/config"
	"github.com/archit2501/plait/internal/eval"
	"github.com/archit2501/plait/internal/syntax"
	"github.com/archit2501/plait/internal/value"
)

// evaluator builds evaluation scopes on demand. Only the locals and
// resources an expression actually references (transitively through
// locals) are materialized, so evaluating one instance never depends on
// unrelated parts of the configuration.
type evaluator struct {
	e *Engine
	// resourceValue returns the aggregate value of a resource: an object,
	// a list (count) or a map (for_each).
	resourceValue func(ra addr.Resource) (value.Value, error)

	mu     sync.Mutex
	locals map[string]value.Value
}

func newEvaluator(e *Engine, rv func(addr.Resource) (value.Value, error)) *evaluator {
	return &evaluator{e: e, resourceValue: rv, locals: map[string]value.Value{}}
}

// context returns an eval context sufficient for exprs, with extra
// variables (count, each) layered on top.
func (ev *evaluator) context(extra map[string]value.Value, exprs ...syntax.Expr) (*eval.Context, error) {
	vars := map[string]value.Value{"var": value.Map(ev.e.Vars), "path": ev.e.pathValue()}
	localVals := map[string]value.Value{}
	resources := map[string]map[string]value.Value{}
	for _, x := range exprs {
		if x == nil {
			continue
		}
		for _, ref := range syntax.References(x) {
			if len(ref.Parts) < 2 {
				continue
			}
			root, name := ref.Parts[0], ref.Parts[1]
			switch root {
			case "var", "path", "count", "each":
			case "local":
				v, err := ev.local(name)
				if err != nil {
					return nil, err
				}
				localVals[name] = v
			default:
				ra := addr.Resource{Type: root, Name: name}
				if _, ok := ev.e.Config.Resources[ra.String()]; !ok {
					continue // validated at load time; a for-bound name shadows nothing here
				}
				v, err := ev.resourceValue(ra)
				if err != nil {
					return nil, err
				}
				if resources[root] == nil {
					resources[root] = map[string]value.Value{}
				}
				resources[root][name] = v
			}
		}
	}
	vars["local"] = value.Map(localVals)
	for typ, m := range resources {
		vars[typ] = value.Map(m)
	}
	for k, v := range extra {
		vars[k] = v
	}
	return &eval.Context{Variables: vars, BaseDir: ev.e.Config.Dir}, nil
}

// local evaluates (and memoizes) a local value.
func (ev *evaluator) local(name string) (value.Value, error) {
	ev.mu.Lock()
	v, ok := ev.locals[name]
	ev.mu.Unlock()
	if ok {
		return v, nil
	}
	l, ok := ev.e.Config.Locals[name]
	if !ok {
		return value.Null, fmt.Errorf("no local value %q", name)
	}
	ctx, err := ev.context(nil, l.Expr)
	if err != nil {
		return value.Null, err
	}
	v, err = eval.Eval(l.Expr, ctx)
	if err != nil {
		return value.Null, err
	}
	ev.mu.Lock()
	ev.locals[name] = v
	ev.mu.Unlock()
	return v, nil
}

// instance is one expansion of a resource block.
type instance struct {
	Key  addr.Key
	Each value.Value // each.value for for_each instances
}

// expand evaluates count / for_each. Both must be known at plan time,
// exactly as in Terraform: the set of instances is part of the plan.
func (ev *evaluator) expand(r *config.Resource) ([]instance, error) {
	switch {
	case r.Count != nil:
		ctx, err := ev.context(nil, r.Count)
		if err != nil {
			return nil, err
		}
		v, err := eval.Eval(r.Count, ctx)
		if err != nil {
			return nil, err
		}
		if !v.IsKnown() {
			return nil, posErr(r.Count.Start(), "the count value for %s depends on attributes that are only known after apply; "+
				"count must be known at plan time", r.Addr)
		}
		n, err := v.ToNumber()
		if err != nil || n < 0 || n != math.Trunc(n) {
			return nil, posErr(r.Count.Start(), "count for %s must be a non-negative whole number, got %s", r.Addr, v.GoString())
		}
		out := make([]instance, int(n))
		for i := range out {
			out[i] = instance{Key: addr.IntK(i)}
		}
		return out, nil
	case r.ForEach != nil:
		ctx, err := ev.context(nil, r.ForEach)
		if err != nil {
			return nil, err
		}
		v, err := eval.Eval(r.ForEach, ctx)
		if err != nil {
			return nil, err
		}
		if !v.IsKnown() {
			return nil, posErr(r.ForEach.Start(), "the for_each value for %s depends on attributes that are only known after apply; "+
				"its keys must be known at plan time", r.Addr)
		}
		switch v.Kind() {
		case value.KindMap:
			var out []instance
			for _, k := range v.Keys() {
				e, _ := v.Get(k)
				out = append(out, instance{Key: addr.StrK(k), Each: e})
			}
			return out, nil
		case value.KindList:
			seen := map[string]bool{}
			var out []instance
			for _, e := range v.AsList() {
				if !e.IsKnown() || e.Kind() != value.KindString {
					return nil, posErr(r.ForEach.Start(), "for_each over a list requires known strings (a set); got %s", e.GoString())
				}
				s := e.AsString()
				if seen[s] {
					continue
				}
				seen[s] = true
				out = append(out, instance{Key: addr.StrK(s), Each: e})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Key.Str < out[j].Key.Str })
			return out, nil
		}
		return nil, posErr(r.ForEach.Start(), "for_each for %s must be a map or a set of strings, got %s", r.Addr, v.Kind())
	}
	return []instance{{Key: addr.Key{}}}, nil
}

// instanceVars returns the count / each variables for an instance.
func instanceVars(r *config.Resource, inst instance) map[string]value.Value {
	switch {
	case r.Count != nil:
		return map[string]value.Value{"count": value.Map(map[string]value.Value{"index": value.Int(inst.Key.Int)})}
	case r.ForEach != nil:
		return map[string]value.Value{"each": value.Map(map[string]value.Value{
			"key": value.String(inst.Key.Str), "value": inst.Each})}
	}
	return nil
}

// instanceConfig evaluates a resource instance's provider-facing
// attributes into an object.
func (ev *evaluator) instanceConfig(r *config.Resource, inst instance) (value.Value, error) {
	exprs := make([]syntax.Expr, len(r.Attrs))
	for i, a := range r.Attrs {
		exprs[i] = a.Expr
	}
	ctx, err := ev.context(instanceVars(r, inst), exprs...)
	if err != nil {
		return value.Null, err
	}
	m := make(map[string]value.Value, len(r.Attrs))
	for _, a := range r.Attrs {
		v, err := eval.Eval(a.Expr, ctx)
		if err != nil {
			return value.Null, err
		}
		m[a.Name] = v
	}
	return value.Map(m), nil
}

// aggregate builds the value a reference to a whole resource sees.
func aggregate(r *config.Resource, keys []addr.Key, get func(addr.Key) value.Value) value.Value {
	switch {
	case r.Count != nil:
		sorted := append([]addr.Key(nil), keys...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Int < sorted[j].Int })
		l := make([]value.Value, len(sorted))
		for i, k := range sorted {
			l[i] = get(k)
		}
		return value.List(l...)
	case r.ForEach != nil:
		m := make(map[string]value.Value, len(keys))
		for _, k := range keys {
			m[k.Str] = get(k)
		}
		return value.Map(m)
	}
	return get(addr.Key{})
}

func posErr(p syntax.Pos, format string, args ...any) error {
	return &syntax.Error{Pos: p, Msg: fmt.Sprintf(format, args...)}
}

// nullUnknowns replaces unknown values with null so that a planned object
// can be written into state while its create is in flight.
func nullUnknowns(v value.Value) value.Value {
	switch v.Kind() {
	case value.KindUnknown:
		return value.Null
	case value.KindList:
		l := v.AsList()
		for i := range l {
			l[i] = nullUnknowns(l[i])
		}
		return value.List(l...)
	case value.KindMap:
		m := v.AsMap()
		for k, e := range m {
			m[k] = nullUnknowns(e)
		}
		return value.Map(m)
	}
	return v
}
