package config

import (
	"fmt"
	"sort"

	"github.com/archit2501/plait/internal/addr"
	"github.com/archit2501/plait/internal/dag"
	"github.com/archit2501/plait/internal/syntax"
)

// Graph node name prefixes for non-resource objects.
const (
	VarPrefix      = "var."
	LocalPrefix    = "local."
	OutputPrefix   = "output."
	ProviderPrefix = "provider."
)

// refKind describes where an expression appears, which limits what it may
// reference.
type refKind uint8

const (
	inResource refKind = iota
	inResourceCount
	inLocal
	inOutput
	inProvider
)

// resolveRef maps a reference to the graph node it depends on. It returns ""
// for references that need no graph edge (count.index, each.value, path.*).
func (c *Config) resolveRef(ref syntax.Reference, kind refKind, res *Resource) (string, error) {
	errAt := func(format string, args ...any) error {
		return &syntax.Error{Pos: ref.Pos, Msg: fmt.Sprintf(format, args...)}
	}
	root := ref.Parts[0]
	need := func(what string) (string, error) {
		if len(ref.Parts) < 2 {
			return "", errAt("reference to %q must be followed by an attribute name (%s.<name>)", root, what)
		}
		return ref.Parts[1], nil
	}
	switch root {
	case "var":
		name, err := need("var")
		if err != nil {
			return "", err
		}
		if _, ok := c.Variables[name]; !ok {
			return "", errAt("reference to undeclared input variable %q", name)
		}
		return VarPrefix + name, nil
	case "local":
		if kind == inProvider {
			return "", errAt("provider configuration may only reference variables")
		}
		name, err := need("local")
		if err != nil {
			return "", err
		}
		if _, ok := c.Locals[name]; !ok {
			return "", errAt("reference to undeclared local value %q", name)
		}
		return LocalPrefix + name, nil
	case "count":
		if kind != inResource || res == nil || res.Count == nil {
			return "", errAt("count.index is only valid in resources that set count")
		}
		if len(ref.Parts) != 2 || ref.Parts[1] != "index" {
			return "", errAt("the only count attribute is count.index")
		}
		return "", nil
	case "each":
		if kind != inResource || res == nil || res.ForEach == nil {
			return "", errAt("each.key and each.value are only valid in resources that set for_each")
		}
		if len(ref.Parts) < 2 || (ref.Parts[1] != "key" && ref.Parts[1] != "value") {
			return "", errAt("the each object has only key and value attributes")
		}
		return "", nil
	case "path":
		if len(ref.Parts) < 2 || (ref.Parts[1] != "module" && ref.Parts[1] != "cwd") {
			return "", errAt("the path object has only module and cwd attributes")
		}
		return "", nil
	case "self", "data", "module", "terraform":
		return "", errAt("%q references are not supported by plait", root)
	}
	if kind == inProvider {
		return "", errAt("provider configuration may only reference variables")
	}
	name, err := need("resource")
	if err != nil {
		return "", err
	}
	ra := addr.Resource{Type: root, Name: name}
	if _, ok := c.Resources[ra.String()]; !ok {
		return "", errAt("reference to undeclared resource %s", ra)
	}
	// count/for_each may reference resources too, but their values must be
	// known at plan time; that is enforced during instance expansion.
	return ra.String(), nil
}

// exprDeps returns the sorted, de-duplicated graph nodes referenced by exprs.
func (c *Config) exprDeps(kind refKind, res *Resource, exprs ...syntax.Expr) ([]string, error) {
	set := map[string]bool{}
	for _, e := range exprs {
		if e == nil {
			continue
		}
		for _, ref := range syntax.References(e) {
			n, err := c.resolveRef(ref, kind, res)
			if err != nil {
				return nil, err
			}
			if n != "" {
				set[n] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// ResourceDeps returns the resource addresses that r depends on, directly or
// through locals, plus explicit depends_on entries.
func (c *Config) ResourceDeps(r *Resource) ([]addr.Resource, error) {
	g, err := c.Graph()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []addr.Resource
	var visit func(n string)
	visit = func(n string) {
		for _, d := range g.DependsOn(n) {
			if seen[d] {
				continue
			}
			seen[d] = true
			if _, isRes := c.Resources[d]; isRes {
				out = append(out, c.Resources[d].Addr)
				continue // the dependency's own deps are reached through it
			}
			visit(d)
		}
	}
	visit(r.Addr.String())
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// validateReferences checks every reference in the configuration resolves.
func (c *Config) validateReferences() error {
	var diags Diagnostics
	check := func(kind refKind, res *Resource, exprs ...syntax.Expr) {
		if _, err := c.exprDeps(kind, res, exprs...); err != nil {
			diags = append(diags, err)
		}
	}
	for _, l := range c.Locals {
		check(inLocal, nil, l.Expr)
	}
	for _, o := range c.Outputs {
		check(inOutput, nil, o.Expr)
	}
	for _, p := range c.Providers {
		for _, a := range p.Attrs {
			check(inProvider, nil, a.Expr)
		}
	}
	for _, r := range c.Resources {
		check(inResourceCount, nil, r.Count, r.ForEach)
		for _, a := range r.Attrs {
			check(inResource, r, a.Expr)
		}
		for _, d := range r.DependsOn {
			if _, ok := c.Resources[d.String()]; !ok {
				diags.addf(r.Pos, "depends_on references undeclared resource %s", d)
			}
		}
	}
	sort.Slice(diags, func(i, j int) bool { return diags[i].Error() < diags[j].Error() })
	return diags.err()
}

// Graph builds the configuration-level dependency graph over variables,
// locals, resources, outputs and providers, and rejects cycles.
// The graph is computed once and cached; a Config is immutable after loading.
func (c *Config) Graph() (*dag.Graph, error) {
	if c.graph != nil {
		return c.graph, nil
	}
	g := dag.New()
	for n := range c.Variables {
		g.Add(VarPrefix + n)
	}
	addDeps := func(node string, deps []string) {
		g.Add(node)
		for _, d := range deps {
			g.AddEdge(node, d)
		}
	}
	for _, l := range c.Locals {
		deps, err := c.exprDeps(inLocal, nil, l.Expr)
		if err != nil {
			return nil, err
		}
		addDeps(LocalPrefix+l.Name, deps)
	}
	for _, o := range c.Outputs {
		deps, err := c.exprDeps(inOutput, nil, o.Expr)
		if err != nil {
			return nil, err
		}
		addDeps(OutputPrefix+o.Name, deps)
	}
	for _, p := range c.Providers {
		var exprs []syntax.Expr
		for _, a := range p.Attrs {
			exprs = append(exprs, a.Expr)
		}
		deps, err := c.exprDeps(inProvider, nil, exprs...)
		if err != nil {
			return nil, err
		}
		addDeps(ProviderPrefix+p.Name, deps)
	}
	for _, r := range c.Resources {
		exprs := []syntax.Expr{r.Count, r.ForEach}
		for _, a := range r.Attrs {
			exprs = append(exprs, a.Expr)
		}
		deps, err := c.exprDeps(inResource, r, exprs...)
		if err != nil {
			return nil, err
		}
		for _, d := range r.DependsOn {
			deps = append(deps, d.String())
		}
		if _, ok := c.Providers[r.Addr.Provider()]; ok {
			deps = append(deps, ProviderPrefix+r.Addr.Provider())
		}
		addDeps(r.Addr.String(), deps)
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	c.graph = g
	return g, nil
}
