// Package workspace wires configuration loading, variables, the state store
// and its lock, providers and the engine into the operations the CLI
// exposes. Every operation that reads state for planning or writes it holds
// the state lock for its whole duration.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/SlateGitOrg/plait/internal/config"
	"github.com/SlateGitOrg/plait/internal/engine"
	"github.com/SlateGitOrg/plait/internal/provider"
	"github.com/SlateGitOrg/plait/internal/provider/cloudprov"
	"github.com/SlateGitOrg/plait/internal/provider/localfs"
	"github.com/SlateGitOrg/plait/internal/state"
	"github.com/SlateGitOrg/plait/internal/value"
)

// DefaultStateFile is the state file name inside the configuration
// directory.
const DefaultStateFile = "plait.state.json"

// Options configures a workspace.
type Options struct {
	Dir         string // configuration directory
	StatePath   string // default: <Dir>/plait.state.json
	Vars        config.VarInputs
	Parallelism int
	LockTimeout time.Duration
	Log         *slog.Logger
	Hooks       engine.Hooks
	// Registry overrides the built-in providers (tests use this).
	Registry provider.Registry
}

// Workspace is a configuration directory plus its state.
type Workspace struct {
	opts  Options
	Store *state.Store
}

// Open validates options and returns a workspace.
func Open(opts Options) (*Workspace, error) {
	if opts.Dir == "" {
		opts.Dir = "."
	}
	if opts.StatePath == "" {
		opts.StatePath = filepath.Join(opts.Dir, DefaultStateFile)
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Registry == nil {
		opts.Registry = DefaultRegistry(opts.Dir)
	}
	return &Workspace{opts: opts, Store: &state.Store{Path: opts.StatePath}}, nil
}

// DefaultRegistry returns the built-in providers. The local provider's
// relative paths resolve against the configuration directory.
func DefaultRegistry(dir string) provider.Registry {
	return provider.Registry{
		localfs.Name:   func() provider.Provider { return localfs.NewAt(dir) },
		cloudprov.Name: cloudprov.New,
	}
}

// Load parses the configuration and resolves variables.
func (w *Workspace) Load() (*config.Config, map[string]value.Value, error) {
	cfg, err := config.LoadDir(w.opts.Dir)
	if err != nil {
		return nil, nil, err
	}
	vars, err := cfg.ResolveVariables(w.opts.Vars)
	if err != nil {
		return nil, nil, err
	}
	return cfg, vars, nil
}

func (w *Workspace) engine(cfg *config.Config, vars map[string]value.Value) *engine.Engine {
	e := engine.New(cfg, vars, w.opts.Registry)
	if w.opts.Parallelism > 0 {
		e.Parallelism = w.opts.Parallelism
	}
	e.Log = w.opts.Log
	e.Hooks = w.opts.Hooks
	return e
}

// Session is a locked view of the workspace.
type Session struct {
	w      *Workspace
	lock   *state.Lock
	Config *config.Config
	Vars   map[string]value.Value
	State  *state.State
	Engine *engine.Engine
}

// Begin loads configuration, acquires the state lock and reads state.
func (w *Workspace) Begin(operation string) (*Session, error) {
	cfg, vars, err := w.Load()
	if err != nil {
		return nil, err
	}
	return w.beginWith(operation, cfg, vars)
}

func (w *Workspace) beginWith(operation string, cfg *config.Config, vars map[string]value.Value) (*Session, error) {
	lock, err := w.Store.Lock(operation, w.opts.LockTimeout)
	if err != nil {
		return nil, err
	}
	if lock.BrokeStale != nil {
		w.opts.Log.Warn("removed a stale state lock left by a process that no longer exists",
			"lock_id", lock.BrokeStale.ID, "pid", lock.BrokeStale.PID, "operation", lock.BrokeStale.Operation)
	}
	st, err := w.Store.Load()
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	return &Session{w: w, lock: lock, Config: cfg, Vars: vars, State: st, Engine: w.engine(cfg, vars)}, nil
}

// StaleLockBroken reports the stale lock this session removed, if any.
func (s *Session) StaleLockBroken() *state.LockInfo { return s.lock.BrokeStale }

// Close releases the lock.
func (s *Session) Close() error { return s.lock.Unlock() }

// Plan plans against the session's state.
func (s *Session) Plan(ctx context.Context, opts engine.PlanOptions) (*engine.Plan, error) {
	return s.Engine.Plan(ctx, s.State, opts)
}

// Apply applies a plan, persisting state after every step. The plan must
// have been made against the state currently on disk.
func (s *Session) Apply(ctx context.Context, p *engine.Plan) (*engine.ApplyResult, error) {
	// A state that has never been written gets a fresh lineage on every
	// load, so a plan made against "no state" matches any unwritten state.
	fresh := s.State.Serial == 0 && p.PriorState != nil && p.PriorState.Serial == 0
	if p.PriorState == nil || p.PriorState.Serial != s.State.Serial || (!fresh && p.PriorState.Lineage != s.State.Lineage) {
		return nil, errors.New("the plan is stale: state has changed since it was created; run plan again")
	}
	res, err := s.Engine.Apply(ctx, p, func(st *state.State) error { return s.w.Store.Save(st) })
	if res != nil {
		s.State = res.State
	}
	return res, err
}

// Refresh updates state from providers and saves it.
func (s *Session) Refresh(ctx context.Context) ([]engine.Drift, error) {
	st, drift, err := s.Engine.Refresh(ctx, s.State)
	if err != nil {
		return nil, err
	}
	if len(drift) == 0 {
		return nil, nil
	}
	if err := s.w.Store.Save(st); err != nil {
		return nil, err
	}
	s.State = st
	return drift, nil
}

// SavePlan writes a plan file.
func SavePlan(path string, p *engine.Plan) error {
	data, err := engine.MarshalPlan(p)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// LoadPlan reads a plan file and checks it against the configuration.
func (w *Workspace) LoadPlan(path string) (*engine.Plan, *config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	p, err := engine.UnmarshalPlan(data)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.LoadDir(w.opts.Dir)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Digest != p.ConfigDigest {
		return nil, nil, fmt.Errorf("plan %s was made from a different configuration; run plan again", path)
	}
	return p, cfg, nil
}

// ApplySaved applies a saved plan under the state lock.
func (w *Workspace) ApplySaved(ctx context.Context, path string) (*engine.ApplyResult, error) {
	p, cfg, err := w.LoadPlan(path)
	if err != nil {
		return nil, err
	}
	s, err := w.beginWith("apply", cfg, p.Variables)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	return s.Apply(ctx, p)
}

// ReadState returns the stored state without locking (read-only commands).
func (w *Workspace) ReadState() (*state.State, error) { return w.Store.Load() }

// EncodeState renders state as the JSON document stored on disk.
func EncodeState(st *state.State) ([]byte, error) {
	data, err := st.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Write(data)
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
