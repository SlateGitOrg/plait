// Package engine turns a configuration and a prior state into a plan, and a
// plan into converged infrastructure.
//
// The three operations mirror Terraform's model:
//
//   - Refresh asks each provider for the real state of every tracked object
//     (and resolves objects whose create was interrupted by a crash).
//   - Plan evaluates the configuration in dependency order, expands
//     count/for_each, asks providers to plan each instance and classifies
//     every change as create, update, replace or delete. Attributes only a
//     provider can determine are "known after apply".
//   - Apply builds an instance-level graph from the plan, walks it in
//     parallel, re-evaluates each instance's configuration with the real
//     values of its dependencies, checks the provider's final plan against
//     the reviewed one, and persists state after every step.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/archit2501/plait/internal/addr"
	"github.com/archit2501/plait/internal/config"
	"github.com/archit2501/plait/internal/eval"
	"github.com/archit2501/plait/internal/provider"
	"github.com/archit2501/plait/internal/value"
)

// DefaultParallelism is the default number of concurrent provider operations.
const DefaultParallelism = 4

// Event describes one operation on one instance, for progress reporting.
type Event struct {
	Addr    addr.Instance
	Op      Op
	Deposed bool
}

// Op is a single provider operation performed during apply or refresh.
type Op string

// Operations reported through Hooks.
const (
	OpCreate  Op = "create"
	OpUpdate  Op = "update"
	OpDestroy Op = "destroy"
	OpRefresh Op = "refresh"
)

// Hooks receive progress callbacks. They may be called concurrently from
// several goroutines; implementations must be safe for that.
type Hooks struct {
	Pre  func(ev Event)
	Post func(ev Event, result value.Value, err error, elapsed time.Duration)
}

func (h Hooks) pre(ev Event) {
	if h.Pre != nil {
		h.Pre(ev)
	}
}

func (h Hooks) post(ev Event, result value.Value, err error, start time.Time) {
	if h.Post != nil {
		h.Post(ev, result, err, time.Since(start))
	}
}

// Engine plans and applies one configuration.
type Engine struct {
	Config      *config.Config
	Vars        map[string]value.Value
	Registry    provider.Registry
	Parallelism int
	Log         *slog.Logger
	Hooks       Hooks

	mu        sync.Mutex
	providers map[string]provider.Provider
}

// New returns an engine for a loaded configuration and resolved variables.
func New(cfg *config.Config, vars map[string]value.Value, reg provider.Registry) *Engine {
	return &Engine{Config: cfg, Vars: vars, Registry: reg, Parallelism: DefaultParallelism, Log: slog.Default()}
}

func (e *Engine) parallelism() int {
	if e.Parallelism < 1 {
		return 1
	}
	return e.Parallelism
}

func (e *Engine) log() *slog.Logger {
	if e.Log == nil {
		return slog.Default()
	}
	return e.Log
}

// Provider returns the configured provider instance for name, creating and
// configuring it on first use.
func (e *Engine) Provider(ctx context.Context, name string) (provider.Provider, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.providers[name]; ok {
		return p, nil
	}
	factory, ok := e.Registry[name]
	if !ok {
		return nil, fmt.Errorf("no provider named %q is available (have: %v)", name, e.Registry.Names())
	}
	p := factory()
	cfgVal := value.Null
	if pc, ok := e.Config.Providers[name]; ok {
		ectx := &eval.Context{Variables: map[string]value.Value{"var": value.Map(e.Vars), "path": e.pathValue()}, BaseDir: e.Config.Dir}
		m := map[string]value.Value{}
		for _, a := range pc.Attrs {
			v, err := eval.Eval(a.Expr, ectx)
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
			m[a.Name] = v
		}
		cfgVal = value.Map(m)
	}
	norm, err := provider.ValidateConfig(p.Schema().Provider, "provider "+name, cfgVal)
	if err != nil {
		return nil, err
	}
	if err := p.Configure(ctx, norm); err != nil {
		return nil, fmt.Errorf("configuring provider %q: %w", name, err)
	}
	if e.providers == nil {
		e.providers = map[string]provider.Provider{}
	}
	e.providers[name] = p
	e.log().Debug("provider configured", "provider", name)
	return p, nil
}

// resourceSchema returns the provider and schema for a resource type.
func (e *Engine) resourceSchema(ctx context.Context, ra addr.Resource) (provider.Provider, provider.ResourceSchema, error) {
	p, err := e.Provider(ctx, ra.Provider())
	if err != nil {
		return nil, provider.ResourceSchema{}, err
	}
	rs, ok := p.Schema().Resources[ra.Type]
	if !ok {
		return nil, provider.ResourceSchema{}, fmt.Errorf("provider %q has no resource type %q", ra.Provider(), ra.Type)
	}
	return p, rs, nil
}

// pathValue is the value of the "path" object. Relative paths in plait are
// always relative to the configuration directory, so path.module is ".".
func (e *Engine) pathValue() value.Value {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return value.Map(map[string]value.Value{"module": value.String("."), "cwd": value.String(cwd)})
}

// resourceDeps returns the resource addresses r depends on, as strings.
func (e *Engine) resourceDeps(r *config.Resource) ([]string, error) {
	deps, err := e.Config.ResourceDeps(r)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(deps))
	for i, d := range deps {
		out[i] = d.String()
	}
	sort.Strings(out)
	return out, nil
}
