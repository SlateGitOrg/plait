package value

import (
	"encoding/json"
	"strings"
	"testing"
)

func sample() Value {
	return Map(map[string]Value{
		"s":    String("x"),
		"n":    Number(1.5),
		"i":    Int(42),
		"b":    True,
		"null": Null,
		"list": List(Int(1), Strings("a", "b")),
		"m":    Map(map[string]Value{"k-1": String("v"), "two words": False}),
	})
}

func TestJSONRoundTrip(t *testing.T) {
	v := sample().With("u", Unknown)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"b":true,"i":42,"list":[1,["a","b"]],"m":{"k-1":"v","two words":false},"n":1.5,"null":null,"s":"x","u":{"__plait_unknown__":true}}`
	if string(b) != want {
		t.Fatalf("marshal:\n got %s\nwant %s", b, want)
	}
	var back Value
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	u, _ := back.Get("u")
	if u.IsKnown() {
		t.Fatal("unknown marker not decoded")
	}
	if !back.With("u", Null).Equal(sample().With("u", Null)) {
		t.Fatalf("round trip mismatch: %s", back.GoString())
	}
	if err := back.UnmarshalJSON([]byte(`{"a":`)); err == nil {
		t.Fatal("expected error for truncated JSON")
	}
}

func TestEqualityAndKnownness(t *testing.T) {
	if u := Unknown; u.Equal(Unknown) {
		t.Fatal("unknowns must never be equal")
	}
	if n := Null; !n.Equal(Null) || Null.Equal(False) {
		t.Fatal("null equality")
	}
	if List(Int(1)).Equal(List(Int(1), Int(2))) {
		t.Fatal("length mismatch")
	}
	if Map(map[string]Value{"a": Int(1)}).Equal(Map(map[string]Value{"b": Int(1)})) {
		t.Fatal("key mismatch")
	}
	nested := List(Unknown)
	if !nested.IsKnown() || nested.IsWhollyKnown() {
		t.Fatal("shallow vs deep known")
	}
	if Map(map[string]Value{"x": Unknown}).IsWhollyKnown() {
		t.Fatal("map with unknown")
	}
	if !sample().IsWhollyKnown() {
		t.Fatal("sample is fully known")
	}
}

func TestRenderAndAccessors(t *testing.T) {
	got := sample().GoString()
	want := `{ b = true, i = 42, list = [1, ["a", "b"]], m = { k-1 = "v", "two words" = false }, n = 1.5, null = null, s = "x" }`
	if got != want {
		t.Fatalf("render:\n got %s\nwant %s", got, want)
	}
	if Map(nil).String() != "{}" || Unknown.String() != "(known after apply)" {
		t.Fatal("special renders")
	}
	if FormatNumber(1e20) != "1e+20" || FormatNumber(-3) != "-3" {
		t.Fatal("number formatting")
	}
	l := List(Int(1), Int(2))
	if e, ok := l.Index(1); !ok || e.AsNumber() != 2 {
		t.Fatal("Index")
	}
	if _, ok := l.Index(5); ok {
		t.Fatal("Index out of range")
	}
	if _, ok := l.Get("x"); ok {
		t.Fatal("Get on list")
	}
	if l.Keys() != nil || Null.Len() != 0 || String("hé").Len() != 2 {
		t.Fatal("Keys/Len")
	}
	cp := l.AsList()
	cp[0] = Int(9)
	if first, _ := l.Index(0); first.AsNumber() != 1 {
		t.Fatal("AsList must copy")
	}
	m := sample().AsMap()
	delete(m, "s")
	if _, ok := sample().Get("s"); !ok {
		t.Fatal("AsMap must copy")
	}
	if !True.AsBool() || String("q").AsString() != "q" {
		t.Fatal("scalar accessors")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("AsBool on string must panic")
		}
	}()
	String("x").AsBool()
}

func TestConversions(t *testing.T) {
	for _, c := range []struct {
		v    Value
		want string
		err  bool
	}{
		{Int(3), "3", false}, {True, "true", false}, {String("s"), "s", false}, {Null, "", true},
	} {
		s, err := c.v.ToString()
		if (err != nil) != c.err || s != c.want {
			t.Errorf("ToString(%s) = %q, %v", c.v, s, err)
		}
	}
	if n, err := String(" 2.5 ").ToNumber(); err != nil || n != 2.5 {
		t.Error("numeric string")
	}
	if _, err := True.ToNumber(); err == nil {
		t.Error("bool to number should fail")
	}
	if _, err := Int(1).Truthy(); err == nil {
		t.Error("number is not truthy")
	}
	g, err := sample().ToGo()
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromGo(g)
	if err != nil || !back.Equal(sample()) {
		t.Fatalf("Go round trip: %v %v", back, err)
	}
	if _, err := List(Unknown).ToGo(); err == nil {
		t.Error("unknown ToGo should fail")
	}
	if _, err := FromGo(struct{}{}); err == nil {
		t.Error("unsupported Go type")
	}
	if v, _ := FromGo(json.Number("7")); v.AsNumber() != 7 {
		t.Error("json.Number")
	}
	if v, _ := FromGo(3); v.AsNumber() != 3 {
		t.Error("int")
	}
	if v, _ := FromGo(True); !v.AsBool() {
		t.Error("Value passthrough")
	}
}

func TestTypeConvert(t *testing.T) {
	listStr := Type{Kind: TypeList, Elem: &Type{Kind: TypeString}}
	mapNum := Type{Kind: TypeMap, Elem: &Type{Kind: TypeNumber}}
	if listStr.String() != "list(string)" || mapNum.String() != "map(number)" || (Type{Kind: TypeList}).String() != "list(any)" || AnyType.String() != "any" {
		t.Fatal("type names")
	}
	v, err := Convert(List(Int(1), True), listStr)
	if err != nil || v.GoString() != `["1", "true"]` {
		t.Fatalf("list(string): %v %v", v, err)
	}
	v, err = Convert(Map(map[string]Value{"a": String("2")}), mapNum)
	if err != nil || v.GoString() != `{ a = 2 }` {
		t.Fatalf("map(number): %v %v", v, err)
	}
	bad := []struct {
		v Value
		t Type
	}{
		{List(String("x")), Type{Kind: TypeList, Elem: &Type{Kind: TypeNumber}}},
		{String("x"), listStr},
		{Int(1), mapNum},
		{Map(map[string]Value{"a": String("z")}), mapNum},
		{String("yes"), Type{Kind: TypeBool}},
		{Int(1), Type{Kind: TypeBool}},
		{List(), Type{Kind: TypeString}},
		{Int(0), Type{Kind: 99}},
	}
	for _, b := range bad {
		if _, err := Convert(b.v, b.t); err == nil {
			t.Errorf("Convert(%s, %s) should fail", b.v, b.t)
		}
	}
	if v, _ := Convert(String("false"), Type{Kind: TypeBool}); v.AsBool() {
		t.Error("bool from string")
	}
	if v, _ := Convert(Unknown, Type{Kind: TypeNumber}); v.IsKnown() {
		t.Error("unknown passes through")
	}
	if v, _ := Convert(True, Type{Kind: TypeBool}); !v.AsBool() {
		t.Error("bool passthrough")
	}
}

func TestWalkAndWith(t *testing.T) {
	var paths []string
	sample().Walk(func(path []string, _ Value) { paths = append(paths, strings.Join(path, ".")) })
	joined := strings.Join(paths, "|")
	if !strings.Contains(joined, "list.1.0") || !strings.Contains(joined, "m.two words") {
		t.Fatalf("walk paths: %s", joined)
	}
	v := Null.With("a", Int(1))
	if v.Kind() != KindMap || v.Len() != 1 {
		t.Fatal("With on null")
	}
	for k := KindNull; k <= KindUnknown+1; k++ {
		if k.String() == "" {
			t.Fatal("kind name")
		}
	}
}
