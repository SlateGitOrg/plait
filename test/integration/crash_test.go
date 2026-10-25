package integration

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/archit2501/plait/internal/cli"
	"github.com/archit2501/plait/internal/engine"
	"github.com/archit2501/plait/internal/fakecloud"
	"github.com/archit2501/plait/internal/state"
)

// TestMain lets the test binary double as the plait CLI: the crash tests
// re-execute it as a child process and SIGKILL that child from inside the
// fake cloud at the worst possible moment — after a mutation committed on
// the server but before the child saw the response.
func TestMain(m *testing.M) {
	if os.Getenv("PLAIT_CRASH_HELPER") == "1" {
		args := strings.Split(os.Getenv("PLAIT_CRASH_ARGS"), "\x1f")
		os.Exit(cli.Run(args, cli.IO{In: strings.NewReader(""), Out: os.Stdout, Err: os.Stderr}))
	}
	os.Exit(m.Run())
}

// crashDuring runs `plait <args>` in a child process and kills it when the
// fake cloud commits an event matching (op, kind). It returns the child's
// output for diagnostics.
func (e *env) crashDuring(op, kind string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "PLAIT_CRASH_HELPER=1", "PLAIT_CRASH_ARGS="+strings.Join(args, "\x1f"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	exited := make(chan struct{})
	var once sync.Once
	var killed atomic.Bool
	var proc atomic.Pointer[os.Process]
	e.cloud.SetOnCommit(func(ev fakecloud.Event) {
		if ev.Op != op || ev.Kind != kind {
			return
		}
		once.Do(func() {
			killed.Store(true)
			_ = proc.Load().Kill()
			<-exited // the child is dead before the server writes the response
		})
	})
	defer e.cloud.SetOnCommit(nil)
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	proc.Store(cmd.Process)
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		e.t.Fatalf("child did not finish:\n%s", out.String())
	}
	if !killed.Load() {
		e.t.Fatalf("child finished without reaching %s %s:\n%s", op, kind, out.String())
	}
	return out.String()
}

func (e *env) cliArgs(cmd string, extra ...string) []string {
	args := []string{cmd, "-dir", e.dir, "-auto-approve", "-parallelism", "1"}
	for _, v := range e.vars {
		args = append(args, "-var", v)
	}
	return append(args, extra...)
}

func TestCrashMidApplyRecovers(t *testing.T) {
	e := newEnv(t)
	e.write(stack)
	out := e.crashDuring("create", "instances", e.cliArgs("apply")...)

	// The crashed run left a lock behind and recorded the in-flight create
	// as pending, with the idempotency token it sent.
	ws := e.workspace(4)
	held, err := ws.Store.ReadLock()
	if err != nil {
		t.Fatalf("expected a stale lock file: %v\n%s", err, out)
	}
	st := e.state()
	web0 := st.Instances["fakecloud_instance.web[0]"]
	if web0 == nil || web0.Current.Status != state.StatusPending || web0.Current.CreateToken == "" {
		t.Fatalf("web[0] should be pending with a token: %+v\n%s", web0, out)
	}
	if st.Instances["fakecloud_network.main"].Current.Status != state.StatusReady {
		t.Fatal("network should be ready")
	}
	if st.Instances["fakecloud_instance.web[1]"] != nil {
		t.Fatal("web[1] should not have started")
	}
	if n := len(e.cloud.Objects("instances")); n != 1 {
		t.Fatalf("server should hold the committed instance, has %d", n)
	}

	// Recovery: the stale lock is broken, refresh finds the committed
	// instance by its token, and apply finishes the rest.
	s, p := e.plan(engine.PlanOptions{})
	if s.StaleLockBroken() == nil || s.StaleLockBroken().ID != held.ID {
		t.Fatalf("stale lock %s not broken", held.ID)
	}
	_ = s.Close()
	if len(p.Drift) != 1 || p.Drift[0].Kind != "recovered" || p.Drift[0].Addr.String() != "fakecloud_instance.web[0]" {
		t.Fatalf("drift %+v", p.Drift)
	}
	if got := counts(p); got != [3]int{2, 0, 0} {
		t.Fatalf("recovery plan %v\n%s", got, e.render(p))
	}
	if !strings.Contains(e.render(p), "was being created when plait was interrupted; the object exists and is now tracked") {
		t.Fatalf("render:\n%s", e.render(p))
	}
	e.apply(engine.PlanOptions{})
	if n := len(e.cloud.Objects("instances")); n != 2 {
		t.Fatalf("expected exactly 2 instances after recovery (no duplicate), got %d", n)
	}
	s, p = e.plan(engine.PlanOptions{})
	_ = s.Close()
	if p.HasChanges() {
		t.Fatalf("not converged:\n%s", e.render(p))
	}
}

func TestCrashMidDestroyRecovers(t *testing.T) {
	e := newEnv(t)
	e.write(stack)
	e.apply(engine.PlanOptions{})
	out := e.crashDuring("delete", "instances", e.cliArgs("destroy")...)
	if st := e.state(); len(st.Instances) < 2 {
		t.Fatalf("state should still list the instance whose delete was lost:\n%s", out)
	}
	p, res := e.apply(engine.PlanOptions{Destroy: true})
	deleted := 0
	for _, d := range p.Drift {
		if d.Kind == "deleted" {
			deleted++
		}
	}
	if deleted != 1 {
		t.Fatalf("expected the lost delete to show up as drift: %+v", p.Drift)
	}
	if res.Err() != nil || !e.state().Empty() {
		t.Fatalf("destroy did not converge: %v", res.Err())
	}
	if st := e.cloud.Stats(); st.Objects["networks"]+st.Objects["instances"]+st.Objects["buckets"] != 0 {
		t.Fatalf("objects left: %+v", st.Objects)
	}
}

func TestCrashDuringCreateBeforeDestroyLeavesDeposedObjectsThatAreCleanedUp(t *testing.T) {
	e := newEnv(t)
	src := strings.ReplaceAll(stack, "  cidr = var.cidr\n", "  cidr = var.cidr\n  lifecycle { create_before_destroy = true }\n")
	src = strings.ReplaceAll(src, "  tags       = { env = var.env }\n", "  tags       = { env = var.env }\n  lifecycle { create_before_destroy = true }\n")
	e.vars = []string{"web_count=1"}
	e.write(src)
	e.apply(engine.PlanOptions{})
	e.vars = append(e.vars, "cidr=10.5.0.0/16")
	out := e.crashDuring("create", "instances", e.cliArgs("apply")...)
	st := e.state()
	net := st.Instances["fakecloud_network.main"]
	web := st.Instances["fakecloud_instance.web[0]"]
	if net.Deposed == nil || web.Deposed == nil || web.Current.Status != state.StatusPending {
		t.Fatalf("expected deposed objects and a pending replacement:\nnet=%+v\nweb=%+v\n%s", net, web, out)
	}
	p, _ := e.apply(engine.PlanOptions{})
	deposed := 0
	for _, c := range p.Changes {
		if c.Deposed && c.Action == engine.Delete {
			deposed++
		}
	}
	if deposed != 2 {
		t.Fatalf("expected two deposed deletes:\n%s", e.render(p))
	}
	if n, i := len(e.cloud.Objects("networks")), len(e.cloud.Objects("instances")); n != 1 || i != 1 {
		t.Fatalf("after cleanup: %d networks, %d instances", n, i)
	}
	if objs := e.cloud.Objects("networks"); objs[0]["cidr"] != "10.5.0.0/16" {
		t.Fatalf("wrong network survived: %+v", objs)
	}
}
