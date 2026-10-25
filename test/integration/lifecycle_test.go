package integration

import (
	"testing"

	"github.com/archit2501/plait/internal/engine"
)

const stack = `
variable "env" { default = "dev" }
variable "web_count" { default = 2 }

provider "fakecloud" {
  endpoint    = "ENDPOINT"
  token       = "test-token"
  retry_base_ms = 1
}

locals {
  prefix = "${var.env}-app"
}

resource "fakecloud_network" "main" {
  name = "${local.prefix}-net"
  cidr = "10.0.0.0/16"
}

resource "fakecloud_instance" "web" {
  count      = var.web_count
  name       = "${local.prefix}-web-${count.index}"
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

func TestCreateUpdateDestroyLifecycle(t *testing.T) {
	e := newEnv(t)
	e.write(stack)
	p, res := e.apply(engine.PlanOptions{})
	if got := counts(p); got != [3]int{6, 0, 0} {
		t.Fatalf("first plan counts %v\n%s", got, e.render(p))
	}
	if res.Added != 6 {
		t.Fatalf("added %d", res.Added)
	}
	t.Log(e.render(p))
	st := e.state()
	if len(st.Instances) != 6 {
		t.Fatalf("state has %d instances", len(st.Instances))
	}
	// Second plan: nothing to do.
	s, p2 := e.plan(engine.PlanOptions{})
	_ = s.Close()
	if p2.HasChanges() {
		t.Fatalf("expected no changes:\n%s", e.render(p2))
	}
	// Destroy.
	p3, res3 := e.apply(engine.PlanOptions{Destroy: true})
	t.Log(e.render(p3))
	if res3.Destroyed != 6 {
		t.Fatalf("destroyed %d", res3.Destroyed)
	}
	if st := e.cloud.Stats(); st.Objects["networks"]+st.Objects["instances"]+st.Objects["buckets"] != 0 {
		t.Fatalf("objects left: %+v", st.Objects)
	}
}
