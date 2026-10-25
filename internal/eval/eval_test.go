package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SlateGitOrg/plait/internal/syntax"
	"github.com/SlateGitOrg/plait/internal/value"
)

func testCtx() *Context {
	return &Context{Variables: map[string]value.Value{
		"var": value.Map(map[string]value.Value{
			"name":   value.String("web"),
			"count":  value.Int(3),
			"zones":  value.Strings("a", "b", "c"),
			"tags":   value.Map(map[string]value.Value{"env": value.String("dev"), "team": value.String("infra")}),
			"enable": value.True,
		}),
		"net": value.Map(map[string]value.Value{
			"main": value.Map(map[string]value.Value{"id": value.Unknown, "cidr": value.String("10.0.0.0/16")}),
		}),
		"srv": value.List(
			value.Map(map[string]value.Value{"id": value.String("i-1")}),
			value.Map(map[string]value.Value{"id": value.String("i-2")}),
		),
	}}
}

func evalString(t *testing.T, src string) (value.Value, error) {
	t.Helper()
	e, err := syntax.ParseExpr("test", src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return Eval(e, testCtx())
}

func TestEvalExpressions(t *testing.T) {
	cases := []struct{ src, want string }{
		{`1 + 2 * 3`, `7`},
		{`(1 + 2) * 3`, `9`},
		{`10 / 4`, `2.5`},
		{`7 % 3`, `1`},
		{`-var.count`, `-3`},
		{`!var.enable`, `false`},
		{`var.count > 2 && var.enable`, `true`},
		{`var.count < 2 || false`, `false`},
		{`var.count >= 3`, `true`},
		{`var.count <= 2`, `false`},
		{`var.name == "web"`, `true`},
		{`var.name != "web"`, `false`},
		{`var.enable ? "on" : "off"`, `"on"`},
		{`"${var.name}-${var.count}"`, `"web-3"`},
		{`"${var.zones}"`, `["a", "b", "c"]`},
		{`"literal $${not} interp"`, `"literal ${not} interp"`},
		{`var.zones[1]`, `"b"`},
		{`var.zones.0`, `"a"`},
		{`var.tags["env"]`, `"dev"`},
		{`var.tags.team`, `"infra"`},
		{`srv[*].id`, `["i-1", "i-2"]`},
		{`srv.*.id`, `["i-1", "i-2"]`},
		{`[for z in var.zones : upper(z)]`, `["A", "B", "C"]`},
		{`[for i, z in var.zones : "${i}${z}" if z != "b"]`, `["0a", "2c"]`},
		{`{for k, v in var.tags : v => k}`, `{ dev = "env", infra = "team" }`},
		{`{a = 1, "b" = 2, (var.name) = 3}`, `{ a = 1, b = 2, web = 3 }`},
		{`length(var.zones)`, `3`},
		{`length("héllo")`, `5`},
		{`join(",", var.zones)`, `"a,b,c"`},
		{`split(",", "x,y")`, `["x", "y"]`},
		{`format("%s-%d", "a", 7)`, `"a-7"`},
		{`format("%v|%q|%d%%", [1], "x", 2.9)`, `"[1]|\"x\"|2%"`},
		{`concat(["a"], ["b", "c"])`, `["a", "b", "c"]`},
		{`merge(var.tags, {env = "prod"})`, `{ env = "prod", team = "infra" }`},
		{`keys(var.tags)`, `["env", "team"]`},
		{`values(var.tags)`, `["dev", "infra"]`},
		{`lookup(var.tags, "nope", "dflt")`, `"dflt"`},
		{`contains(var.zones, "c")`, `true`},
		{`element(var.zones, 4)`, `"b"`},
		{`toset(["b", "a", "b"])`, `["a", "b"]`},
		{`flatten([[1, [2]], 3])`, `[1, 2, 3]`},
		{`zipmap(["a", "b"], [1, 2])`, `{ a = 1, b = 2 }`},
		{`tostring(5)`, `"5"`},
		{`tonumber("4.5")`, `4.5`},
		{`tobool("true")`, `true`},
		{`range(3)`, `[0, 1, 2]`},
		{`range(1, 7, 2)`, `[1, 3, 5]`},
		{`coalesce("", null, "x")`, `"x"`},
		{`jsonencode({b = [1, true], a = null})`, `"{\"a\":null,\"b\":[1,true]}"`},
		{`jsondecode("{\"k\": [1, \"v\"]}")`, `{ k = [1, "v"] }`},
		{`sha256("abc")`, `"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"`},
		{`base64encode("hi")`, `"aGk="`},
		{`cidrsubnet("10.0.0.0/16", 8, 2)`, `"10.0.2.0/24"`},
		{`cidrhost("10.0.1.0/24", 5)`, `"10.0.1.5"`},
		{`max(1, 9, 3)`, `9`},
		{`min(4, -2)`, `-2`},
		{`abs(-2) + ceil(1.2) + floor(1.8)`, `5`},
		{`upper("a") == "A" && lower("B") == "b"`, `true`},
		{`trimspace("  x ")`, `"x"`},
		{`replace("a-b-c", "-", "_")`, `"a_b_c"`},
		{`substr("hello", 1, 3)`, `"ell"`},
		{`substr("hello", -2, -1)`, `"lo"`},
		{`startswith("plait", "pl") && endswith("plait", "it")`, `true`},
		{`max(var.zones...)`, `ERR`},
		{`length(concat(var.zones, ["d"])...)`, `ERR`},
		{`min([3, 1]...)`, `1`},
	}
	for _, c := range cases {
		got, err := evalString(t, c.src)
		if c.want == "ERR" {
			if err == nil {
				t.Errorf("%s: expected error, got %s", c.src, got.GoString())
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.src, err)
			continue
		}
		if got.GoString() != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.src, got.GoString(), c.want)
		}
	}
}

func TestUnknownPropagation(t *testing.T) {
	cases := []string{
		`net.main.id`,
		`"prefix-${net.main.id}"`,
		`net.main.id == "x"`,
		`upper(net.main.id)`,
		`net.main.id != "" ? 1 : 2`,
		`[for x in net.main.id : x]`,
		`{(net.main.id) = 1}`,
		`-net.main.id`,
		`var.zones[net.main.id]`,
		`net.main.id.anything`,
		`max([1, 2]...) + length(net.main.id)`,
		`min(net.main.id...)`,
		`[for z in var.zones : z if net.main.id == z]`,
		`{for z in var.zones : (net.main.id) => z}`,
	}
	for _, src := range cases {
		got, err := evalString(t, src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got.IsKnown() {
			t.Errorf("%s: expected unknown, got %s", src, got.GoString())
		}
	}
	// Collections that merely contain unknowns stay known for functions
	// that allow it.
	got, err := evalString(t, `length([net.main.id, 1])`)
	if err != nil || got.GoString() != "2" {
		t.Errorf("length with unknown element: got %v, %v", got, err)
	}
	got, err = evalString(t, `[net.main.id, net.main.cidr]`)
	if err != nil || !got.IsKnown() || got.IsWhollyKnown() {
		t.Errorf("list with unknown element should be shallowly known: %v %v", got, err)
	}
}

func TestEvalErrors(t *testing.T) {
	cases := map[string]string{
		`nope.x`:                          `unknown reference "nope"`,
		`var.missing`:                     `no attribute "missing"`,
		`var.zones[7]`:                    `out of range`,
		`var.zones["x"]`:                  `whole number`,
		`var.tags["zzz"]`:                 `does not exist`,
		`var.name[0]`:                     `cannot index`,
		`var.name.x`:                      `cannot access attribute`,
		`1 / 0`:                           `division by zero`,
		`1 % 0`:                           `modulo by zero`,
		`"a" + 1`:                         `numeric operands`,
		`1 && true`:                       `bool operands`,
		`-"x"`:                            `requires a number`,
		`!1`:                              `requires a bool`,
		`1 ? 2 : 3`:                       `condition`,
		`nosuch(1)`:                       `unknown function`,
		`upper()`:                         `expects 1 argument`,
		`format("%s")`:                    `not enough arguments`,
		`format("%s", 1, 2)`:              `too many arguments`,
		`format("%x", 1)`:                 `unsupported format verb`,
		`"${null}"`:                       `null value`,
		`"${var.tags}x"`:                  `cannot interpolate`,
		`[for x in 5 : x]`:                `requires a list or map`,
		`{for x in var.zones : "k" => x}`: `duplicate key`,
		`lookup(var.tags, "nope")`:        `not found`,
		`element([], 0)`:                  `empty list`,
		`cidrsubnet("10.0.0.0/30", 8, 1)`: `cannot extend`,
		`cidrsubnet("10.0.0.0/16", 2, 9)`: `does not fit`,
		`cidrhost("10.0.0.0/30", 9)`:      `out of range`,
		`cidrsubnet("::/64", 8, 1)`:       `IPv4`,
		`range(1, 2, 0)`:                  `step`,
		`range(5000)`:                     `1024`,
		`jsondecode("{")`:                 `invalid JSON`,
		`coalesce(null, "")`:              `no non-null`,
		`zipmap(["a"], [])`:               `elements`,
		`toset([[1]])`:                    `not supported`,
		`keys([1])`:                       `requires a map`,
		`values(1)`:                       `requires a map`,
		`merge(1)`:                        `must be a map`,
		`concat(1)`:                       `must be a list`,
		`length(1)`:                       `cannot take length`,
		`srv.*.id[*]`:                     ``,
		`var.tags[*]`:                     `splat`,
		`tonumber("x")`:                   `cannot convert`,
		`tobool("yes")`:                   `bool required`,
		`substr("abc", 9, 1)`:             `out of range`,
		`file("does-not-exist.txt")`:      `no such file`,
	}
	for src, want := range cases {
		_, err := evalString(t, src)
		if want == "" {
			continue
		}
		if err == nil {
			t.Errorf("%s: expected error containing %q", src, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q does not contain %q", src, err, want)
		}
	}
}

func TestFileFunctionAndResolver(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "greeting.txt"), []byte("hi there"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := syntax.ParseExpr("t", `"${file("greeting.txt")} ${dyn.x}"`)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ctx := &Context{BaseDir: dir, Resolver: func(root string) (value.Value, bool, error) {
		calls++
		if root == "dyn" {
			return value.Map(map[string]value.Value{"x": value.String("!")}), true, nil
		}
		return value.Null, false, nil
	}}
	got, err := Eval(e, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AsString() != "hi there !" || calls != 1 {
		t.Fatalf("got %q after %d resolver calls", got.AsString(), calls)
	}
	if len(FuncNames()) < 30 {
		t.Fatalf("expected a sizeable stdlib, got %d functions", len(FuncNames()))
	}
}
