// Package eval evaluates syntax expressions against a scope of variables,
// propagating unknown values so that plans can express "known after apply".
package eval

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/archit2501/plait/internal/syntax"
	"github.com/archit2501/plait/internal/value"
)

// Error is an evaluation error tied to a source position.
type Error struct {
	Pos syntax.Pos
	Msg string
}

func (e *Error) Error() string { return e.Pos.String() + ": " + e.Msg }

func errf(p syntax.Pos, format string, args ...any) error {
	return &Error{Pos: p, Msg: fmt.Sprintf(format, args...)}
}

// Context is the scope for evaluation.
type Context struct {
	// Variables maps root names (var, local, each, count, resource types, …)
	// to values.
	Variables map[string]value.Value
	// Resolver, if set, is consulted for root names missing from Variables.
	Resolver func(root string) (value.Value, bool, error)
	// Funcs is the function table; nil means the standard library.
	Funcs map[string]Function
	// BaseDir is the directory relative file paths (the file function) resolve against.
	BaseDir string
}

func (c *Context) lookup(root string, pos syntax.Pos) (value.Value, error) {
	if v, ok := c.Variables[root]; ok {
		return v, nil
	}
	if c.Resolver != nil {
		v, ok, err := c.Resolver(root)
		if err != nil {
			var ee *Error
			if errors.As(err, &ee) {
				return value.Null, err
			}
			return value.Null, errf(pos, "%v", err)
		}
		if ok {
			return v, nil
		}
	}
	return value.Null, errf(pos, "unknown reference %q", root)
}

func (c *Context) child(extra map[string]value.Value) *Context {
	vars := make(map[string]value.Value, len(c.Variables)+len(extra))
	for k, v := range c.Variables {
		vars[k] = v
	}
	for k, v := range extra {
		vars[k] = v
	}
	return &Context{Variables: vars, Resolver: c.Resolver, Funcs: c.Funcs, BaseDir: c.BaseDir}
}

// Eval evaluates e in ctx.
func Eval(e syntax.Expr, ctx *Context) (value.Value, error) {
	switch x := e.(type) {
	case *syntax.LiteralExpr:
		return x.Val, nil
	case *syntax.TemplateExpr:
		return evalTemplate(x, ctx)
	case *syntax.TraversalExpr:
		var base value.Value
		var err error
		if x.Source != nil {
			base, err = Eval(x.Source, ctx)
		} else {
			base, err = ctx.lookup(x.Root, x.Pos)
		}
		if err != nil {
			return value.Null, err
		}
		return traverse(base, x.Steps, ctx, x.Pos)
	case *syntax.FuncCallExpr:
		return evalCall(x, ctx)
	case *syntax.BinaryExpr:
		return evalBinary(x, ctx)
	case *syntax.UnaryExpr:
		v, err := Eval(x.X, ctx)
		if err != nil {
			return value.Null, err
		}
		if !v.IsKnown() {
			return value.Unknown, nil
		}
		switch x.Op {
		case "-":
			if v.Kind() != value.KindNumber {
				return value.Null, errf(x.Pos, "unary '-' requires a number, got %s", v.Kind())
			}
			return value.Number(-v.AsNumber()), nil
		default:
			if v.Kind() != value.KindBool {
				return value.Null, errf(x.Pos, "'!' requires a bool, got %s", v.Kind())
			}
			return value.Bool(!v.AsBool()), nil
		}
	case *syntax.ConditionalExpr:
		c, err := Eval(x.Cond, ctx)
		if err != nil {
			return value.Null, err
		}
		if !c.IsKnown() {
			return value.Unknown, nil
		}
		b, err := c.Truthy()
		if err != nil {
			return value.Null, errf(x.Pos, "condition: %v", err)
		}
		if b {
			return Eval(x.True, ctx)
		}
		return Eval(x.False, ctx)
	case *syntax.TupleExpr:
		elems := make([]value.Value, len(x.Elems))
		for i, el := range x.Elems {
			v, err := Eval(el, ctx)
			if err != nil {
				return value.Null, err
			}
			elems[i] = v
		}
		return value.List(elems...), nil
	case *syntax.ObjectExpr:
		m := make(map[string]value.Value, len(x.Items))
		for _, it := range x.Items {
			k, err := Eval(it.Key, ctx)
			if err != nil {
				return value.Null, err
			}
			if !k.IsKnown() {
				return value.Unknown, nil
			}
			ks, err := k.ToString()
			if err != nil {
				return value.Null, errf(it.Key.Start(), "object key: %v", err)
			}
			v, err := Eval(it.Val, ctx)
			if err != nil {
				return value.Null, err
			}
			m[ks] = v
		}
		return value.Map(m), nil
	case *syntax.ForExpr:
		return evalFor(x, ctx)
	}
	return value.Null, fmt.Errorf("eval: unsupported expression %T", e)
}

func evalTemplate(t *syntax.TemplateExpr, ctx *Context) (value.Value, error) {
	if len(t.Parts) == 1 {
		if _, lit := t.Parts[0].(*syntax.LiteralExpr); !lit {
			return Eval(t.Parts[0], ctx)
		}
	}
	var b strings.Builder
	unknown := false
	for _, part := range t.Parts {
		v, err := Eval(part, ctx)
		if err != nil {
			return value.Null, err
		}
		if !v.IsKnown() {
			unknown = true
			continue
		}
		if v.IsNull() {
			return value.Null, errf(part.Start(), "cannot interpolate a null value")
		}
		s, err := v.ToString()
		if err != nil {
			return value.Null, errf(part.Start(), "cannot interpolate %s value", v.Kind())
		}
		b.WriteString(s)
	}
	if unknown {
		return value.Unknown, nil
	}
	return value.String(b.String()), nil
}

func traverse(base value.Value, steps []syntax.Step, ctx *Context, pos syntax.Pos) (value.Value, error) {
	cur := base
	for i, st := range steps {
		if !cur.IsKnown() {
			return value.Unknown, nil
		}
		switch s := st.(type) {
		case syntax.AttrStep:
			if cur.Kind() != value.KindMap {
				return value.Null, errf(pos, "cannot access attribute %q on a %s value", s.Name, cur.Kind())
			}
			v, ok := cur.Get(s.Name)
			if !ok {
				return value.Null, errf(pos, "object has no attribute %q", s.Name)
			}
			cur = v
		case syntax.IndexStep:
			k, err := Eval(s.Key, ctx)
			if err != nil {
				return value.Null, err
			}
			if !k.IsKnown() {
				return value.Unknown, nil
			}
			v, err := index(cur, k, pos)
			if err != nil {
				return value.Null, err
			}
			cur = v
		case syntax.SplatStep:
			var elems []value.Value
			switch cur.Kind() {
			case value.KindNull:
				return value.List(), nil
			case value.KindList:
				elems = cur.AsList()
			case value.KindMap:
				return value.Null, errf(pos, "splat operator requires a list; use values() for maps")
			default:
				elems = []value.Value{cur}
			}
			out := make([]value.Value, len(elems))
			for j, el := range elems {
				v, err := traverse(el, steps[i+1:], ctx, pos)
				if err != nil {
					return value.Null, err
				}
				out[j] = v
			}
			return value.List(out...), nil
		}
	}
	return cur, nil
}

func index(coll, key value.Value, pos syntax.Pos) (value.Value, error) {
	switch coll.Kind() {
	case value.KindList:
		n, err := key.ToNumber()
		if err != nil || n != math.Trunc(n) {
			return value.Null, errf(pos, "list index must be a whole number, got %s", key.GoString())
		}
		v, ok := coll.Index(int(n))
		if !ok {
			return value.Null, errf(pos, "index %d out of range for list of length %d", int(n), coll.Len())
		}
		return v, nil
	case value.KindMap:
		ks, err := key.ToString()
		if err != nil {
			return value.Null, errf(pos, "map key must be a string, got %s", key.Kind())
		}
		v, ok := coll.Get(ks)
		if !ok {
			return value.Null, errf(pos, "key %q does not exist", ks)
		}
		return v, nil
	}
	return value.Null, errf(pos, "cannot index a %s value", coll.Kind())
}

func evalBinary(x *syntax.BinaryExpr, ctx *Context) (value.Value, error) {
	l, err := Eval(x.LHS, ctx)
	if err != nil {
		return value.Null, err
	}
	r, err := Eval(x.RHS, ctx)
	if err != nil {
		return value.Null, err
	}
	if !l.IsWhollyKnown() || !r.IsWhollyKnown() {
		return value.Unknown, nil
	}
	switch x.Op {
	case "==":
		return value.Bool(l.Equal(r)), nil
	case "!=":
		return value.Bool(!l.Equal(r)), nil
	case "&&", "||":
		lb, lerr := l.Truthy()
		rb, rerr := r.Truthy()
		if lerr != nil || rerr != nil {
			return value.Null, errf(x.Pos, "operator %s requires bool operands, got %s and %s", x.Op, l.Kind(), r.Kind())
		}
		if x.Op == "&&" {
			return value.Bool(lb && rb), nil
		}
		return value.Bool(lb || rb), nil
	}
	ln, lerr := l.ToNumber()
	rn, rerr := r.ToNumber()
	if lerr != nil || rerr != nil {
		return value.Null, errf(x.Pos, "operator %s requires numeric operands, got %s and %s", x.Op, l.Kind(), r.Kind())
	}
	switch x.Op {
	case "+":
		return value.Number(ln + rn), nil
	case "-":
		return value.Number(ln - rn), nil
	case "*":
		return value.Number(ln * rn), nil
	case "/":
		if rn == 0 {
			return value.Null, errf(x.Pos, "division by zero")
		}
		return value.Number(ln / rn), nil
	case "%":
		if rn == 0 {
			return value.Null, errf(x.Pos, "modulo by zero")
		}
		return value.Number(math.Mod(ln, rn)), nil
	case "<":
		return value.Bool(ln < rn), nil
	case ">":
		return value.Bool(ln > rn), nil
	case "<=":
		return value.Bool(ln <= rn), nil
	case ">=":
		return value.Bool(ln >= rn), nil
	}
	return value.Null, errf(x.Pos, "unsupported operator %q", x.Op)
}

func evalFor(x *syntax.ForExpr, ctx *Context) (value.Value, error) {
	coll, err := Eval(x.Coll, ctx)
	if err != nil {
		return value.Null, err
	}
	if !coll.IsKnown() {
		return value.Unknown, nil
	}
	type pair struct{ k, v value.Value }
	var items []pair
	switch coll.Kind() {
	case value.KindList:
		for i, e := range coll.AsList() {
			items = append(items, pair{value.Int(i), e})
		}
	case value.KindMap:
		for _, k := range coll.Keys() {
			v, _ := coll.Get(k)
			items = append(items, pair{value.String(k), v})
		}
	case value.KindNull:
	default:
		return value.Null, errf(x.Pos, "for-expression requires a list or map, got %s", coll.Kind())
	}
	var list []value.Value
	obj := map[string]value.Value{}
	for _, it := range items {
		bind := map[string]value.Value{x.ValVar: it.v}
		if x.KeyVar != "" {
			bind[x.KeyVar] = it.k
		}
		inner := ctx.child(bind)
		if x.Cond != nil {
			c, err := Eval(x.Cond, inner)
			if err != nil {
				return value.Null, err
			}
			if !c.IsKnown() {
				return value.Unknown, nil
			}
			keep, err := c.Truthy()
			if err != nil {
				return value.Null, errf(x.Cond.Start(), "for-expression condition: %v", err)
			}
			if !keep {
				continue
			}
		}
		v, err := Eval(x.ValExpr, inner)
		if err != nil {
			return value.Null, err
		}
		if x.KeyExpr == nil {
			list = append(list, v)
			continue
		}
		k, err := Eval(x.KeyExpr, inner)
		if err != nil {
			return value.Null, err
		}
		if !k.IsKnown() {
			return value.Unknown, nil
		}
		ks, err := k.ToString()
		if err != nil {
			return value.Null, errf(x.KeyExpr.Start(), "for-expression key: %v", err)
		}
		if _, dup := obj[ks]; dup {
			return value.Null, errf(x.KeyExpr.Start(), "duplicate key %q in for-expression result", ks)
		}
		obj[ks] = v
	}
	if x.KeyExpr != nil {
		return value.Map(obj), nil
	}
	return value.List(list...), nil
}

func evalCall(x *syntax.FuncCallExpr, ctx *Context) (value.Value, error) {
	funcs := ctx.Funcs
	if funcs == nil {
		funcs = Stdlib()
	}
	fn, ok := funcs[x.Name]
	if !ok {
		return value.Null, errf(x.Pos, "call to unknown function %q", x.Name)
	}
	args := make([]value.Value, 0, len(x.Args))
	for i, a := range x.Args {
		v, err := Eval(a, ctx)
		if err != nil {
			return value.Null, err
		}
		if x.ExpandFinal && i == len(x.Args)-1 {
			if !v.IsKnown() {
				return value.Unknown, nil
			}
			if v.Kind() != value.KindList {
				return value.Null, errf(a.Start(), "expanded argument must be a list, got %s", v.Kind())
			}
			args = append(args, v.AsList()...)
			continue
		}
		args = append(args, v)
	}
	if len(args) < fn.MinArgs || (fn.MaxArgs >= 0 && len(args) > fn.MaxArgs) {
		return value.Null, errf(x.Pos, "function %q: %s, got %d", x.Name, arity(fn), len(args))
	}
	for _, a := range args {
		if (fn.AllowUnknown && !a.IsKnown()) || (!fn.AllowUnknown && !a.IsWhollyKnown()) {
			return value.Unknown, nil
		}
	}
	out, err := fn.Impl(ctx, args)
	if err != nil {
		return value.Null, errf(x.Pos, "%s: %v", x.Name, err)
	}
	return out, nil
}

func arity(fn Function) string {
	switch {
	case fn.MaxArgs < 0:
		return "expects at least " + strconv.Itoa(fn.MinArgs) + " argument(s)"
	case fn.MinArgs == fn.MaxArgs:
		return "expects " + strconv.Itoa(fn.MinArgs) + " argument(s)"
	}
	return "expects " + strconv.Itoa(fn.MinArgs) + " to " + strconv.Itoa(fn.MaxArgs) + " arguments"
}

// sortedUniqueStrings is a helper for toset and distinct.
func sortedUniqueStrings(ss []string) []string {
	sort.Strings(ss)
	out := ss[:0]
	for i, s := range ss {
		if i == 0 || s != ss[i-1] {
			out = append(out, s)
		}
	}
	return out
}
