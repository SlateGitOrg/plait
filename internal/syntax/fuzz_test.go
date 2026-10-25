package syntax

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseFile checks that the parser never panics and that every error it
// returns carries a position. The seed corpus is every configuration in the
// repository, so plain `go test` runs it as a regression test.
func FuzzParseFile(f *testing.F) {
	for _, pattern := range []string{"../../examples/*/*.plait", "../engine/testdata/golden/*/*.plait"} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if data, err := os.ReadFile(m); err == nil {
				f.Add(string(data))
			}
		}
	}
	f.Add(`a = "${x}" b = <<EOT
  y
  EOT
`)
	f.Add(`x = [for k, v in m : k => v if v]`)
	f.Fuzz(func(t *testing.T, src string) {
		file, err := ParseFile("fuzz.plait", src)
		if err != nil {
			se, ok := err.(*Error)
			if !ok || se.Pos.Line < 1 {
				t.Fatalf("error without position: %T %v", err, err)
			}
			return
		}
		for _, a := range file.Body.Attributes {
			_ = References(a.Expr)
		}
	})
}
