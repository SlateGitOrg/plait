package value

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// unknownMarker is the JSON encoding of an unknown value. It only appears in
// saved plans; state files never contain unknowns.
const unknownMarker = "__plait_unknown__"

// MarshalJSON encodes v as JSON. Unknown values encode as
// {"__plait_unknown__": true}; map keys are emitted in sorted order so output
// is deterministic.
func (v Value) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	if err := v.encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (v Value) encode(buf *bytes.Buffer) error {
	switch v.kind {
	case KindNull:
		buf.WriteString("null")
	case KindUnknown:
		buf.WriteString(`{"` + unknownMarker + `":true}`)
	case KindBool, KindString:
		var x any = v.s
		if v.kind == KindBool {
			x = v.b
		}
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	case KindNumber:
		buf.WriteString(FormatNumber(v.n))
	case KindList:
		buf.WriteByte('[')
		for i, e := range v.l {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := e.encode(buf); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case KindMap:
		keys := make([]string, 0, len(v.m))
		for k := range v.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := v.m[k].encode(buf); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	}
	return nil
}

// UnmarshalJSON decodes JSON into v, recognising the unknown marker.
func (v *Value) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	out, err := decode(dec)
	if err != nil {
		return err
	}
	*v = out
	return nil
}

func decode(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return Null, err
	}
	switch t := tok.(type) {
	case nil:
		return Null, nil
	case bool:
		return Bool(t), nil
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return Null, fmt.Errorf("invalid number %s: %w", t, err)
		}
		return Number(f), nil
	case string:
		return String(t), nil
	case json.Delim:
		switch t {
		case '[':
			var elems []Value
			for dec.More() {
				e, err := decode(dec)
				if err != nil {
					return Null, err
				}
				elems = append(elems, e)
			}
			if _, err := dec.Token(); err != nil {
				return Null, err
			}
			return Value{kind: KindList, l: append([]Value{}, elems...)}, nil
		case '{':
			m := map[string]Value{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return Null, err
				}
				k, ok := kt.(string)
				if !ok {
					return Null, fmt.Errorf("invalid object key %v", kt)
				}
				e, err := decode(dec)
				if err != nil {
					return Null, err
				}
				m[k] = e
			}
			if _, err := dec.Token(); err != nil {
				return Null, err
			}
			if len(m) == 1 {
				if mk, ok := m[unknownMarker]; ok && mk.kind == KindBool && mk.b {
					return Unknown, nil
				}
			}
			return Value{kind: KindMap, m: m}, nil
		}
	}
	return Null, fmt.Errorf("unexpected JSON token %v", tok)
}

// FromGo converts a value produced by encoding/json (map[string]any, []any,
// float64, string, bool, nil) into a Value.
func FromGo(x any) (Value, error) {
	switch t := x.(type) {
	case nil:
		return Null, nil
	case bool:
		return Bool(t), nil
	case float64:
		return Number(t), nil
	case int:
		return Int(t), nil
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return Null, err
		}
		return Number(f), nil
	case string:
		return String(t), nil
	case []any:
		l := make([]Value, len(t))
		for i, e := range t {
			v, err := FromGo(e)
			if err != nil {
				return Null, err
			}
			l[i] = v
		}
		return Value{kind: KindList, l: l}, nil
	case map[string]any:
		m := make(map[string]Value, len(t))
		for k, e := range t {
			v, err := FromGo(e)
			if err != nil {
				return Null, err
			}
			m[k] = v
		}
		return Value{kind: KindMap, m: m}, nil
	case Value:
		return t, nil
	}
	return Null, fmt.Errorf("unsupported Go type %T", x)
}

// ToGo converts v into plain Go values suitable for encoding/json. Unknown
// values are rejected.
func (v Value) ToGo() (any, error) {
	switch v.kind {
	case KindNull:
		return nil, nil
	case KindBool:
		return v.b, nil
	case KindNumber:
		return v.n, nil
	case KindString:
		return v.s, nil
	case KindList:
		out := make([]any, len(v.l))
		for i, e := range v.l {
			g, err := e.ToGo()
			if err != nil {
				return nil, err
			}
			out[i] = g
		}
		return out, nil
	case KindMap:
		out := make(map[string]any, len(v.m))
		for k, e := range v.m {
			g, err := e.ToGo()
			if err != nil {
				return nil, err
			}
			out[k] = g
		}
		return out, nil
	}
	return nil, fmt.Errorf("cannot convert unknown value to a concrete value")
}
