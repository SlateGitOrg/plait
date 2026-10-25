package eval

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SlateGitOrg/plait/internal/value"
)

// Function is a built-in function.
type Function struct {
	MinArgs int
	MaxArgs int // -1 for variadic
	// AllowUnknown lets collections containing unknown elements through to
	// Impl (top-level unknown arguments still short-circuit to unknown).
	AllowUnknown bool
	Impl         func(ctx *Context, args []value.Value) (value.Value, error)
}

var stdlib map[string]Function

// Stdlib returns the built-in function table.
func Stdlib() map[string]Function { return stdlib }

func init() {
	stdlib = map[string]Function{
		"abs":          {1, 1, false, numFn(math.Abs)},
		"ceil":         {1, 1, false, numFn(math.Ceil)},
		"floor":        {1, 1, false, numFn(math.Floor)},
		"max":          {1, -1, false, fnMinMax(true)},
		"min":          {1, -1, false, fnMinMax(false)},
		"length":       {1, 1, true, fnLength},
		"upper":        {1, 1, false, strFn(strings.ToUpper)},
		"lower":        {1, 1, false, strFn(strings.ToLower)},
		"trimspace":    {1, 1, false, strFn(strings.TrimSpace)},
		"replace":      {3, 3, false, fnReplace},
		"split":        {2, 2, false, fnSplit},
		"join":         {2, 2, false, fnJoin},
		"format":       {1, -1, false, fnFormat},
		"substr":       {3, 3, false, fnSubstr},
		"startswith":   {2, 2, false, fnHasAffix(strings.HasPrefix)},
		"endswith":     {2, 2, false, fnHasAffix(strings.HasSuffix)},
		"concat":       {0, -1, true, fnConcat},
		"merge":        {0, -1, true, fnMerge},
		"keys":         {1, 1, true, fnKeys},
		"values":       {1, 1, true, fnValues},
		"lookup":       {2, 3, true, fnLookup},
		"contains":     {2, 2, false, fnContains},
		"element":      {2, 2, true, fnElement},
		"toset":        {1, 1, false, fnToSet},
		"distinct":     {1, 1, false, fnToSet},
		"flatten":      {1, 1, true, fnFlatten},
		"zipmap":       {2, 2, true, fnZipmap},
		"tostring":     {1, 1, false, fnTo(value.Type{Kind: value.TypeString})},
		"tonumber":     {1, 1, false, fnTo(value.Type{Kind: value.TypeNumber})},
		"tobool":       {1, 1, false, fnTo(value.Type{Kind: value.TypeBool})},
		"range":        {1, 3, false, fnRange},
		"coalesce":     {1, -1, true, fnCoalesce},
		"jsonencode":   {1, 1, false, fnJSONEncode},
		"jsondecode":   {1, 1, false, fnJSONDecode},
		"sha256":       {1, 1, false, strFn(func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) })},
		"base64encode": {1, 1, false, strFn(func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) })},
		"file":         {1, 1, false, fnFile},
		"cidrsubnet":   {3, 3, false, fnCidrSubnet},
		"cidrhost":     {2, 2, false, fnCidrHost},
	}
}

func numFn(f func(float64) float64) func(*Context, []value.Value) (value.Value, error) {
	return func(_ *Context, a []value.Value) (value.Value, error) {
		n, err := a[0].ToNumber()
		if err != nil {
			return value.Null, err
		}
		return value.Number(f(n)), nil
	}
}

func strFn(f func(string) string) func(*Context, []value.Value) (value.Value, error) {
	return func(_ *Context, a []value.Value) (value.Value, error) {
		s, err := a[0].ToString()
		if err != nil {
			return value.Null, err
		}
		return value.String(f(s)), nil
	}
}

func strArgs(a []value.Value) ([]string, error) {
	out := make([]string, len(a))
	for i, v := range a {
		s, err := v.ToString()
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
		out[i] = s
	}
	return out, nil
}

func listArg(v value.Value, what string) ([]value.Value, error) {
	if v.Kind() != value.KindList {
		return nil, fmt.Errorf("%s must be a list, got %s", what, v.Kind())
	}
	return v.AsList(), nil
}

func fnMinMax(isMax bool) func(*Context, []value.Value) (value.Value, error) {
	return func(_ *Context, a []value.Value) (value.Value, error) {
		best := 0.0
		for i, v := range a {
			n, err := v.ToNumber()
			if err != nil {
				return value.Null, err
			}
			if i == 0 || (isMax && n > best) || (!isMax && n < best) {
				best = n
			}
		}
		return value.Number(best), nil
	}
}

func fnLength(_ *Context, a []value.Value) (value.Value, error) {
	switch a[0].Kind() {
	case value.KindList, value.KindMap, value.KindString:
		return value.Int(a[0].Len()), nil
	}
	return value.Null, fmt.Errorf("cannot take length of %s", a[0].Kind())
}

func fnReplace(_ *Context, a []value.Value) (value.Value, error) {
	s, err := strArgs(a)
	if err != nil {
		return value.Null, err
	}
	return value.String(strings.ReplaceAll(s[0], s[1], s[2])), nil
}

func fnSplit(_ *Context, a []value.Value) (value.Value, error) {
	s, err := strArgs(a)
	if err != nil {
		return value.Null, err
	}
	if s[1] == "" {
		return value.List(), nil
	}
	return value.Strings(strings.Split(s[1], s[0])...), nil
}

func fnJoin(_ *Context, a []value.Value) (value.Value, error) {
	sep, err := a[0].ToString()
	if err != nil {
		return value.Null, err
	}
	elems, err := listArg(a[1], "second argument")
	if err != nil {
		return value.Null, err
	}
	parts, err := strArgs(elems)
	if err != nil {
		return value.Null, err
	}
	return value.String(strings.Join(parts, sep)), nil
}

// fnFormat supports %s, %d, %v, %q and %%.
func fnFormat(_ *Context, a []value.Value) (value.Value, error) {
	spec, err := a[0].ToString()
	if err != nil {
		return value.Null, err
	}
	args := a[1:]
	var b strings.Builder
	next := 0
	for i := 0; i < len(spec); i++ {
		c := spec[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		// %[flags][width]verb with flags '-' (left-align) and '0' (zero-pad).
		i++
		left, zero, width := false, false, 0
		for ; i < len(spec) && (spec[i] == '-' || spec[i] == '0'); i++ {
			left = left || spec[i] == '-'
			zero = zero || spec[i] == '0'
		}
		for ; i < len(spec) && spec[i] >= '0' && spec[i] <= '9'; i++ {
			width = width*10 + int(spec[i]-'0')
		}
		if i >= len(spec) {
			return value.Null, errors.New("format string ends inside a verb")
		}
		verb := spec[i]
		if verb == '%' {
			b.WriteByte('%')
			continue
		}
		if next >= len(args) {
			return value.Null, fmt.Errorf("not enough arguments for format verb %%%c", verb)
		}
		piece, err := formatVerb(verb, args[next])
		if err != nil {
			return value.Null, err
		}
		next++
		if pad := width - len([]rune(piece)); pad > 0 {
			switch {
			case left:
				piece += strings.Repeat(" ", pad)
			case zero && verb == 'd' && strings.HasPrefix(piece, "-"):
				piece = "-" + strings.Repeat("0", pad) + piece[1:]
			case zero && verb == 'd':
				piece = strings.Repeat("0", pad) + piece
			default:
				piece = strings.Repeat(" ", pad) + piece
			}
		}
		b.WriteString(piece)
	}
	if next != len(args) {
		return value.Null, fmt.Errorf("too many arguments: format uses %d, got %d", next, len(args))
	}
	return value.String(b.String()), nil
}

func formatVerb(verb byte, arg value.Value) (string, error) {
	switch verb {
	case 's', 'v':
		if verb == 'v' && (arg.Kind() == value.KindList || arg.Kind() == value.KindMap) {
			return arg.GoString(), nil
		}
		return arg.ToString()
	case 'd':
		n, err := arg.ToNumber()
		if err != nil {
			return "", err
		}
		return value.FormatNumber(math.Trunc(n)), nil
	case 'q':
		s, err := arg.ToString()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%q", s), nil
	}
	return "", fmt.Errorf("unsupported format verb %%%c", verb)
}

func fnSubstr(_ *Context, a []value.Value) (value.Value, error) {
	s, err := a[0].ToString()
	if err != nil {
		return value.Null, err
	}
	off, err1 := a[1].ToNumber()
	n, err2 := a[2].ToNumber()
	if err1 != nil || err2 != nil {
		return value.Null, errors.New("offset and length must be numbers")
	}
	r := []rune(s)
	start := int(off)
	if start < 0 {
		start += len(r)
	}
	if start < 0 || start > len(r) {
		return value.Null, fmt.Errorf("offset %d out of range", int(off))
	}
	end := len(r)
	if n >= 0 && start+int(n) < end {
		end = start + int(n)
	}
	return value.String(string(r[start:end])), nil
}

func fnHasAffix(f func(string, string) bool) func(*Context, []value.Value) (value.Value, error) {
	return func(_ *Context, a []value.Value) (value.Value, error) {
		s, err := strArgs(a)
		if err != nil {
			return value.Null, err
		}
		return value.Bool(f(s[0], s[1])), nil
	}
}

func fnConcat(_ *Context, a []value.Value) (value.Value, error) {
	var out []value.Value
	for i, v := range a {
		l, err := listArg(v, fmt.Sprintf("argument %d", i+1))
		if err != nil {
			return value.Null, err
		}
		out = append(out, l...)
	}
	return value.List(out...), nil
}

func fnMerge(_ *Context, a []value.Value) (value.Value, error) {
	out := map[string]value.Value{}
	for i, v := range a {
		if v.IsNull() {
			continue
		}
		if v.Kind() != value.KindMap {
			return value.Null, fmt.Errorf("argument %d must be a map, got %s", i+1, v.Kind())
		}
		for k, e := range v.AsMap() {
			out[k] = e
		}
	}
	return value.Map(out), nil
}

func fnKeys(_ *Context, a []value.Value) (value.Value, error) {
	if a[0].Kind() != value.KindMap {
		return value.Null, fmt.Errorf("keys requires a map, got %s", a[0].Kind())
	}
	return value.Strings(a[0].Keys()...), nil
}

func fnValues(_ *Context, a []value.Value) (value.Value, error) {
	if a[0].Kind() != value.KindMap {
		return value.Null, fmt.Errorf("values requires a map, got %s", a[0].Kind())
	}
	var out []value.Value
	for _, k := range a[0].Keys() {
		v, _ := a[0].Get(k)
		out = append(out, v)
	}
	return value.List(out...), nil
}

func fnLookup(_ *Context, a []value.Value) (value.Value, error) {
	if a[0].Kind() != value.KindMap {
		return value.Null, fmt.Errorf("lookup requires a map, got %s", a[0].Kind())
	}
	k, err := a[1].ToString()
	if err != nil {
		return value.Null, err
	}
	if v, ok := a[0].Get(k); ok {
		return v, nil
	}
	if len(a) == 3 {
		return a[2], nil
	}
	return value.Null, fmt.Errorf("key %q not found and no default given", k)
}

func fnContains(_ *Context, a []value.Value) (value.Value, error) {
	l, err := listArg(a[0], "first argument")
	if err != nil {
		return value.Null, err
	}
	for _, e := range l {
		if e.Equal(a[1]) {
			return value.True, nil
		}
	}
	return value.False, nil
}

func fnElement(_ *Context, a []value.Value) (value.Value, error) {
	l, err := listArg(a[0], "first argument")
	if err != nil {
		return value.Null, err
	}
	if len(l) == 0 {
		return value.Null, errors.New("cannot use element on an empty list")
	}
	n, err := a[1].ToNumber()
	if err != nil {
		return value.Null, err
	}
	i := int(n) % len(l)
	if i < 0 {
		i += len(l)
	}
	return l[i], nil
}

func fnToSet(_ *Context, a []value.Value) (value.Value, error) {
	l, err := listArg(a[0], "argument")
	if err != nil {
		return value.Null, err
	}
	ss, err := strArgs(l)
	if err != nil {
		return value.Null, fmt.Errorf("sets of non-string values are not supported: %w", err)
	}
	return value.Strings(sortedUniqueStrings(ss)...), nil
}

func fnFlatten(_ *Context, a []value.Value) (value.Value, error) {
	var out []value.Value
	var walk func(v value.Value)
	walk = func(v value.Value) {
		if v.Kind() == value.KindList {
			for _, e := range v.AsList() {
				walk(e)
			}
			return
		}
		out = append(out, v)
	}
	if _, err := listArg(a[0], "argument"); err != nil {
		return value.Null, err
	}
	walk(a[0])
	return value.List(out...), nil
}

func fnZipmap(_ *Context, a []value.Value) (value.Value, error) {
	ks, err := listArg(a[0], "keys")
	if err != nil {
		return value.Null, err
	}
	vs, err := listArg(a[1], "values")
	if err != nil {
		return value.Null, err
	}
	if len(ks) != len(vs) {
		return value.Null, fmt.Errorf("keys has %d elements but values has %d", len(ks), len(vs))
	}
	keys, err := strArgs(ks)
	if err != nil {
		return value.Null, err
	}
	out := make(map[string]value.Value, len(keys))
	for i, k := range keys {
		out[k] = vs[i]
	}
	return value.Map(out), nil
}

func fnTo(t value.Type) func(*Context, []value.Value) (value.Value, error) {
	return func(_ *Context, a []value.Value) (value.Value, error) { return value.Convert(a[0], t) }
}

func fnRange(_ *Context, a []value.Value) (value.Value, error) {
	nums := make([]float64, len(a))
	for i, v := range a {
		n, err := v.ToNumber()
		if err != nil {
			return value.Null, err
		}
		nums[i] = n
	}
	start, end, step := 0.0, nums[0], 1.0
	if len(nums) >= 2 {
		start, end = nums[0], nums[1]
	}
	if len(nums) == 3 {
		step = nums[2]
	}
	if step == 0 {
		return value.Null, errors.New("step must not be zero")
	}
	if (end-start)/step > 1024 {
		return value.Null, errors.New("range would produce more than 1024 elements")
	}
	var out []value.Value
	for x := start; (step > 0 && x < end) || (step < 0 && x > end); x += step {
		out = append(out, value.Number(x))
	}
	return value.List(out...), nil
}

func fnCoalesce(_ *Context, a []value.Value) (value.Value, error) {
	for _, v := range a {
		if v.IsNull() {
			continue
		}
		if v.Kind() == value.KindString && v.AsString() == "" {
			continue
		}
		return v, nil
	}
	return value.Null, errors.New("no non-null, non-empty argument")
}

func fnJSONEncode(_ *Context, a []value.Value) (value.Value, error) {
	b, err := a[0].MarshalJSON()
	if err != nil {
		return value.Null, err
	}
	return value.String(string(b)), nil
}

func fnJSONDecode(_ *Context, a []value.Value) (value.Value, error) {
	s, err := a[0].ToString()
	if err != nil {
		return value.Null, err
	}
	var g any
	if err := json.Unmarshal([]byte(s), &g); err != nil {
		return value.Null, fmt.Errorf("invalid JSON: %w", err)
	}
	return value.FromGo(g)
}

func fnFile(ctx *Context, a []value.Value) (value.Value, error) {
	p, err := a[0].ToString()
	if err != nil {
		return value.Null, err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(ctx.BaseDir, p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return value.Null, err
	}
	return value.String(string(data)), nil
}

func parseIPv4Net(s string) (*net.IPNet, error) {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		return nil, err
	}
	if n.IP.To4() == nil {
		return nil, errors.New("only IPv4 prefixes are supported")
	}
	return n, nil
}

func ipToUint(ip net.IP) uint32 {
	v4 := ip.To4()
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
}

func uintToIP(u uint32) net.IP {
	return net.IPv4(byte(u>>24), byte(u>>16), byte(u>>8), byte(u))
}

// fnCidrSubnet computes a subnet address within a prefix, like Terraform's
// cidrsubnet(prefix, newbits, netnum).
func fnCidrSubnet(_ *Context, a []value.Value) (value.Value, error) {
	prefix, err := a[0].ToString()
	if err != nil {
		return value.Null, err
	}
	n, err := parseIPv4Net(prefix)
	if err != nil {
		return value.Null, err
	}
	newbits, err1 := a[1].ToNumber()
	netnum, err2 := a[2].ToNumber()
	if err1 != nil || err2 != nil {
		return value.Null, errors.New("newbits and netnum must be numbers")
	}
	ones, _ := n.Mask.Size()
	total := ones + int(newbits)
	if newbits < 0 || total > 32 {
		return value.Null, fmt.Errorf("cannot extend /%d prefix by %d bits", ones, int(newbits))
	}
	if netnum < 0 || netnum >= math.Pow(2, newbits) {
		return value.Null, fmt.Errorf("netnum %d does not fit in %d bits", int(netnum), int(newbits))
	}
	base := ipToUint(n.IP)
	sub := base | uint32(netnum)<<(32-total)
	return value.String(fmt.Sprintf("%s/%d", uintToIP(sub), total)), nil
}

func fnCidrHost(_ *Context, a []value.Value) (value.Value, error) {
	prefix, err := a[0].ToString()
	if err != nil {
		return value.Null, err
	}
	n, err := parseIPv4Net(prefix)
	if err != nil {
		return value.Null, err
	}
	host, err := a[1].ToNumber()
	if err != nil {
		return value.Null, err
	}
	ones, _ := n.Mask.Size()
	size := math.Pow(2, float64(32-ones))
	if host < 0 || host >= size {
		return value.Null, fmt.Errorf("host number %d out of range for /%d", int(host), ones)
	}
	return value.String(uintToIP(ipToUint(n.IP) + uint32(host)).String()), nil
}

// FuncNames returns the sorted names of the built-in functions.
func FuncNames() []string {
	names := make([]string, 0, len(stdlib))
	for n := range stdlib {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
