// Package cli implements the plait command-line interface.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/archit2501/plait/internal/config"
	"github.com/archit2501/plait/internal/engine"
	"github.com/archit2501/plait/internal/state"
	"github.com/archit2501/plait/internal/value"
	"github.com/archit2501/plait/internal/workspace"
)

// Version is the plait version, overridable at link time.
var Version = "0.1.0"

const usage = `plait — a small declarative infrastructure-as-code engine

Usage: plait <command> [flags]

Core commands:
  validate      check configuration syntax, references and dependency cycles
  plan          show the changes required to match the configuration
  apply         create, update and delete objects to match the configuration
  destroy       delete every object plait manages
  refresh       update state from the real infrastructure

Inspection:
  show          show the state, or a saved plan file
  state list    list tracked resource instances
  state show    show one tracked instance
  output        print output values
  graph         print the dependency graph (Graphviz DOT)

Other:
  force-unlock  release a stuck state lock by its ID
  fakecloud     run the in-repo fake cloud API server
  version       print the version

Common flags: -dir DIR (configuration directory, default "."), -state FILE,
-var NAME=VALUE, -var-file FILE, -parallelism N. Set PLAIT_LOG=debug for logs.
`

// IO bundles the process streams so tests can drive the CLI.
type IO struct {
	In       io.Reader
	Out, Err io.Writer
}

// Run executes the CLI and returns the process exit code.
func Run(args []string, stdio IO) int {
	c := &cli{io: stdio}
	if len(args) == 0 {
		c.printf("%s", usage)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, rest := args[0], args[1:]
	var err error
	code := 0
	switch cmd {
	case "validate":
		err = c.validate(rest)
	case "plan":
		code, err = c.plan(ctx, rest)
	case "apply":
		err = c.apply(ctx, rest, false)
	case "destroy":
		err = c.apply(ctx, rest, true)
	case "refresh":
		err = c.refresh(ctx, rest)
	case "show":
		err = c.show(rest)
	case "state":
		err = c.state(rest)
	case "output":
		err = c.output(rest)
	case "graph":
		err = c.graph(ctx, rest)
	case "force-unlock":
		err = c.forceUnlock(rest)
	case "fakecloud":
		err = c.fakecloud(ctx, rest)
	case "version", "-version", "--version":
		c.printf("plait v%s\n", Version)
	case "help", "-h", "-help", "--help":
		c.printf("%s", usage)
	default:
		c.errorf("unknown command %q\n\n%s", cmd, usage)
		return 1
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		c.errorf("Error: %v\n", err)
		return 1
	}
	return code
}

type cli struct {
	io IO
	mu sync.Mutex // serializes progress output from parallel hooks
}

func (c *cli) printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = fmt.Fprintf(c.io.Out, format, args...)
}

func (c *cli) errorf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = fmt.Fprintf(c.io.Err, format, args...)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

// common holds flags shared by most commands.
type common struct {
	dir         string
	statePath   string
	vars        multiFlag
	varFiles    multiFlag
	parallelism int
	lockTimeout time.Duration
}

func (c *cli) flags(name string, withVars bool) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.io.Err)
	co := &common{}
	fs.StringVar(&co.dir, "dir", ".", "configuration directory")
	fs.StringVar(&co.statePath, "state", "", "state file (default <dir>/"+workspace.DefaultStateFile+")")
	fs.DurationVar(&co.lockTimeout, "lock-timeout", 0, "how long to wait for the state lock")
	if withVars {
		fs.Var(&co.vars, "var", "set an input variable: NAME=VALUE (repeatable)")
		fs.Var(&co.varFiles, "var-file", "load variable values from a "+config.VarsFileExt+" file (repeatable)")
		fs.IntVar(&co.parallelism, "parallelism", engine.DefaultParallelism, "maximum concurrent provider operations")
	}
	return fs, co
}

func logger(w io.Writer) *slog.Logger {
	level := slog.LevelWarn
	switch strings.ToLower(os.Getenv("PLAIT_LOG")) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

func (c *cli) workspace(co *common, hooks engine.Hooks) (*workspace.Workspace, error) {
	log := logger(c.io.Err)
	slog.SetDefault(log)
	return workspace.Open(workspace.Options{
		Dir: co.dir, StatePath: co.statePath, Parallelism: co.parallelism, LockTimeout: co.lockTimeout,
		Vars: config.VarInputs{Env: os.Environ(), VarFiles: co.varFiles, Flags: co.vars},
		Log:  log, Hooks: hooks,
	})
}

func (c *cli) validate(args []string) error {
	fs, co := c.flags("validate", false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadDir(co.dir)
	if err != nil {
		return err
	}
	c.printf("Success! The configuration is valid (%d resources, %d variables, %d locals, %d outputs).\n",
		len(cfg.Resources), len(cfg.Variables), len(cfg.Locals), len(cfg.Outputs))
	return nil
}

func (c *cli) plan(ctx context.Context, args []string) (int, error) {
	fs, co := c.flags("plan", true)
	out := fs.String("out", "", "save the plan to this file so it can be applied later with: plait apply FILE")
	destroy := fs.Bool("destroy", false, "plan to destroy every tracked object")
	refresh := fs.Bool("refresh", true, "refresh state from providers before planning")
	asJSON := fs.Bool("json", false, "print the plan as JSON")
	detailed := fs.Bool("detailed-exitcode", false, "exit 2 when the plan has changes")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	ws, err := c.workspace(co, engine.Hooks{})
	if err != nil {
		return 0, err
	}
	s, err := ws.Begin("plan")
	if err != nil {
		return 0, err
	}
	defer func() { _ = s.Close() }()
	p, err := s.Plan(ctx, engine.PlanOptions{Destroy: *destroy, SkipRefresh: !*refresh})
	if err != nil {
		return 0, err
	}
	if *asJSON {
		data, err := engine.MarshalPlan(p)
		if err != nil {
			return 0, err
		}
		c.printf("%s\n", data)
	} else if err := engine.Render(c.io.Out, p, engine.RenderOptions{}); err != nil {
		return 0, err
	}
	if *out != "" {
		if err := workspace.SavePlan(*out, p); err != nil {
			return 0, err
		}
		if !*asJSON {
			c.printf("\nSaved the plan to %s. To perform exactly these actions, run: plait apply %s\n", *out, *out)
		}
	}
	if *detailed && p.HasChanges() {
		return 2, nil
	}
	return 0, nil
}

// progress returns hooks that print Terraform-style progress lines.
func (c *cli) progress() engine.Hooks {
	verbs := map[engine.Op][3]string{
		engine.OpCreate:  {"Creating...", "Creation complete", "Creation failed"},
		engine.OpUpdate:  {"Modifying...", "Modifications complete", "Modification failed"},
		engine.OpDestroy: {"Destroying...", "Destruction complete", "Destruction failed"},
	}
	name := func(ev engine.Event) string {
		if ev.Deposed {
			return ev.Addr.String() + " (deposed object)"
		}
		return ev.Addr.String()
	}
	return engine.Hooks{
		Pre: func(ev engine.Event) {
			if v, ok := verbs[ev.Op]; ok {
				c.printf("%s: %s\n", name(ev), v[0])
			}
		},
		Post: func(ev engine.Event, result value.Value, err error, elapsed time.Duration) {
			v, ok := verbs[ev.Op]
			if !ok {
				return
			}
			if err != nil {
				c.printf("%s: %s after %s\n", name(ev), v[2], roundDur(elapsed))
				return
			}
			id := ""
			if idv, ok := result.Get("id"); ok && idv.Kind() == value.KindString {
				id = " [id=" + idv.AsString() + "]"
			}
			c.printf("%s: %s after %s%s\n", name(ev), v[1], roundDur(elapsed), id)
		},
	}
}

func roundDur(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func (c *cli) confirm(question string) bool {
	c.printf("%s\n  Only 'yes' will be accepted to approve.\n\n  Enter a value: ", question)
	line, _ := bufio.NewReader(c.io.In).ReadString('\n')
	return strings.TrimSpace(line) == "yes"
}

func (c *cli) apply(ctx context.Context, args []string, destroy bool) error {
	name := "apply"
	if destroy {
		name = "destroy"
	}
	fs, co := c.flags(name, true)
	auto := fs.Bool("auto-approve", false, "skip interactive approval")
	refresh := fs.Bool("refresh", true, "refresh state from providers before planning")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ws, err := c.workspace(co, c.progress())
	if err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("apply takes at most one argument: a saved plan file")
	}
	if fs.NArg() == 1 {
		if destroy {
			return errors.New("destroy does not take a plan file; use `plait plan -destroy -out FILE` and `plait apply FILE`")
		}
		res, err := ws.ApplySaved(ctx, fs.Arg(0))
		return c.finishApply(res, err)
	}
	s, err := ws.Begin(name)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	p, err := s.Plan(ctx, engine.PlanOptions{Destroy: destroy, SkipRefresh: !*refresh})
	if err != nil {
		return err
	}
	if err := engine.Render(c.io.Out, p, engine.RenderOptions{}); err != nil {
		return err
	}
	if !p.HasChanges() {
		return nil
	}
	c.printf("\n")
	if !*auto {
		q := "Do you want to perform these actions?"
		if destroy {
			q = "Do you really want to destroy all resources?"
		}
		if !c.confirm(q) {
			return errors.New("apply cancelled")
		}
		c.printf("\n")
	}
	res, err := s.Apply(ctx, p)
	return c.finishApply(res, err)
}

func (c *cli) finishApply(res *engine.ApplyResult, err error) error {
	if err != nil {
		return err
	}
	if ferr := res.Err(); ferr != nil {
		c.printf("\nApply finished with errors. Resources: %d added, %d changed, %d destroyed.\n", res.Added, res.Changed, res.Destroyed)
		return ferr
	}
	c.printf("\nApply complete! Resources: %d added, %d changed, %d destroyed.\n", res.Added, res.Changed, res.Destroyed)
	if len(res.State.Outputs) > 0 {
		c.printf("\nOutputs:\n\n")
		c.printOutputs(res.State.Outputs)
	}
	return nil
}

func (c *cli) printOutputs(outs map[string]value.Value) {
	names := make([]string, 0, len(outs))
	for n := range outs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c.printf("%s = %s\n", n, outs[n].GoString())
	}
}

func (c *cli) refresh(ctx context.Context, args []string) error {
	fs, co := c.flags("refresh", true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ws, err := c.workspace(co, engine.Hooks{})
	if err != nil {
		return err
	}
	s, err := ws.Begin("refresh")
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	drift, err := s.Refresh(ctx)
	if err != nil {
		return err
	}
	if len(drift) == 0 {
		c.printf("No changes. State matches the real infrastructure.\n")
		return nil
	}
	p := &engine.Plan{Drift: drift}
	if err := engine.Render(c.io.Out, p, engine.RenderOptions{}); err != nil {
		return err
	}
	c.printf("State updated (%d objects changed outside of plait).\n", len(drift))
	return nil
}

func (c *cli) show(args []string) error {
	fs, co := c.flags("show", false)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 1 {
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		p, err := engine.UnmarshalPlan(data)
		if err != nil {
			return err
		}
		if *asJSON {
			c.printf("%s", data)
			return nil
		}
		return engine.Render(c.io.Out, p, engine.RenderOptions{})
	}
	ws, err := c.workspace(co, engine.Hooks{})
	if err != nil {
		return err
	}
	st, err := ws.ReadState()
	if err != nil {
		return err
	}
	if *asJSON {
		data, err := workspace.EncodeState(st)
		if err != nil {
			return err
		}
		c.printf("%s", data)
		return nil
	}
	if st.Empty() {
		c.printf("The state is empty. No resources are managed.\n")
		return nil
	}
	for _, inst := range st.Sorted() {
		c.showInstance(inst)
	}
	if len(st.Outputs) > 0 {
		c.printf("Outputs:\n\n")
		c.printOutputs(st.Outputs)
	}
	return nil
}

func (c *cli) showInstance(inst *state.Instance) {
	for _, o := range []struct {
		obj   *state.Object
		label string
	}{{inst.Current, ""}, {inst.Deposed, " (deposed object)"}} {
		if o.obj == nil {
			continue
		}
		status := ""
		if o.obj.Status != state.StatusReady {
			status = " [" + string(o.obj.Status) + "]"
		}
		c.printf("# %s%s%s:\nresource %q %q {\n", inst.Addr, o.label, status, inst.Addr.Type, inst.Addr.Name)
		keys := o.obj.Attrs.Keys()
		width := 0
		for _, k := range keys {
			width = max(width, len(k))
		}
		for _, k := range keys {
			v, _ := o.obj.Attrs.Get(k)
			if v.IsNull() {
				continue
			}
			c.printf("    %s%s = %s\n", k, strings.Repeat(" ", width-len(k)), v.GoString())
		}
		c.printf("}\n\n")
	}
}

func (c *cli) state(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: plait state list | plait state show ADDRESS")
	}
	sub, rest := args[0], args[1:]
	fs, co := c.flags("state "+sub, false)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	ws, err := c.workspace(co, engine.Hooks{})
	if err != nil {
		return err
	}
	st, err := ws.ReadState()
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		for _, inst := range st.Sorted() {
			c.printf("%s\n", inst.Addr)
		}
		return nil
	case "show":
		if fs.NArg() != 1 {
			return errors.New("usage: plait state show ADDRESS")
		}
		for _, inst := range st.Sorted() {
			if inst.Addr.String() == fs.Arg(0) {
				c.showInstance(inst)
				return nil
			}
		}
		return fmt.Errorf("no instance %s in state", fs.Arg(0))
	}
	return fmt.Errorf("unknown state subcommand %q", sub)
}

func (c *cli) output(args []string) error {
	fs, co := c.flags("output", false)
	asJSON := fs.Bool("json", false, "print JSON")
	raw := fs.Bool("raw", false, "print a string output without quotes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ws, err := c.workspace(co, engine.Hooks{})
	if err != nil {
		return err
	}
	st, err := ws.ReadState()
	if err != nil {
		return err
	}
	if fs.NArg() == 1 {
		v, ok := st.Outputs[fs.Arg(0)]
		if !ok {
			return fmt.Errorf("output %q not found in state", fs.Arg(0))
		}
		switch {
		case *raw && v.Kind() == value.KindString:
			c.printf("%s\n", v.AsString())
		case *asJSON:
			data, err := json.Marshal(v)
			if err != nil {
				return err
			}
			c.printf("%s\n", data)
		default:
			c.printf("%s\n", v.GoString())
		}
		return nil
	}
	if *asJSON {
		data, err := json.MarshalIndent(st.Outputs, "", "  ")
		if err != nil {
			return err
		}
		c.printf("%s\n", data)
		return nil
	}
	c.printOutputs(st.Outputs)
	return nil
}

func (c *cli) graph(ctx context.Context, args []string) error {
	fs, co := c.flags("graph", true)
	apply := fs.Bool("apply", false, "print the instance-level apply graph of the current plan instead")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ws, err := c.workspace(co, engine.Hooks{})
	if err != nil {
		return err
	}
	var nodes []string
	var edges [][2]string
	if *apply {
		s, err := ws.Begin("graph")
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		p, err := s.Plan(ctx, engine.PlanOptions{})
		if err != nil {
			return err
		}
		nodes, edges, err = engine.ApplyGraph(p)
		if err != nil {
			return err
		}
	} else {
		cfg, err := config.LoadDir(co.dir)
		if err != nil {
			return err
		}
		g, err := cfg.Graph()
		if err != nil {
			return err
		}
		nodes, edges = g.Nodes(), g.Edges()
	}
	c.printf("digraph plait {\n  rankdir = \"RL\";\n  node [shape = box];\n")
	for _, n := range nodes {
		c.printf("  %q;\n", n)
	}
	for _, e := range edges {
		c.printf("  %q -> %q;\n", e[0], e[1])
	}
	c.printf("}\n")
	return nil
}

func (c *cli) forceUnlock(args []string) error {
	fs, co := c.flags("force-unlock", false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: plait force-unlock LOCK_ID")
	}
	ws, err := c.workspace(co, engine.Hooks{})
	if err != nil {
		return err
	}
	if err := ws.Store.ForceUnlock(fs.Arg(0)); err != nil {
		return err
	}
	c.printf("State lock %s released.\n", fs.Arg(0))
	return nil
}
