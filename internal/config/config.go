// Package config loads a directory of *.plait files into a typed
// configuration model and validates its structure and references.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/archit2501/plait/internal/addr"
	"github.com/archit2501/plait/internal/dag"
	"github.com/archit2501/plait/internal/eval"
	"github.com/archit2501/plait/internal/syntax"
	"github.com/archit2501/plait/internal/value"
)

// FileExt is the extension of configuration files.
const FileExt = ".plait"

// Config is a loaded configuration directory.
type Config struct {
	Dir       string
	Variables map[string]*Variable
	Locals    map[string]*Local
	Resources map[string]*Resource // keyed by "type.name"
	Outputs   map[string]*Output
	Providers map[string]*Provider
	// Digest is a SHA-256 over every source file name and content. Saved
	// plans record it so that applying a plan against edited
	// configuration is refused.
	Digest string

	graph *dag.Graph
}

// Variable is an input variable declaration.
type Variable struct {
	Name        string
	Type        value.Type
	Default     *value.Value // nil means the variable is required
	Description string
	Pos         syntax.Pos
}

// Local is a named local value.
type Local struct {
	Name string
	Expr syntax.Expr
	Pos  syntax.Pos
}

// Lifecycle holds the lifecycle meta-arguments of a resource.
type Lifecycle struct {
	CreateBeforeDestroy bool
	PreventDestroy      bool
	IgnoreChanges       []string
}

// Resource is a managed resource block.
type Resource struct {
	Addr      addr.Resource
	Count     syntax.Expr
	ForEach   syntax.Expr
	DependsOn []addr.Resource
	Lifecycle Lifecycle
	// Attrs are the provider-facing attributes (meta-arguments removed).
	Attrs []*syntax.Attribute
	Pos   syntax.Pos
}

// Output is a named output value.
type Output struct {
	Name        string
	Expr        syntax.Expr
	Description string
	Pos         syntax.Pos
}

// Provider is a provider configuration block.
type Provider struct {
	Name  string
	Attrs []*syntax.Attribute
	Pos   syntax.Pos
}

// Diagnostics collects multiple configuration errors.
type Diagnostics []error

func (d Diagnostics) Error() string {
	msgs := make([]string, len(d))
	for i, e := range d {
		msgs[i] = e.Error()
	}
	return strings.Join(msgs, "\n")
}

func (d *Diagnostics) addf(pos syntax.Pos, format string, args ...any) {
	*d = append(*d, &syntax.Error{Pos: pos, Msg: fmt.Sprintf(format, args...)})
}

func (d Diagnostics) err() error {
	if len(d) == 0 {
		return nil
	}
	return d
}

// LoadDir parses every *.plait file in dir (sorted by name) and validates
// the result.
func LoadDir(dir string) (*Config, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*"+FileExt))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no %s files found in %s", FileExt, dir)
	}
	sort.Strings(matches)
	files := make(map[string]string, len(matches))
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		files[filepath.Base(m)] = string(data)
	}
	return LoadFiles(dir, files)
}

// LoadFiles builds a Config from in-memory sources keyed by file name.
func LoadFiles(dir string, files map[string]string) (*Config, error) {
	cfg := &Config{
		Dir:       dir,
		Variables: map[string]*Variable{},
		Locals:    map[string]*Local{},
		Resources: map[string]*Resource{},
		Outputs:   map[string]*Output{},
		Providers: map[string]*Provider{},
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%d:%s\x00%d:%s\x00", len(n), n, len(files[n]), files[n])
	}
	cfg.Digest = hex.EncodeToString(h.Sum(nil))
	var diags Diagnostics
	for _, n := range names {
		f, err := syntax.ParseFile(n, files[n])
		if err != nil {
			diags = append(diags, err)
			continue
		}
		cfg.decodeFile(f, &diags)
	}
	if err := diags.err(); err != nil {
		return nil, err
	}
	if err := cfg.validateReferences(); err != nil {
		return nil, err
	}
	if _, err := cfg.Graph(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) decodeFile(f *syntax.File, diags *Diagnostics) {
	for _, a := range f.Body.Attributes {
		diags.addf(a.Pos, "unexpected top-level attribute %q; only blocks are allowed", a.Name)
	}
	for _, b := range f.Body.Blocks {
		switch b.Type {
		case "variable":
			c.decodeVariable(b, diags)
		case "locals":
			if len(b.Labels) != 0 {
				diags.addf(b.Pos, "locals block takes no labels")
				continue
			}
			for _, nb := range b.Body.Blocks {
				diags.addf(nb.Pos, "unexpected block %q in locals", nb.Type)
			}
			for _, a := range b.Body.Attributes {
				if prev, dup := c.Locals[a.Name]; dup {
					diags.addf(a.Pos, "duplicate local %q (first defined at %s)", a.Name, prev.Pos)
					continue
				}
				c.Locals[a.Name] = &Local{Name: a.Name, Expr: a.Expr, Pos: a.Pos}
			}
		case "resource":
			c.decodeResource(b, diags)
		case "output":
			if !labels(b, 1, diags) {
				continue
			}
			name := b.Labels[0]
			if prev, dup := c.Outputs[name]; dup {
				diags.addf(b.Pos, "duplicate output %q (first defined at %s)", name, prev.Pos)
				continue
			}
			v := b.Body.Attr("value")
			if v == nil {
				diags.addf(b.Pos, "output %q requires a value attribute", name)
				continue
			}
			out := &Output{Name: name, Expr: v.Expr, Pos: b.Pos}
			for _, a := range b.Body.Attributes {
				switch a.Name {
				case "value":
				case "description":
					out.Description = staticString(a, diags)
				default:
					diags.addf(a.Pos, "unsupported argument %q in output", a.Name)
				}
			}
			c.Outputs[name] = out
		case "provider":
			if !labels(b, 1, diags) {
				continue
			}
			name := b.Labels[0]
			if prev, dup := c.Providers[name]; dup {
				diags.addf(b.Pos, "duplicate provider %q (first defined at %s)", name, prev.Pos)
				continue
			}
			c.Providers[name] = &Provider{Name: name, Attrs: b.Body.Attributes, Pos: b.Pos}
		default:
			diags.addf(b.Pos, "unsupported block type %q", b.Type)
		}
	}
}

func labels(b *syntax.Block, n int, diags *Diagnostics) bool {
	if len(b.Labels) != n {
		diags.addf(b.Pos, "%s block requires %d label(s), got %d", b.Type, n, len(b.Labels))
		return false
	}
	if !validName(b.Labels[n-1]) {
		diags.addf(b.Pos, "invalid name %q: use letters, digits, underscores and dashes", b.Labels[n-1])
		return false
	}
	return true
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(i > 0 && (r == '-' || (r >= '0' && r <= '9')))
		if !ok {
			return false
		}
	}
	return true
}

// staticString evaluates an attribute that must be a constant string.
func staticString(a *syntax.Attribute, diags *Diagnostics) string {
	v, err := eval.Eval(a.Expr, &eval.Context{})
	if err != nil || v.Kind() != value.KindString {
		diags.addf(a.Pos, "%s must be a constant string", a.Name)
		return ""
	}
	return v.AsString()
}

func staticBool(a *syntax.Attribute, diags *Diagnostics) bool {
	v, err := eval.Eval(a.Expr, &eval.Context{})
	if err != nil || v.Kind() != value.KindBool {
		diags.addf(a.Pos, "%s must be a constant bool", a.Name)
		return false
	}
	return v.AsBool()
}

func (c *Config) decodeVariable(b *syntax.Block, diags *Diagnostics) {
	if !labels(b, 1, diags) {
		return
	}
	name := b.Labels[0]
	if prev, dup := c.Variables[name]; dup {
		diags.addf(b.Pos, "duplicate variable %q (first defined at %s)", name, prev.Pos)
		return
	}
	v := &Variable{Name: name, Type: value.AnyType, Pos: b.Pos}
	for _, a := range b.Body.Attributes {
		switch a.Name {
		case "type":
			t, err := parseType(a.Expr)
			if err != nil {
				diags.addf(a.Pos, "invalid type for variable %q: %v", name, err)
				continue
			}
			v.Type = t
		case "description":
			v.Description = staticString(a, diags)
		case "default":
			if refs := syntax.References(a.Expr); len(refs) > 0 {
				diags.addf(a.Pos, "variable default may not contain references")
				continue
			}
			d, err := eval.Eval(a.Expr, &eval.Context{})
			if err != nil {
				*diags = append(*diags, err)
				continue
			}
			v.Default = &d
		default:
			diags.addf(a.Pos, "unsupported argument %q in variable", a.Name)
		}
	}
	if v.Default != nil {
		conv, err := value.Convert(*v.Default, v.Type)
		if err != nil {
			diags.addf(b.Pos, "default for variable %q does not match type %s: %v", name, v.Type, err)
		} else {
			v.Default = &conv
		}
	}
	c.Variables[name] = v
}

// parseType interprets a type expression: string, number, bool, any, list,
// map, list(T), map(T).
func parseType(e syntax.Expr) (value.Type, error) {
	switch x := e.(type) {
	case *syntax.TraversalExpr:
		if x.Source != nil || len(x.Steps) != 0 {
			break
		}
		switch x.Root {
		case "string":
			return value.Type{Kind: value.TypeString}, nil
		case "number":
			return value.Type{Kind: value.TypeNumber}, nil
		case "bool":
			return value.Type{Kind: value.TypeBool}, nil
		case "any":
			return value.AnyType, nil
		case "list":
			return value.Type{Kind: value.TypeList}, nil
		case "map":
			return value.Type{Kind: value.TypeMap}, nil
		}
		return value.Type{}, fmt.Errorf("unknown type %q", x.Root)
	case *syntax.FuncCallExpr:
		if (x.Name == "list" || x.Name == "map") && len(x.Args) == 1 {
			elem, err := parseType(x.Args[0])
			if err != nil {
				return value.Type{}, err
			}
			k := value.TypeList
			if x.Name == "map" {
				k = value.TypeMap
			}
			return value.Type{Kind: k, Elem: &elem}, nil
		}
		return value.Type{}, fmt.Errorf("unsupported type constructor %q", x.Name)
	}
	return value.Type{}, errors.New("expected a type such as string, number, bool, list(string) or map(any)")
}

// metaArgs are resource arguments interpreted by plait itself.
var metaArgs = map[string]bool{"count": true, "for_each": true, "depends_on": true, "provider": true}

func (c *Config) decodeResource(b *syntax.Block, diags *Diagnostics) {
	if len(b.Labels) != 2 {
		diags.addf(b.Pos, "resource block requires 2 labels (type and name), got %d", len(b.Labels))
		return
	}
	if !validName(b.Labels[0]) || !validName(b.Labels[1]) {
		diags.addf(b.Pos, "invalid resource type or name %q.%q", b.Labels[0], b.Labels[1])
		return
	}
	ra := addr.Resource{Type: b.Labels[0], Name: b.Labels[1]}
	if !strings.Contains(ra.Type, "_") {
		diags.addf(b.Pos, "resource type %q must be prefixed with its provider name (e.g. local_file)", ra.Type)
		return
	}
	if prev, dup := c.Resources[ra.String()]; dup {
		diags.addf(b.Pos, "duplicate resource %s (first defined at %s)", ra, prev.Pos)
		return
	}
	r := &Resource{Addr: ra, Pos: b.Pos}
	for _, a := range b.Body.Attributes {
		if !metaArgs[a.Name] {
			r.Attrs = append(r.Attrs, a)
			continue
		}
		switch a.Name {
		case "count":
			r.Count = a.Expr
		case "for_each":
			r.ForEach = a.Expr
		case "depends_on":
			deps, err := decodeDependsOn(a)
			if err != nil {
				*diags = append(*diags, err)
				continue
			}
			r.DependsOn = deps
		case "provider":
			diags.addf(a.Pos, "provider aliases are not supported; the provider is inferred from the resource type")
		}
	}
	if r.Count != nil && r.ForEach != nil {
		diags.addf(b.Pos, "resource %s cannot use both count and for_each", ra)
	}
	for _, nb := range b.Body.Blocks {
		if nb.Type != "lifecycle" {
			diags.addf(nb.Pos, "unsupported nested block %q in resource %s", nb.Type, ra)
			continue
		}
		for _, a := range nb.Body.Attributes {
			switch a.Name {
			case "create_before_destroy":
				r.Lifecycle.CreateBeforeDestroy = staticBool(a, diags)
			case "prevent_destroy":
				r.Lifecycle.PreventDestroy = staticBool(a, diags)
			case "ignore_changes":
				names, err := decodeNameList(a)
				if err != nil {
					*diags = append(*diags, err)
					continue
				}
				r.Lifecycle.IgnoreChanges = names
			default:
				diags.addf(a.Pos, "unsupported lifecycle argument %q", a.Name)
			}
		}
	}
	c.Resources[ra.String()] = r
}

func decodeDependsOn(a *syntax.Attribute) ([]addr.Resource, error) {
	tup, ok := a.Expr.(*syntax.TupleExpr)
	if !ok {
		return nil, &syntax.Error{Pos: a.Pos, Msg: "depends_on must be a list of resource references"}
	}
	var out []addr.Resource
	for _, e := range tup.Elems {
		tr, ok := e.(*syntax.TraversalExpr)
		if !ok || tr.Source != nil || len(tr.Steps) != 1 {
			return nil, &syntax.Error{Pos: e.Start(), Msg: "depends_on entries must be TYPE.NAME references"}
		}
		step, ok := tr.Steps[0].(syntax.AttrStep)
		if !ok {
			return nil, &syntax.Error{Pos: e.Start(), Msg: "depends_on entries must be TYPE.NAME references"}
		}
		out = append(out, addr.Resource{Type: tr.Root, Name: step.Name})
	}
	return out, nil
}

func decodeNameList(a *syntax.Attribute) ([]string, error) {
	tup, ok := a.Expr.(*syntax.TupleExpr)
	if !ok {
		return nil, &syntax.Error{Pos: a.Pos, Msg: a.Name + " must be a list of attribute names"}
	}
	var out []string
	for _, e := range tup.Elems {
		tr, ok := e.(*syntax.TraversalExpr)
		if !ok || tr.Source != nil || len(tr.Steps) != 0 {
			return nil, &syntax.Error{Pos: e.Start(), Msg: a.Name + " entries must be bare attribute names"}
		}
		out = append(out, tr.Root)
	}
	return out, nil
}

// SortedResources returns resources ordered by address.
func (c *Config) SortedResources() []*Resource {
	out := make([]*Resource, 0, len(c.Resources))
	for _, r := range c.Resources {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr.String() < out[j].Addr.String() })
	return out
}

// ProviderNames returns the providers used by resources or configured.
func (c *Config) ProviderNames() []string {
	set := map[string]bool{}
	for _, r := range c.Resources {
		set[r.Addr.Provider()] = true
	}
	for n := range c.Providers {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
