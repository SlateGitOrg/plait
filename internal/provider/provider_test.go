package provider

import (
	"strings"
	"testing"

	"github.com/SlateGitOrg/plait/internal/value"
)

var (
	tStr = value.Type{Kind: value.TypeString}
	tNum = value.Type{Kind: value.TypeNumber}
)

var rs = ResourceSchema{Attributes: []Attribute{
	{Name: "name", Type: tStr, Required: true},
	{Name: "zone", Type: tStr, Optional: true, ForceNew: true},
	{Name: "size", Type: tNum, Optional: true, Computed: true},
	{Name: "id", Type: tStr, Computed: true},
}}

func obj(kv ...any) value.Value {
	m := map[string]value.Value{}
	for i := 0; i < len(kv); i += 2 {
		v, ok := kv[i+1].(value.Value)
		if !ok {
			v, _ = value.FromGo(kv[i+1])
		}
		m[kv[i].(string)] = v
	}
	return value.Map(m)
}

func TestValidateConfig(t *testing.T) {
	got, err := ValidateConfig(rs, "x", obj("name", "a", "size", "3"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GoString() != `{ id = null, name = "a", size = 3, zone = null }` {
		t.Fatalf("normalized: %s", got.GoString())
	}
	if _, err := ValidateConfig(rs, "x", value.Null); err == nil || !strings.Contains(err.Error(), `missing required argument "name"`) {
		t.Fatalf("null config: %v", err)
	}
	_, err = ValidateConfig(rs, "x", obj("name", "a", "id", "i", "bogus", 1, "size", "big"))
	for _, want := range []string{`"id" is computed`, `unsupported argument "bogus"`, `argument "size"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
	if _, err := ValidateConfig(rs, "x", value.String("s")); err == nil {
		t.Fatal("non-object config accepted")
	}
}

func TestPlanFromSchema(t *testing.T) {
	cfg := obj("name", "a", "zone", "z1", "size", nil, "id", nil)
	create := PlanFromSchema(rs, value.Null, cfg)
	if create.Planned.GoString() != `{ id = (known after apply), name = "a", size = (known after apply), zone = "z1" }` || len(create.RequiresReplace) != 0 {
		t.Fatalf("create: %s %v", create.Planned.GoString(), create.RequiresReplace)
	}
	prior := obj("name", "a", "zone", "z1", "size", 2, "id", "i-1")
	same := PlanFromSchema(rs, prior, cfg)
	if !same.Planned.Equal(prior) {
		t.Fatalf("no-op plan differs: %s", same.Planned.GoString())
	}
	upd := PlanFromSchema(rs, prior, obj("name", "b", "zone", "z1", "size", 5, "id", nil))
	if upd.Planned.GoString() != `{ id = "i-1", name = "b", size = 5, zone = "z1" }` {
		t.Fatalf("update: %s", upd.Planned.GoString())
	}
	repl := PlanFromSchema(rs, prior, obj("name", "a", "zone", "z2", "size", nil, "id", nil))
	if len(repl.RequiresReplace) != 1 || repl.RequiresReplace[0] != "zone" {
		t.Fatalf("replace: %v", repl.RequiresReplace)
	}
	if id, _ := repl.Planned.Get("id"); id.IsKnown() {
		t.Fatal("replacement id must be unknown")
	}
	unk := PlanFromSchema(rs, prior, obj("name", "a", "zone", value.Unknown))
	if len(unk.RequiresReplace) != 1 {
		t.Fatal("an unknown force-new value must require replacement")
	}
}

func TestHelpers(t *testing.T) {
	r := Registry{"b": nil, "a": nil}
	if got := strings.Join(r.Names(), ","); got != "a,b" {
		t.Fatal(got)
	}
	if StringAttr(obj("a", "x", "n", 1), "a") != "x" || StringAttr(obj("n", 1), "n") != "" || StringAttr(value.Null, "a") != "" {
		t.Fatal("StringAttr")
	}
	if _, ok := rs.Attr("nope"); ok {
		t.Fatal("Attr")
	}
}
