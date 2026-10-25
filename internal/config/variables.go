package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/archit2501/plait/internal/eval"
	"github.com/archit2501/plait/internal/syntax"
	"github.com/archit2501/plait/internal/value"
)

// VarsFileExt is the extension of variable definition files.
const VarsFileExt = ".plaitvars"

// AutoVarsFile is loaded automatically from the configuration directory when
// present.
const AutoVarsFile = "plait" + VarsFileExt

// EnvPrefix is the environment variable prefix for input variables
// (PLAIT_VAR_region=us-east-1).
const EnvPrefix = "PLAIT_VAR_"

// VarInputs gathers raw variable assignments from every source. Later
// sources override earlier ones: defaults < environment < plait.plaitvars <
// -var-file files (in order) < -var flags (in order).
type VarInputs struct {
	Env      []string // KEY=VALUE pairs, normally os.Environ()
	VarFiles []string
	Flags    []string // NAME=VALUE pairs from -var
}

// ResolveVariables computes the final value of every declared variable.
func (c *Config) ResolveVariables(in VarInputs) (map[string]value.Value, error) {
	out := map[string]value.Value{}
	for name, v := range c.Variables {
		if v.Default != nil {
			out[name] = *v.Default
		}
	}
	var diags Diagnostics
	setRaw := func(name, raw, source string) {
		v, ok := c.Variables[name]
		if !ok {
			diags = append(diags, fmt.Errorf("%s: value for undeclared variable %q", source, name))
			return
		}
		val, err := parseRawVar(v, raw)
		if err != nil {
			diags = append(diags, fmt.Errorf("%s: variable %q: %w", source, name, err))
			return
		}
		out[name] = val
	}
	envNames := make([]string, 0)
	envVals := map[string]string{}
	for _, kv := range in.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, EnvPrefix) {
			continue
		}
		name := strings.TrimPrefix(k, EnvPrefix)
		if _, declared := c.Variables[name]; !declared {
			continue // like Terraform, ignore env values for undeclared variables
		}
		envNames = append(envNames, name)
		envVals[name] = v
	}
	sort.Strings(envNames)
	for _, n := range envNames {
		setRaw(n, envVals[n], EnvPrefix+n)
	}
	files := in.VarFiles
	auto := filepath.Join(c.Dir, AutoVarsFile)
	if _, err := os.Stat(auto); err == nil {
		files = append([]string{auto}, files...)
	}
	for _, f := range files {
		vals, err := c.loadVarFile(f)
		if err != nil {
			diags = append(diags, err)
			continue
		}
		for k, v := range vals {
			out[k] = v
		}
	}
	for _, kv := range in.Flags {
		name, raw, ok := strings.Cut(kv, "=")
		if !ok {
			diags = append(diags, fmt.Errorf("-var %q: expected NAME=VALUE", kv))
			continue
		}
		setRaw(strings.TrimSpace(name), raw, "-var")
	}
	for _, name := range sortedVarNames(c.Variables) {
		if _, ok := out[name]; !ok {
			diags.addf(c.Variables[name].Pos, "no value for required variable %q (set it with -var, a %s file or %s%s)", name, VarsFileExt, EnvPrefix, name)
		}
	}
	if err := diags.err(); err != nil {
		return nil, err
	}
	return out, nil
}

func sortedVarNames(m map[string]*Variable) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// parseRawVar interprets a string from the command line or environment.
// String-typed variables take the raw text; other types parse it as a
// constant expression (e.g. 3, true, ["a"], {k = "v"}).
func parseRawVar(v *Variable, raw string) (value.Value, error) {
	if v.Type.Kind == value.TypeString {
		return value.String(raw), nil
	}
	expr, err := syntax.ParseExpr("<input>", raw)
	if err != nil || len(syntax.References(expr)) > 0 {
		if v.Type.Kind == value.TypeAny {
			return value.String(raw), nil
		}
		if err == nil {
			err = errors.New("value must be a constant")
		}
		return value.Null, err
	}
	val, err := eval.Eval(expr, &eval.Context{})
	if err != nil {
		return value.Null, err
	}
	return value.Convert(val, v.Type)
}

func (c *Config) loadVarFile(path string) (map[string]value.Value, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := syntax.ParseFile(filepath.Base(path), string(data))
	if err != nil {
		return nil, err
	}
	var diags Diagnostics
	for _, b := range f.Body.Blocks {
		diags.addf(b.Pos, "variable files may only contain attributes")
	}
	out := map[string]value.Value{}
	for _, a := range f.Body.Attributes {
		v, ok := c.Variables[a.Name]
		if !ok {
			diags.addf(a.Pos, "value for undeclared variable %q", a.Name)
			continue
		}
		if len(syntax.References(a.Expr)) > 0 {
			diags.addf(a.Pos, "variable values must be constants")
			continue
		}
		val, err := eval.Eval(a.Expr, &eval.Context{})
		if err == nil {
			val, err = value.Convert(val, v.Type)
		}
		if err != nil {
			diags.addf(a.Pos, "variable %q: %v", a.Name, err)
			continue
		}
		out[a.Name] = val
	}
	return out, diags.err()
}
