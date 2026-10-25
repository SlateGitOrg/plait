package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/archit2501/plait/internal/value"
)

const good = `
variable "env" {
  type        = string
  default     = "dev"
  description = "deployment environment"
}
variable "sizes" {
  type    = map(number)
  default = { a = "1" }
}
variable "zones" { type = list(string) }
variable "free" {}

provider "fakecloud" {
  endpoint = var.env
}

locals {
  prefix = "${var.env}-app"
  net_id = fakecloud_network.main.id
}

resource "fakecloud_network" "main" {
  name = local.prefix
  cidr = "10.0.0.0/16"
}

resource "fakecloud_instance" "web" {
  count      = length(var.zones)
  name       = "${local.prefix}-${count.index}"
  network_id = local.net_id
  depends_on = [local_file.marker]
  lifecycle {
    create_before_destroy = true
    prevent_destroy       = false
    ignore_changes        = [tags]
  }
}

resource "local_file" "marker" {
  for_each = toset(var.zones)
  path     = "${path.module}/${each.key}.txt"
  content  = each.value
}

output "ips" {
  value       = fakecloud_instance.web[*].private_ip
  description = "instance IPs"
}
`

func TestLoadGoodConfig(t *testing.T) {
	cfg, err := LoadFiles(".", map[string]string{"main.plait": good})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Variables["sizes"].Default.GoString() != "{ a = 1 }" {
		t.Fatalf("default not converted: %s", cfg.Variables["sizes"].Default.GoString())
	}
	if cfg.Variables["zones"].Default != nil || cfg.Variables["env"].Description == "" {
		t.Fatal("variable decode")
	}
	web := cfg.Resources["fakecloud_instance.web"]
	if web.Count == nil || !web.Lifecycle.CreateBeforeDestroy || web.Lifecycle.PreventDestroy ||
		strings.Join(web.Lifecycle.IgnoreChanges, ",") != "tags" || len(web.Attrs) != 2 {
		t.Fatalf("resource decode: %+v", web)
	}
	deps, err := cfg.ResourceDeps(web)
	if err != nil {
		t.Fatal(err)
	}
	var ds []string
	for _, d := range deps {
		ds = append(ds, d.String())
	}
	if strings.Join(ds, ",") != "fakecloud_network.main,local_file.marker" {
		t.Fatalf("deps through locals and depends_on: %v", ds)
	}
	g, _ := cfg.Graph()
	if !g.Has("provider.fakecloud") || strings.Join(g.DependsOn("output.ips"), ",") != "fakecloud_instance.web" {
		t.Fatalf("graph: %v", g.Edges())
	}
	if strings.Join(cfg.ProviderNames(), ",") != "fakecloud,local" {
		t.Fatal(cfg.ProviderNames())
	}
	if len(cfg.SortedResources()) != 3 || cfg.SortedResources()[0].Addr.String() != "fakecloud_instance.web" {
		t.Fatal("SortedResources")
	}
	if cfg.Outputs["ips"].Description != "instance IPs" {
		t.Fatal("output description")
	}
}

func TestConfigErrors(t *testing.T) {
	cases := map[string]string{
		`x = 1`: `top-level attribute`,
		`resource "a_b" "c" {}
resource "a_b" "c" {}`: `duplicate resource`,
		`resource "nounderscore" "c" {}`:                                            `prefixed with its provider`,
		`resource "a_b" {}`:                                                         `2 labels`,
		`resource "a_b" "c" { x = var.nope }`:                                       `undeclared input variable "nope"`,
		`resource "a_b" "c" { x = local.nope }`:                                     `undeclared local value`,
		`resource "a_b" "c" { x = a_b.missing.id }`:                                 `undeclared resource a_b.missing`,
		`resource "a_b" "c" { x = count.index }`:                                    `only valid in resources that set count`,
		`resource "a_b" "c" { count = count.index }`:                                `count.index is only valid`,
		`resource "a_b" "c" { x = each.key }`:                                       `for_each`,
		`resource "a_b" "c" { count = 1 x = 1 }`:                                    `expected newline`,
		`resource "a_b" "c" { x = var }`:                                            `must be followed by an attribute`,
		`resource "a_b" "c" { x = self.id }`:                                        `not supported`,
		`resource "a_b" "c" { x = path.nope }`:                                      `path object`,
		"resource \"a_b\" \"c\" {\n count = 1\n x = count.nope\n}":                  `only count attribute`,
		"resource \"a_b\" \"c\" {\n for_each = {}\n x = each.k\n}":                  `key and value`,
		"resource \"a_b\" \"c\" {\n count = 1\n for_each = {}\n}":                   `both count and for_each`,
		`resource "a_b" "c" { depends_on = a_b.d }`:                                 `list of resource references`,
		`resource "a_b" "c" { depends_on = [a_b] }`:                                 `TYPE.NAME`,
		`resource "a_b" "c" { depends_on = [a_b[0]] }`:                              `TYPE.NAME`,
		`resource "a_b" "c" { depends_on = [a_b.zzz] }`:                             `undeclared resource`,
		`resource "a_b" "c" { provider = x }`:                                       `aliases are not supported`,
		"resource \"a_b\" \"c\" {\n nested {}\n}":                                   `unsupported nested block`,
		"resource \"a_b\" \"c\" {\n lifecycle {\n x = 1\n }\n}":                     `unsupported lifecycle argument`,
		"resource \"a_b\" \"c\" {\n lifecycle {\n create_before_destroy = 1\n }\n}": `constant bool`,
		"resource \"a_b\" \"c\" {\n lifecycle {\n ignore_changes = x\n }\n}":        `list of attribute names`,
		"resource \"a_b\" \"c\" {\n lifecycle {\n ignore_changes = [a.b]\n }\n}":    `bare attribute names`,
		`resource "a_b" "c" { x = a_b.c.id }`:                                       `dependency cycle: a_b.c -> a_b.c`,
		"locals {\n a = local.b\n b = local.a\n}":                                   `dependency cycle: local.a -> local.b -> local.a`,
		"locals {\n a = 1\n}\nlocals {\n a = 2\n}":                                  `duplicate local`,
		`locals "x" {}`:                                                `no labels`,
		"locals {\n blk {}\n}":                                         `unexpected block`,
		`variable "v" { type = strin }`:                                `unknown type`,
		`variable "v" { type = set(string) }`:                          `unsupported type constructor`,
		`variable "v" { type = 1 }`:                                    `expected a type`,
		`variable "v" { default = var.x }`:                             `may not contain references`,
		`variable "v" { default = 1 / 0 }`:                             `division by zero`,
		`variable "v" { nope = 1 }`:                                    `unsupported argument`,
		"variable \"v\" {\n type = number\n default = \"x\"\n}":        `does not match type`,
		"variable \"v\" {}\nvariable \"v\" {}":                         `duplicate variable`,
		`variable "v" { description = 1 }`:                             `constant string`,
		`variable "9bad" {}`:                                           `invalid name`,
		`output "o" {}`:                                                `requires a value`,
		"output \"o\" {\n value = 1\n}\noutput \"o\" {\n value = 1\n}": `duplicate output`,
		"output \"o\" {\n value = 1\n bogus = 2\n}":                    `unsupported argument`,
		`provider "p" { x = a_b.c.id }`:                                `only reference variables`,
		`provider "p" { x = local.y }`:                                 `only reference variables`,
		"provider \"p\" {}\nprovider \"p\" {}":                         `duplicate provider`,
		`module "m" {}`:                                                `unsupported block type`,
		`resource "a_b" "c" { x = }`:                                   `expected expression`,
	}
	for src, want := range cases {
		_, err := LoadFiles(".", map[string]string{"t.plait": src})
		if err == nil {
			t.Errorf("%q: expected error containing %q", src, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q:\n  error %q\n  does not contain %q", src, err, want)
		}
	}
}

func TestLoadDirAndVariables(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("empty dir should fail")
	}
	write("vars.plait", `
variable "name" { type = string }
variable "n" {
  type    = number
  default = 1
}
variable "tags" { type = map(string) }
variable "anything" { default = "d" }
`)
	write("plait.plaitvars", "n = 2\ntags = { a = \"x\" }\n")
	extra := write("extra.plaitvars", "n = 3\n")
	cfg, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	vals, err := cfg.ResolveVariables(VarInputs{
		Env:      []string{"PLAIT_VAR_name=from-env", "PLAIT_VAR_undeclared=x", "OTHER=1", "PLAIT_VAR_anything={k = 1}"},
		VarFiles: []string{extra},
		Flags:    []string{"n=4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := value.Map(vals).GoString()
	want := `{ anything = { k = 1 }, n = 4, name = "from-env", tags = { a = "x" } }`
	if got != want {
		t.Fatalf("resolved:\n got %s\nwant %s", got, want)
	}
	vals, err = cfg.ResolveVariables(VarInputs{Env: []string{"PLAIT_VAR_name=x", "PLAIT_VAR_anything=not an expr ("}, Flags: []string{"tags={b = 2}"}})
	if err != nil {
		t.Fatal(err)
	}
	if vals["anything"].AsString() != "not an expr (" || vals["tags"].GoString() != `{ b = "2" }` {
		t.Fatalf("raw fallbacks: %v", vals)
	}

	errCases := []VarInputs{
		{},                                     // name is required
		{Flags: []string{"name"}},              // malformed flag
		{Flags: []string{"name=x", "zzz=1"}},   // undeclared
		{Flags: []string{"name=x", "n=abc"}},   // not a number
		{Flags: []string{"name=x", "n=var.y"}}, // not constant
		{Flags: []string{"name=x"}, VarFiles: []string{filepath.Join(dir, "missing.plaitvars")}},
		{Flags: []string{"name=x"}, VarFiles: []string{write("bad1.plaitvars", "blk {}\n")}},
		{Flags: []string{"name=x"}, VarFiles: []string{write("bad2.plaitvars", "zzz = 1\n")}},
		{Flags: []string{"name=x"}, VarFiles: []string{write("bad3.plaitvars", "n = var.q\n")}},
		{Flags: []string{"name=x"}, VarFiles: []string{write("bad4.plaitvars", "n = \"abc\"\n")}},
		{Flags: []string{"name=x"}, VarFiles: []string{write("bad5.plaitvars", "n = \n")}},
	}
	for i, in := range errCases {
		if _, err := cfg.ResolveVariables(in); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}
