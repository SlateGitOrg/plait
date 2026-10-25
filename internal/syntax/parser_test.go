package syntax

import (
	"strings"
	"testing"
)

const sample = `
# comment
// another comment
/* block
   comment */
variable "region" {
  default = "us-east-1"
}

resource "fakecloud_instance" "web" {
  count = 2
  name  = "web-${count.index}"
  tags = {
    env  = "dev"
    "k2" : 2,
  }
  list = [
    1,
    2,
  ]
  cond = var.x > 1 ? (
    "big"
  ) : "small"
  content = <<-EOT
    hello ${var.region}
      indented $${literal}
    EOT
  lifecycle {
    create_before_destroy = true
  }
}

locals {}
`

func TestParseFileStructure(t *testing.T) {
	f, err := ParseFile("main.plait", sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Body.Blocks) != 3 {
		t.Fatalf("want 3 blocks, got %d", len(f.Body.Blocks))
	}
	res := f.Body.Blocks[1]
	if res.Type != "resource" || strings.Join(res.Labels, ",") != "fakecloud_instance,web" {
		t.Fatalf("bad block header %s %v", res.Type, res.Labels)
	}
	names := []string{}
	for _, a := range res.Body.Attributes {
		names = append(names, a.Name)
	}
	if got := strings.Join(names, ","); got != "count,name,tags,list,cond,content" {
		t.Fatalf("attributes: %s", got)
	}
	if res.Body.Attr("missing") != nil || res.Body.Attr("count") == nil {
		t.Fatal("Attr lookup broken")
	}
	if len(res.Body.Blocks) != 1 || res.Body.Blocks[0].Type != "lifecycle" {
		t.Fatal("nested lifecycle block missing")
	}
	tmpl, ok := res.Body.Attr("content").Expr.(*TemplateExpr)
	if !ok {
		t.Fatalf("heredoc should be a template, got %T", res.Body.Attr("content").Expr)
	}
	first := tmpl.Parts[0].(*LiteralExpr).Val.AsString()
	if first != "hello " {
		t.Fatalf("heredoc dedent failed: %q", first)
	}
	last := tmpl.Parts[len(tmpl.Parts)-1].(*LiteralExpr).Val.AsString()
	if last != "\n  indented ${literal}\n" {
		t.Fatalf("heredoc tail: %q", last)
	}
	refs := References(res.Body.Attr("name").Expr)
	if len(refs) != 1 || strings.Join(refs[0].Parts, ".") != "count.index" {
		t.Fatalf("refs: %+v", refs)
	}
	if refs[0].Pos.Line != 12 {
		t.Fatalf("interpolation position: %v", refs[0].Pos)
	}
}

func TestReferencesSkipForBindings(t *testing.T) {
	e, err := ParseExpr("t", `[for k, v in res.a[*].id : "${k}${v}${local.x}" if v != data.y[idx.z]]`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range References(e) {
		got = append(got, strings.Join(r.Parts, "."))
	}
	want := "res.a,local.x,data.y,idx.z"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v want %s", got, want)
	}
	e, _ = ParseExpr("t", `f(x.a)[0].b`)
	refs := References(e)
	if len(refs) != 1 || strings.Join(refs[0].Parts, ".") != "x.a" {
		t.Fatalf("refs through call source: %+v", refs)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		`a = `:                          `expected expression`,
		`a = 1 b = 2`:                   `expected newline`,
		`a = 1` + "\n" + `a = 2`:        `duplicate attribute`,
		`resource "x" {`:                `to close block`,
		`resource "x" "${y}" {}`:        `block label`,
		`a = "unterminated`:             `unterminated string`,
		`a = "bad \q escape"`:           `invalid escape`,
		`a = "${x"`:                     `unterminated`,
		`a = <<EOT` + "\nno end\n":      `unterminated heredoc`,
		`a = <<EOT junk` + "\nx\nEOT\n": `newline`,
		`a = << ` + "\n":                `delimiter`,
		`a = @`:                         `unexpected character`,
		`/* never closed`:               `unterminated block comment`,
		`a = [1 2]`:                     `',' or ']'`,
		`a = f(1 2)`:                    `',' or ')'`,
		`a = f(x..., y)`:                `after '...'`,
		`a = {1 = 2}`:                   `object key`,
		`a = {k 2}`:                     `'=' or ':'`,
		`a = {k = 1 j = 2}`:             `after object item`,
		`a = [for 1 in x : y]`:          `iteration variable`,
		`a = [for x on y : x]`:          `'in'`,
		`a = {for x in y : x}`:          `'=>'`,
		`a = (1`:                        `close parenthesis`,
		`a = x.`:                        `attribute name`,
		`a = x[1`:                       `close index`,
		`a = c ? 1`:                     `conditional`,
		`a = "${1 2}"`:                  `end interpolation`,
		`a = [for x in y : x`:           `close for-expression`,
		`= 1`:                           `attribute or block`,
	}
	for src, want := range cases {
		_, err := ParseFile("bad.plait", src)
		if err == nil {
			t.Errorf("%q: expected error containing %q", src, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q does not contain %q", src, err, want)
		}
		if !strings.HasPrefix(err.Error(), "bad.plait:") {
			t.Errorf("%q: error lacks position: %q", src, err)
		}
	}
	if _, err := ParseExpr("e", "1 2"); err == nil {
		t.Error("trailing tokens in expression should fail")
	}
}

func TestNumbersAndEscapes(t *testing.T) {
	f, err := ParseFile("n", "a = 1.5e3\nb = \"tab\\tq\\\"nl\\n\\\\\"\nc = 2e\nd = \"\"\n")
	if err == nil {
		t.Fatalf("2e should not parse as a lone number followed by identifier: %+v", f)
	}
	f, err = ParseFile("n", "a = 1.5e3\nb = \"tab\\tq\\\"nl\\n\\\\\\r\"\nd = \"\"\ne = 1e-2\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Body.Attr("a").Expr.(*LiteralExpr).Val.GoString(); got != "1500" {
		t.Errorf("number: %s", got)
	}
	if got := f.Body.Attr("b").Expr.(*LiteralExpr).Val.AsString(); got != "tab\tq\"nl\n\\\r" {
		t.Errorf("escapes: %q", got)
	}
	if got := f.Body.Attr("d").Expr.(*LiteralExpr).Val.AsString(); got != "" {
		t.Errorf("empty string: %q", got)
	}
	if got := f.Body.Attr("e").Expr.(*LiteralExpr).Val.GoString(); got != "0.01" {
		t.Errorf("negative exponent: %s", got)
	}
	if (Pos{Line: 1, Col: 2}).String() != "1:2" {
		t.Error("Pos without file")
	}
	if tokEOF.String() != "end of file" || tokenKind(200).String() != "token" {
		t.Error("token names")
	}
}
