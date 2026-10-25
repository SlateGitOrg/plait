package addr

import (
	"sort"
	"strings"
	"testing"
)

func TestParseAndString(t *testing.T) {
	for _, s := range []string{
		`local_file.a`, `fakecloud_instance.web[3]`, `fakecloud_bucket.b["logs.v2"]`,
	} {
		inst, err := ParseInstance(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if inst.String() != s {
			t.Fatalf("round trip %s -> %s", s, inst)
		}
	}
	for _, bad := range []string{`nodot`, `a.b.c`, `a.`, `.b`, `a.b[`, `a.b[]`, `a.b[-1]`, `a.b[x]`, `a.b["x]`} {
		if _, err := ParseInstance(bad); err == nil {
			t.Errorf("%s should not parse", bad)
		}
	}
	if (Resource{Type: "fakecloud_network"}).Provider() != "fakecloud" || (Resource{Type: "plain"}).Provider() != "plain" {
		t.Fatal("provider prefix")
	}
}

func TestLessOrdering(t *testing.T) {
	var insts []Instance
	for _, s := range []string{`b.x`, `a.y[10]`, `a.y[2]`, `a.y["k"]`, `a.x`, `a.y`} {
		i, _ := ParseInstance(s)
		insts = append(insts, i)
	}
	sort.Slice(insts, func(i, j int) bool { return Less(insts[i], insts[j]) })
	var out []string
	for _, i := range insts {
		out = append(out, i.String())
	}
	if got := strings.Join(out, " "); got != `a.x a.y a.y[2] a.y[10] a.y["k"] b.x` {
		t.Fatalf("order: %s", got)
	}
}
