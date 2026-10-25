package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archit2501/plait/internal/engine"
	"github.com/archit2501/plait/internal/fakecloud"
	"github.com/archit2501/plait/internal/state"
	"github.com/archit2501/plait/internal/workspace"
)

const stack = `
variable "env" { default = "dev" }
variable "web_count" { default = 2 }
variable "cidr" { default = "10.0.0.0/16" }
variable "size" { default = "small" }

provider "fakecloud" {
  endpoint      = "ENDPOINT"
  token         = "test-token"
  retry_base_ms = 1
}

locals {
  prefix = "${var.env}-app"
}

resource "fakecloud_network" "main" {
  name = "${local.prefix}-net"
  cidr = var.cidr
}

resource "fakecloud_instance" "web" {
  count      = var.web_count
  name       = "${local.prefix}-web-${count.index}"
  size       = var.size
  network_id = fakecloud_network.main.id
  tags       = { env = var.env }
}

resource "fakecloud_bucket" "assets" {
  for_each = toset(["logs", "media"])
  name     = "${local.prefix}-${each.key}"
}

resource "local_file" "inventory" {
  path    = "out/inventory.txt"
  content = join("\n", fakecloud_instance.web[*].private_ip)
}

output "ips" { value = fakecloud_instance.web[*].private_ip }
`

// seqOf returns the event sequence numbers of committed mutations matching
// op and kind.
func seqOf(evs []fakecloud.Event, op, kind string) []int {
	var out []int
	for _, e := range evs {
		if e.Op == op && e.Kind == kind {
			out = append(out, e.Seq)
		}
	}
	return out
}

func TestApplyRefreshDestroyAgainstFakeCloud(t *testing.T) {
	e := newEnv(t)
	e.write(stack)

	p, res := e.apply(engine.PlanOptions{})
	if got := counts(p); got != [3]int{6, 0, 0} || res.Added != 6 {
		t.Fatalf("first apply: plan %v added %d\n%s", got, res.Added, e.render(p))
	}
	evs := e.cloud.Events()
	net := seqOf(evs, "create", "networks")
	for _, s := range seqOf(evs, "create", "instances") {
		if s < net[0] {
			t.Fatalf("instance created before its network: %+v", evs)
		}
	}
	inv, err := os.ReadFile(filepath.Join(e.dir, "out/inventory.txt"))
	if err != nil || strings.Count(string(inv), "10.0.0.") != 2 {
		t.Fatalf("inventory %q %v", inv, err)
	}
	st := e.state()
	if len(st.Instances) != 6 || st.Outputs["ips"].Len() != 2 {
		t.Fatalf("state: %d instances, outputs %v", len(st.Instances), st.Outputs)
	}

	// Idempotent: a second plan has nothing to do.
	s, p2 := e.plan(engine.PlanOptions{})
	_ = s.Close()
	if p2.HasChanges() || len(p2.Drift) != 0 {
		t.Fatalf("expected no changes:\n%s", e.render(p2))
	}

	// In-place update plus count shrink.
	e.vars = []string{"size=large", "web_count=1"}
	p3, res3 := e.apply(engine.PlanOptions{})
	if got := counts(p3); got != [3]int{0, 2, 1} || res3.Changed != 2 || res3.Destroyed != 1 {
		t.Fatalf("update plan %v changed %d destroyed %d\n%s", got, res3.Changed, res3.Destroyed, e.render(p3))
	}
	if objs := e.cloud.Objects("instances"); len(objs) != 1 || objs[0]["size"] != "large" {
		t.Fatalf("instances after update: %+v", objs)
	}

	// Drift: one bucket deleted and the instance resized out of band.
	if _, err := e.cloudDo("DELETE", "/v1/buckets/"+e.idOf(`fakecloud_bucket.assets["logs"]`), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cloudDo("PATCH", "/v1/instances/"+e.idOf("fakecloud_instance.web[0]"), `{"size":"medium"}`); err != nil {
		t.Fatal(err)
	}
	s, p4 := e.plan(engine.PlanOptions{})
	_ = s.Close()
	out := e.render(p4)
	for _, want := range []string{
		`# fakecloud_bucket.assets["logs"] has been deleted`,
		`# fakecloud_instance.web[0] has changed`,
		`~ size = "large" -> "medium"`,
		`# fakecloud_bucket.assets["logs"] will be created`,
		`~ size = "medium" -> "large"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drift plan missing %q:\n%s", want, out)
		}
	}
	// Refresh-only persists the drift without changing infrastructure.
	ws := e.workspace(2)
	sess, err := ws.Begin("refresh")
	if err != nil {
		t.Fatal(err)
	}
	drift, err := sess.Refresh(context.Background())
	_ = sess.Close()
	if err != nil || len(drift) != 2 {
		t.Fatalf("refresh drift %d err %v", len(drift), err)
	}
	if st := e.state(); len(st.Instances) != 4 {
		t.Fatalf("refreshed state should have dropped the deleted bucket: %d", len(st.Instances))
	}
	e.apply(engine.PlanOptions{})

	// Replacement: a new CIDR forces new network and instance; the old
	// instance must be deleted before the old network (the API refuses to
	// delete a network with attached instances).
	before := len(e.cloud.Events())
	e.vars = append(e.vars, "cidr=10.9.0.0/16")
	p5, res5 := e.apply(engine.PlanOptions{})
	if got := counts(p5); got != [3]int{2, 1, 2} {
		t.Fatalf("replace plan %v\n%s", got, e.render(p5))
	}
	if res5.Added != 2 || res5.Destroyed != 2 {
		t.Fatalf("replace result %+v", res5)
	}
	evs = e.cloud.Events()[before:]
	order := make([]string, len(evs))
	for i, ev := range evs {
		order[i] = ev.Op + ":" + ev.Kind
	}
	if got := strings.Join(order, ","); got != "delete:instances,delete:networks,create:networks,create:instances" {
		t.Fatalf("replace order %s", got)
	}

	// Destroy everything, dependents first.
	p6, res6 := e.apply(engine.PlanOptions{Destroy: true})
	if res6.Destroyed != 5 || counts(p6) != [3]int{0, 0, 5} {
		t.Fatalf("destroy %d\n%s", res6.Destroyed, e.render(p6))
	}
	if st := e.cloud.Stats(); st.Objects["networks"]+st.Objects["instances"]+st.Objects["buckets"] != 0 {
		t.Fatalf("objects left: %+v", st.Objects)
	}
	if st := e.state(); !st.Empty() || len(st.Outputs) != 0 {
		t.Fatalf("state not empty after destroy: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "out/inventory.txt")); !os.IsNotExist(err) {
		t.Fatalf("inventory file should be gone: %v", err)
	}
}

func TestCreateBeforeDestroyOrdering(t *testing.T) {
	e := newEnv(t)
	src := strings.ReplaceAll(stack, "  cidr = var.cidr\n", "  cidr = var.cidr\n  lifecycle { create_before_destroy = true }\n")
	src = strings.ReplaceAll(src, "  tags       = { env = var.env }\n", "  tags       = { env = var.env }\n  lifecycle { create_before_destroy = true }\n")
	e.vars = []string{"web_count=1"}
	e.write(src)
	e.apply(engine.PlanOptions{})
	before := len(e.cloud.Events())
	e.vars = append(e.vars, "cidr=10.7.0.0/16")
	e.apply(engine.PlanOptions{})
	var order []string
	for _, ev := range e.cloud.Events()[before:] {
		order = append(order, ev.Op+":"+ev.Kind)
	}
	if got := strings.Join(order, ","); got != "create:networks,create:instances,delete:instances,delete:networks" {
		t.Fatalf("create_before_destroy order %s", got)
	}
	for _, inst := range e.state().Instances {
		if inst.Deposed != nil {
			t.Fatalf("%s still has a deposed object", inst.Addr)
		}
	}
}

func TestParallelismIsBoundedAndUsed(t *testing.T) {
	src := `
provider "fakecloud" {
  endpoint = "ENDPOINT"
  token    = "test-token"
}
resource "fakecloud_bucket" "b" {
  count = 8
  name  = "bucket-${count.index}"
}
`
	for _, par := range []int{1, 4} {
		e := newEnv(t)
		e.cloud.SetFaults(fakecloud.Faults{LatencyMS: 40})
		e.write(src)
		s, err := e.workspace(par).Begin("apply")
		if err != nil {
			t.Fatal(err)
		}
		p, err := s.Plan(context.Background(), engine.PlanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		res, err := s.Apply(context.Background(), p)
		elapsed := time.Since(start)
		_ = s.Close()
		if err != nil || res.Err() != nil {
			t.Fatal(err, res.Err())
		}
		if got := e.cloud.Stats().MaxInFlight; got != par {
			t.Fatalf("parallelism %d: max in-flight requests %d", par, got)
		}
		if par == 4 && elapsed > 8*40*time.Millisecond {
			t.Fatalf("parallel apply took %s, no faster than serial", elapsed)
		}
	}
}

func TestTransientFaultsAreRetriedWithoutDuplicates(t *testing.T) {
	e := newEnv(t)
	e.write(stack)
	e.cloud.SetFaults(fakecloud.Faults{
		FailBefore:      map[string]int{"create:networks": 2},
		FailAfterCommit: map[string]int{"create:instances": 1, "create:buckets": 1},
	})
	_, res := e.apply(engine.PlanOptions{})
	if res.Added != 6 {
		t.Fatalf("added %d", res.Added)
	}
	st := e.cloud.Stats()
	if st.Objects["networks"] != 1 || st.Objects["instances"] != 2 || st.Objects["buckets"] != 2 {
		t.Fatalf("idempotent retries must not duplicate objects: %+v", st.Objects)
	}
}

func TestHardFailureSkipsDependentsAndConverges(t *testing.T) {
	e := newEnv(t)
	e.vars = []string{"cidr=10.0.0.0/30"} // rejected by the API: too small
	e.write(stack)
	s, p := e.plan(engine.PlanOptions{})
	res, err := s.Apply(context.Background(), p)
	_ = s.Close()
	if err != nil {
		t.Fatal(err)
	}
	msg := res.Err().Error()
	if !strings.Contains(msg, "fakecloud_network.main (create)") || !strings.Contains(msg, "too small") ||
		!strings.Contains(msg, "fakecloud_instance.web[0] (create): skipped") {
		t.Fatalf("errors: %s", msg)
	}
	if res.Added != 2 { // the two independent buckets
		t.Fatalf("added %d", res.Added)
	}
	e.vars = nil
	p2, res2 := e.apply(engine.PlanOptions{})
	if len(p2.Drift) != 1 || p2.Drift[0].Kind != "abandoned" || res2.Added != 4 {
		t.Fatalf("recovery: drift %+v added %d", p2.Drift, res2.Added)
	}
}

func TestSavedPlansAndStaleness(t *testing.T) {
	e := newEnv(t)
	e.write(stack)
	s, p := e.plan(engine.PlanOptions{})
	_ = s.Close()
	planPath := filepath.Join(t.TempDir(), "p.json")
	if err := workspace.SavePlan(planPath, p); err != nil {
		t.Fatal(err)
	}
	res, err := e.workspace(4).ApplySaved(context.Background(), planPath)
	if err != nil || res.Err() != nil || res.Added != 6 {
		t.Fatalf("apply saved: %v %v", err, res)
	}
	// The same plan cannot be applied twice: the state serial moved on.
	if _, err := e.workspace(4).ApplySaved(context.Background(), planPath); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale plan accepted: %v", err)
	}
	// Nor after the configuration changes.
	s, p2 := e.plan(engine.PlanOptions{Destroy: true})
	_ = s.Close()
	if err := workspace.SavePlan(planPath, p2); err != nil {
		t.Fatal(err)
	}
	e.write(stack + "\n# edited\n")
	if _, err := e.workspace(4).ApplySaved(context.Background(), planPath); err == nil || !strings.Contains(err.Error(), "different configuration") {
		t.Fatalf("plan for old config accepted: %v", err)
	}
}

func TestStateLockExcludesConcurrentRuns(t *testing.T) {
	e := newEnv(t)
	e.write(stack)
	s1, err := e.workspace(1).Begin("apply")
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := workspace.Open(workspace.Options{Dir: e.dir, LockTimeout: 150 * time.Millisecond})
	_, err = ws.Begin("plan")
	var locked *state.LockedError
	if !errors.As(err, &locked) || locked.Info.Operation != "apply" {
		t.Fatalf("second session should be locked out, got %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := ws.Begin("plan")
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = s2.Close()
}
