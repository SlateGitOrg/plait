// Package addr defines resource and instance addresses such as
// fakecloud_instance.web and fakecloud_instance.web["blue"].
package addr

import (
	"fmt"
	"strconv"
	"strings"
)

// Resource identifies a resource block: TYPE.NAME.
type Resource struct {
	Type string
	Name string
}

func (r Resource) String() string { return r.Type + "." + r.Name }

// Provider returns the provider name implied by the resource type: the
// prefix before the first underscore (local_file -> local).
func (r Resource) Provider() string {
	if i := strings.IndexByte(r.Type, '_'); i > 0 {
		return r.Type[:i]
	}
	return r.Type
}

// KeyKind distinguishes instance key forms.
type KeyKind uint8

// Instance key kinds.
const (
	NoKey     KeyKind = iota // single instance, no count/for_each
	IntKey                   // count index
	StringKey                // for_each key
)

// Key is an instance key.
type Key struct {
	Kind KeyKind
	Int  int
	Str  string
}

// IntK returns a count index key.
func IntK(i int) Key { return Key{Kind: IntKey, Int: i} }

// StrK returns a for_each key.
func StrK(s string) Key { return Key{Kind: StringKey, Str: s} }

func (k Key) String() string {
	switch k.Kind {
	case IntKey:
		return "[" + strconv.Itoa(k.Int) + "]"
	case StringKey:
		return "[" + strconv.Quote(k.Str) + "]"
	}
	return ""
}

// Instance identifies one instance of a resource.
type Instance struct {
	Resource
	Key Key
}

func (i Instance) String() string { return i.Resource.String() + i.Key.String() }

// Less orders instances by type, name, then key (integers numerically).
func Less(a, b Instance) bool {
	if a.Type != b.Type {
		return a.Type < b.Type
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Key.Kind != b.Key.Kind {
		return a.Key.Kind < b.Key.Kind
	}
	if a.Key.Kind == IntKey {
		return a.Key.Int < b.Key.Int
	}
	return a.Key.Str < b.Key.Str
}

// ParseInstance parses TYPE.NAME, TYPE.NAME[N] or TYPE.NAME["key"].
func ParseInstance(s string) (Instance, error) {
	base, keyPart := s, ""
	if i := strings.IndexByte(s, '['); i >= 0 {
		if !strings.HasSuffix(s, "]") {
			return Instance{}, fmt.Errorf("invalid address %q: unterminated index", s)
		}
		base, keyPart = s[:i], s[i+1:len(s)-1]
	}
	dot := strings.IndexByte(base, '.')
	if dot <= 0 || dot == len(base)-1 || strings.Count(base, ".") != 1 {
		return Instance{}, fmt.Errorf("invalid address %q: want TYPE.NAME", s)
	}
	inst := Instance{Resource: Resource{Type: base[:dot], Name: base[dot+1:]}}
	switch {
	case keyPart == "" && strings.Contains(s, "["):
		return Instance{}, fmt.Errorf("invalid address %q: empty index", s)
	case keyPart == "":
	case strings.HasPrefix(keyPart, `"`):
		str, err := strconv.Unquote(keyPart)
		if err != nil {
			return Instance{}, fmt.Errorf("invalid address %q: bad string key", s)
		}
		inst.Key = StrK(str)
	default:
		n, err := strconv.Atoi(keyPart)
		if err != nil || n < 0 {
			return Instance{}, fmt.Errorf("invalid address %q: bad index", s)
		}
		inst.Key = IntK(n)
	}
	return inst, nil
}
