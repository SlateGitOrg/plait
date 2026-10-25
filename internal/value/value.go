// Package value implements the dynamic value model used by plait's expression
// language, plans and state.
//
// A Value is one of null, bool, number, string, list, map, or unknown. Unknown
// is the "known after apply" placeholder: it propagates through every
// operation so that a plan can describe attributes that only a provider can
// determine once the resource actually exists.
package value

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Kind enumerates the dynamic kinds of Value.
type Kind uint8

// Value kinds.
const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindString
	KindList
	KindMap
	KindUnknown
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindList:
		return "list"
	case KindMap:
		return "map"
	case KindUnknown:
		return "unknown"
	}
	return "invalid"
}

// Value is an immutable dynamic value. The zero Value is null.
type Value struct {
	kind Kind
	b    bool
	n    float64
	s    string
	l    []Value
	m    map[string]Value
}

// Null is the null value.
var Null = Value{}

// Unknown is a value that will only be known after apply.
var Unknown = Value{kind: KindUnknown}

// True and False are the boolean values.
var (
	True  = Value{kind: KindBool, b: true}
	False = Value{kind: KindBool, b: false}
)

// Bool returns a boolean value.
func Bool(b bool) Value { return Value{kind: KindBool, b: b} }

// Number returns a numeric value.
func Number(n float64) Value { return Value{kind: KindNumber, n: n} }

// Int returns a numeric value from an int.
func Int(n int) Value { return Value{kind: KindNumber, n: float64(n)} }

// String returns a string value.
func String(s string) Value { return Value{kind: KindString, s: s} }

// List returns a list value. The slice is copied.
func List(elems ...Value) Value {
	cp := make([]Value, len(elems))
	copy(cp, elems)
	return Value{kind: KindList, l: cp}
}

// Map returns a map value. The map is copied.
func Map(m map[string]Value) Value {
	cp := make(map[string]Value, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return Value{kind: KindMap, m: cp}
}

// Strings is a convenience constructor for a list of strings.
func Strings(ss ...string) Value {
	l := make([]Value, len(ss))
	for i, s := range ss {
		l[i] = String(s)
	}
	return Value{kind: KindList, l: l}
}

// Kind reports the value's kind.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports whether v is null.
func (v Value) IsNull() bool { return v.kind == KindNull }

// IsKnown reports whether v itself is known. Collections containing unknown
// elements are still known; use IsWhollyKnown for a deep check.
func (v Value) IsKnown() bool { return v.kind != KindUnknown }

// IsWhollyKnown reports whether v and every nested element are known.
func (v Value) IsWhollyKnown() bool {
	switch v.kind {
	case KindUnknown:
		return false
	case KindList:
		for _, e := range v.l {
			if !e.IsWhollyKnown() {
				return false
			}
		}
	case KindMap:
		for _, e := range v.m {
			if !e.IsWhollyKnown() {
				return false
			}
		}
	}
	return true
}

// AsBool returns the boolean payload. It panics if v is not a bool.
func (v Value) AsBool() bool {
	v.must(KindBool)
	return v.b
}

// AsNumber returns the numeric payload. It panics if v is not a number.
func (v Value) AsNumber() float64 {
	v.must(KindNumber)
	return v.n
}

// AsString returns the string payload. It panics if v is not a string.
func (v Value) AsString() string {
	v.must(KindString)
	return v.s
}

// AsList returns a copy of the list elements. It panics if v is not a list.
func (v Value) AsList() []Value {
	v.must(KindList)
	cp := make([]Value, len(v.l))
	copy(cp, v.l)
	return cp
}

// AsMap returns a copy of the map entries. It panics if v is not a map.
func (v Value) AsMap() map[string]Value {
	v.must(KindMap)
	cp := make(map[string]Value, len(v.m))
	for k, e := range v.m {
		cp[k] = e
	}
	return cp
}

// Len returns the number of elements of a list or map, or the length of a
// string in runes.
func (v Value) Len() int {
	switch v.kind {
	case KindList:
		return len(v.l)
	case KindMap:
		return len(v.m)
	case KindString:
		return len([]rune(v.s))
	}
	return 0
}

// Index returns element i of a list.
func (v Value) Index(i int) (Value, bool) {
	if v.kind != KindList || i < 0 || i >= len(v.l) {
		return Null, false
	}
	return v.l[i], true
}

// Get returns the map entry for key.
func (v Value) Get(key string) (Value, bool) {
	if v.kind != KindMap {
		return Null, false
	}
	e, ok := v.m[key]
	return e, ok
}

// Keys returns the sorted keys of a map value.
func (v Value) Keys() []string {
	if v.kind != KindMap {
		return nil
	}
	keys := make([]string, 0, len(v.m))
	for k := range v.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// With returns a copy of map v with key set to e. A null v is treated as an
// empty map.
func (v Value) With(key string, e Value) Value {
	m := map[string]Value{}
	if v.kind == KindMap {
		for k, x := range v.m {
			m[k] = x
		}
	}
	m[key] = e
	return Value{kind: KindMap, m: m}
}

func (v Value) must(k Kind) {
	if v.kind != k {
		panic(fmt.Sprintf("value: want %s, have %s", k, v.kind))
	}
}

// Equal reports deep equality. Unknown values are never equal to anything,
// including another unknown, because two unknowns may resolve differently.
func (v Value) Equal(o Value) bool {
	if v.kind == KindUnknown || o.kind == KindUnknown || v.kind != o.kind {
		return false
	}
	switch v.kind {
	case KindNull:
		return true
	case KindBool:
		return v.b == o.b
	case KindNumber:
		return v.n == o.n
	case KindString:
		return v.s == o.s
	case KindList:
		if len(v.l) != len(o.l) {
			return false
		}
		for i := range v.l {
			if !v.l[i].Equal(o.l[i]) {
				return false
			}
		}
		return true
	case KindMap:
		if len(v.m) != len(o.m) {
			return false
		}
		for k, e := range v.m {
			oe, ok := o.m[k]
			if !ok || !e.Equal(oe) {
				return false
			}
		}
		return true
	}
	return false
}

// FormatNumber renders a number the way the language prints it: integers
// without a fractional part, everything else in shortest form.
func FormatNumber(n float64) string {
	if n == math.Trunc(n) && math.Abs(n) < 1e15 {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'g', -1, 64)
}

// GoString renders v in HCL-like syntax, used by plan output and diagnostics.
func (v Value) GoString() string {
	var b strings.Builder
	v.render(&b)
	return b.String()
}

// String implements fmt.Stringer using the HCL-like rendering.
func (v Value) String() string { return v.GoString() }

func (v Value) render(b *strings.Builder) {
	switch v.kind {
	case KindNull:
		b.WriteString("null")
	case KindUnknown:
		b.WriteString("(known after apply)")
	case KindBool:
		b.WriteString(strconv.FormatBool(v.b))
	case KindNumber:
		b.WriteString(FormatNumber(v.n))
	case KindString:
		b.WriteString(strconv.Quote(v.s))
	case KindList:
		b.WriteByte('[')
		for i, e := range v.l {
			if i > 0 {
				b.WriteString(", ")
			}
			e.render(b)
		}
		b.WriteByte(']')
	case KindMap:
		if len(v.m) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{ ")
		for i, k := range v.Keys() {
			if i > 0 {
				b.WriteString(", ")
			}
			if isIdent(k) {
				b.WriteString(k)
			} else {
				b.WriteString(strconv.Quote(k))
			}
			b.WriteString(" = ")
			v.m[k].render(b)
		}
		b.WriteString(" }")
	}
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case i > 0 && (r == '-' || (r >= '0' && r <= '9')):
		default:
			return false
		}
	}
	return true
}

// Truthy converts v to a Go bool for conditionals. Only bools are accepted.
func (v Value) Truthy() (bool, error) {
	if v.kind != KindBool {
		return false, fmt.Errorf("expected bool, got %s", v.kind)
	}
	return v.b, nil
}

// ToString converts primitive values to their string form, as used by string
// interpolation.
func (v Value) ToString() (string, error) {
	switch v.kind {
	case KindString:
		return v.s, nil
	case KindNumber:
		return FormatNumber(v.n), nil
	case KindBool:
		return strconv.FormatBool(v.b), nil
	}
	return "", fmt.Errorf("cannot convert %s to string", v.kind)
}

// ToNumber converts a number or numeric string to float64.
func (v Value) ToNumber() (float64, error) {
	switch v.kind {
	case KindNumber:
		return v.n, nil
	case KindString:
		f, err := strconv.ParseFloat(strings.TrimSpace(v.s), 64)
		if err != nil {
			return 0, fmt.Errorf("cannot convert %q to number", v.s)
		}
		return f, nil
	}
	return 0, fmt.Errorf("cannot convert %s to number", v.kind)
}

// Walk calls fn for v and every nested value, depth first, with a path of
// map keys and list indexes.
func (v Value) Walk(fn func(path []string, e Value)) {
	v.walk(nil, fn)
}

func (v Value) walk(path []string, fn func([]string, Value)) {
	fn(path, v)
	switch v.kind {
	case KindList:
		for i, e := range v.l {
			e.walk(append(path[:len(path):len(path)], strconv.Itoa(i)), fn)
		}
	case KindMap:
		for _, k := range v.Keys() {
			v.m[k].walk(append(path[:len(path):len(path)], k), fn)
		}
	}
}
