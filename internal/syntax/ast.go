package syntax

import (
	"fmt"

	"github.com/SlateGitOrg/plait/internal/value"
)

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// File is a parsed configuration file.
type File struct {
	Name string
	Body *Body
}

// Body is a sequence of attributes and nested blocks.
type Body struct {
	Attributes []*Attribute
	Blocks     []*Block
	Pos        Pos
}

// Attribute is "name = expr".
type Attribute struct {
	Name string
	Expr Expr
	Pos  Pos
}

// Block is `type "label" ... { body }`.
type Block struct {
	Type   string
	Labels []string
	Body   *Body
	Pos    Pos
}

// Attr returns the attribute with the given name, or nil.
func (b *Body) Attr(name string) *Attribute {
	for _, a := range b.Attributes {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// Expr is an expression node.
type Expr interface {
	Start() Pos
	exprNode()
}

// LiteralExpr is a constant value.
type LiteralExpr struct {
	Val value.Value
	Pos Pos
}

// TemplateExpr is a string with interpolations. A template consisting of a
// single interpolation yields the interpolated value unchanged (not
// stringified), as in HCL.
type TemplateExpr struct {
	Parts []Expr
	Pos   Pos
}

// Step is one traversal step after the root of a reference.
type Step interface{ stepNode() }

// AttrStep is ".name".
type AttrStep struct{ Name string }

// IndexStep is "[expr]".
type IndexStep struct{ Key Expr }

// SplatStep is "[*]": the remaining steps apply to each element.
type SplatStep struct{}

func (AttrStep) stepNode()  {}
func (IndexStep) stepNode() {}
func (SplatStep) stepNode() {}

// TraversalExpr is a reference: either a root variable name (Source == nil)
// such as var.region or aws_instance.web[0].id, or steps applied to an
// arbitrary expression such as func()[0].
type TraversalExpr struct {
	Root   string
	Source Expr
	Steps  []Step
	Pos    Pos
}

// FuncCallExpr is name(args...). If ExpandFinal is true, the final argument
// is a list whose elements are passed as separate arguments.
type FuncCallExpr struct {
	Name        string
	Args        []Expr
	ExpandFinal bool
	Pos         Pos
}

// BinaryExpr is lhs op rhs.
type BinaryExpr struct {
	Op       string
	LHS, RHS Expr
	Pos      Pos
}

// UnaryExpr is "-x" or "!x".
type UnaryExpr struct {
	Op  string
	X   Expr
	Pos Pos
}

// ConditionalExpr is cond ? a : b.
type ConditionalExpr struct {
	Cond, True, False Expr
	Pos               Pos
}

// TupleExpr is [a, b, c].
type TupleExpr struct {
	Elems []Expr
	Pos   Pos
}

// ObjectItem is one key = value entry of an object constructor.
type ObjectItem struct {
	Key Expr
	Val Expr
}

// ObjectExpr is { k = v, ... }.
type ObjectExpr struct {
	Items []ObjectItem
	Pos   Pos
}

// ForExpr is [for k, v in coll : val if cond] or {for k, v in coll : key => val if cond}.
type ForExpr struct {
	KeyVar  string // empty when only a value variable is declared
	ValVar  string
	Coll    Expr
	KeyExpr Expr // non-nil for object results
	ValExpr Expr
	Cond    Expr
	Pos     Pos
}

// Start returns the position where the expression begins.
func (e *LiteralExpr) Start() Pos { return e.Pos }

// Start implements Expr.
func (e *TemplateExpr) Start() Pos { return e.Pos }

// Start implements Expr.
func (e *TraversalExpr) Start() Pos { return e.Pos }

// Start implements Expr.
func (e *FuncCallExpr) Start() Pos { return e.Pos }

// Start implements Expr.
func (e *BinaryExpr) Start() Pos { return e.Pos }

// Start implements Expr.
func (e *UnaryExpr) Start() Pos { return e.Pos }

// Start implements Expr.
func (e *ConditionalExpr) Start() Pos { return e.Pos }

// Start implements Expr.
func (e *TupleExpr) Start() Pos { return e.Pos }

// Start implements Expr.
func (e *ObjectExpr) Start() Pos { return e.Pos }

// Start implements Expr.
func (e *ForExpr) Start() Pos { return e.Pos }

func (*LiteralExpr) exprNode()     {}
func (*TemplateExpr) exprNode()    {}
func (*TraversalExpr) exprNode()   {}
func (*FuncCallExpr) exprNode()    {}
func (*BinaryExpr) exprNode()      {}
func (*UnaryExpr) exprNode()       {}
func (*ConditionalExpr) exprNode() {}
func (*TupleExpr) exprNode()       {}
func (*ObjectExpr) exprNode()      {}
func (*ForExpr) exprNode()         {}

// Reference is a static prefix of a root traversal: the root name followed by
// the leading attribute names, e.g. ["aws_instance", "web", "id"]. It is what
// dependency analysis needs.
type Reference struct {
	Parts []string
	Pos   Pos
}

// References returns every root-variable reference in e, excluding names
// bound by enclosing for-expressions.
func References(e Expr) []Reference {
	var out []Reference
	collectRefs(e, map[string]bool{}, &out)
	return out
}

func collectRefs(e Expr, bound map[string]bool, out *[]Reference) {
	switch x := e.(type) {
	case nil, *LiteralExpr:
	case *TemplateExpr:
		for _, p := range x.Parts {
			collectRefs(p, bound, out)
		}
	case *TraversalExpr:
		if x.Source != nil {
			collectRefs(x.Source, bound, out)
		} else if !bound[x.Root] {
			parts := []string{x.Root}
			for _, s := range x.Steps {
				as, ok := s.(AttrStep)
				if !ok {
					break
				}
				parts = append(parts, as.Name)
			}
			*out = append(*out, Reference{Parts: parts, Pos: x.Pos})
		}
		for _, s := range x.Steps {
			if is, ok := s.(IndexStep); ok {
				collectRefs(is.Key, bound, out)
			}
		}
	case *FuncCallExpr:
		for _, a := range x.Args {
			collectRefs(a, bound, out)
		}
	case *BinaryExpr:
		collectRefs(x.LHS, bound, out)
		collectRefs(x.RHS, bound, out)
	case *UnaryExpr:
		collectRefs(x.X, bound, out)
	case *ConditionalExpr:
		collectRefs(x.Cond, bound, out)
		collectRefs(x.True, bound, out)
		collectRefs(x.False, bound, out)
	case *TupleExpr:
		for _, el := range x.Elems {
			collectRefs(el, bound, out)
		}
	case *ObjectExpr:
		for _, it := range x.Items {
			collectRefs(it.Key, bound, out)
			collectRefs(it.Val, bound, out)
		}
	case *ForExpr:
		collectRefs(x.Coll, bound, out)
		inner := make(map[string]bool, len(bound)+2)
		for k := range bound {
			inner[k] = true
		}
		if x.KeyVar != "" {
			inner[x.KeyVar] = true
		}
		inner[x.ValVar] = true
		collectRefs(x.KeyExpr, inner, out)
		collectRefs(x.ValExpr, inner, out)
		collectRefs(x.Cond, inner, out)
	}
}
