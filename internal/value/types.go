package value

import (
	"fmt"
	"strconv"
)

// TypeKind enumerates the type constraints a variable may declare.
type TypeKind uint8

// Type constraint kinds.
const (
	TypeAny TypeKind = iota
	TypeString
	TypeNumber
	TypeBool
	TypeList
	TypeMap
)

// Type is a type constraint such as string, list(number) or map(any).
type Type struct {
	Kind TypeKind
	Elem *Type // element type for list and map; nil means any
}

// AnyType accepts every value unchanged.
var AnyType = Type{Kind: TypeAny}

func (t Type) String() string {
	switch t.Kind {
	case TypeString:
		return "string"
	case TypeNumber:
		return "number"
	case TypeBool:
		return "bool"
	case TypeList, TypeMap:
		name := "list"
		if t.Kind == TypeMap {
			name = "map"
		}
		elem := "any"
		if t.Elem != nil {
			elem = t.Elem.String()
		}
		return name + "(" + elem + ")"
	}
	return "any"
}

// Convert converts v to satisfy t, applying the same safe conversions as
// HCL: numbers and bools convert to strings, numeric strings convert to
// numbers, and "true"/"false" convert to bools. Null and unknown pass
// through unchanged.
func Convert(v Value, t Type) (Value, error) {
	if v.kind == KindNull || v.kind == KindUnknown {
		return v, nil
	}
	switch t.Kind {
	case TypeAny:
		return v, nil
	case TypeString:
		s, err := v.ToString()
		if err != nil {
			return Null, fmt.Errorf("string required, got %s", v.kind)
		}
		return String(s), nil
	case TypeNumber:
		n, err := v.ToNumber()
		if err != nil {
			return Null, fmt.Errorf("number required: %w", err)
		}
		return Number(n), nil
	case TypeBool:
		switch {
		case v.kind == KindBool:
			return v, nil
		case v.kind == KindString:
			b, err := strconv.ParseBool(v.s)
			if err != nil || (v.s != "true" && v.s != "false") {
				return Null, fmt.Errorf("bool required, got %q", v.s)
			}
			return Bool(b), nil
		}
		return Null, fmt.Errorf("bool required, got %s", v.kind)
	case TypeList:
		if v.kind != KindList {
			return Null, fmt.Errorf("list required, got %s", v.kind)
		}
		elem := AnyType
		if t.Elem != nil {
			elem = *t.Elem
		}
		out := make([]Value, len(v.l))
		for i, e := range v.l {
			c, err := Convert(e, elem)
			if err != nil {
				return Null, fmt.Errorf("element %d: %w", i, err)
			}
			out[i] = c
		}
		return Value{kind: KindList, l: out}, nil
	case TypeMap:
		if v.kind != KindMap {
			return Null, fmt.Errorf("map required, got %s", v.kind)
		}
		elem := AnyType
		if t.Elem != nil {
			elem = *t.Elem
		}
		out := make(map[string]Value, len(v.m))
		for k, e := range v.m {
			c, err := Convert(e, elem)
			if err != nil {
				return Null, fmt.Errorf("element %q: %w", k, err)
			}
			out[k] = c
		}
		return Value{kind: KindMap, m: out}, nil
	}
	return Null, fmt.Errorf("unsupported type %s", t)
}
