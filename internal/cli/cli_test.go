package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archit2501/plait/internal/fakecloud"
)

const cfg = `
variable "endpoint" { type = string }
variable "n" { default = 1 }
provider "fakecloud" {
  endpoint = var.endpoint
  token    = "tk"
}
resource "fakecloud_network" "net" {
  name = "cli-net"
  cidr = "10.3.0.0/16"
}
resource "fakecloud_instance" "vm" {
  count      = var.n
  name       = "vm-${count.index}"
  network_id = fakecloud_network.net.id
}
resource "local_file" "f" {
  path    = "hello.txt"
  content = "hi"
}
output "ips" { value = fakecloud_instance.vm[*].private_ip }
output "net" { value = fakecloud_network.net.id }
`

type harness struct {
	t   *testing.T
	dir string
	url string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	hs := httptest.NewServer(fakecloud.New("tk"))
	t.Cleanup(hs.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.plait"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, dir: dir, url: hs.URL}
}

// run executes plait with -dir and the endpoint variable where accepted.
func (h *harness) run(stdin string, args ...string) (int, string, string) {
	h.t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, IO{In: strings.NewReader(stdin), Out: &out, Err: &errb})
	return code, out.String(), errb.String()
}

func (h *harness) withVars(cmd string, extra ...string) []string {
	return append([]string{cmd, "-dir", h.dir, "-var", "endpoint=" + h.url}, extra...)
}

func TestCLIWorkflow(t *testing.T) {
	h := newHarness(t)
	if code, out, _ := h.run("", "validate", "-dir", h.dir); code != 0 || !strings.Contains(out, "3 resources") {
		t.Fatalf("validate: %d %s", code, out)
	}
	code, out, errOut := h.run("", h.withVars("plan", "-detailed-exitcode")...)
	if code != 2 || !strings.Contains(out, "Plan: 3 to add, 0 to change, 0 to destroy.") {
		t.Fatalf("plan: %d\n%s\n%s", code, out, errOut)
	}
	// Declining the prompt cancels.
	if code, _, errOut := h.run("no\n", h.withVars("apply")...); code != 1 || !strings.Contains(errOut, "apply cancelled") {
		t.Fatalf("decline: %d %s", code, errOut)
	}
	code, out, errOut = h.run("yes\n", h.withVars("apply")...)
	if code != 0 || !strings.Contains(out, "Apply complete! Resources: 3 added, 0 changed, 0 destroyed.") ||
		!strings.Contains(out, "fakecloud_network.net: Creation complete after") || !strings.Contains(out, `ips = ["10.3.0.4"]`) {
		t.Fatalf("apply: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ := h.run("", h.withVars("plan", "-detailed-exitcode")...); code != 0 || !strings.Contains(out, "No changes.") {
		t.Fatalf("converged plan: %d %s", code, out)
	}
	if _, out, _ := h.run("", "state", "list", "-dir", h.dir); out != "fakecloud_instance.vm[0]\nfakecloud_network.net\nlocal_file.f\n" {
		t.Fatalf("state list: %q", out)
	}
	if _, out, _ := h.run("", "state", "show", "-dir", h.dir, "fakecloud_network.net"); !strings.Contains(out, `cidr = "10.3.0.0/16"`) {
		t.Fatalf("state show: %s", out)
	}
	if code, _, errOut := h.run("", "state", "show", "-dir", h.dir, "nope.x"); code != 1 || !strings.Contains(errOut, "no instance") {
		t.Fatal("state show missing")
	}
	if _, out, _ := h.run("", "show", "-dir", h.dir); !strings.Contains(out, `resource "local_file" "f"`) || !strings.Contains(out, "Outputs:") {
		t.Fatalf("show: %s", out)
	}
	_, out, _ = h.run("", "show", "-dir", h.dir, "-json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc["serial"] == nil {
		t.Fatalf("show -json: %v", err)
	}
	if _, out, _ := h.run("", "output", "-dir", h.dir, "-raw", "net"); out != "net-0001\n" {
		t.Fatalf("output -raw: %q", out)
	}
	if _, out, _ := h.run("", "output", "-dir", h.dir, "-json", "ips"); out != "[\"10.3.0.4\"]\n" {
		t.Fatalf("output -json: %q", out)
	}
	if _, out, _ := h.run("", "output", "-dir", h.dir, "net"); out != "\"net-0001\"\n" {
		t.Fatalf("output: %q", out)
	}
	if _, out, _ := h.run("", "output", "-dir", h.dir); !strings.Contains(out, "net = ") {
		t.Fatalf("outputs: %q", out)
	}
	if _, out, _ := h.run("", "output", "-dir", h.dir, "-json"); !strings.Contains(out, `"net": "net-0001"`) {
		t.Fatalf("outputs json: %q", out)
	}
	if code, _, _ := h.run("", "output", "-dir", h.dir, "missing"); code != 1 {
		t.Fatal("missing output")
	}
	if _, out, _ := h.run("", "graph", "-dir", h.dir); !strings.Contains(out, `"fakecloud_instance.vm" -> "fakecloud_network.net";`) {
		t.Fatalf("graph: %s", out)
	}

	// Saved plan workflow: scale out, review, apply the file.
	planFile := filepath.Join(h.dir, "scale.plan.json")
	if code, out, _ := h.run("", h.withVars("plan", "-var", "n=2", "-out", planFile)...); code != 0 || !strings.Contains(out, "Saved the plan") {
		t.Fatalf("plan -out: %d %s", code, out)
	}
	if _, out, _ := h.run("", "show", planFile); !strings.Contains(out, "fakecloud_instance.vm[1] will be created") {
		t.Fatalf("show plan: %s", out)
	}
	if _, out, _ := h.run("", "show", "-json", planFile); !strings.Contains(out, `"format_version": 1`) {
		t.Fatalf("show plan json: %s", out)
	}
	if _, out, _ := h.run("", h.withVars("graph", "-apply", "-var", "n=2")...); !strings.Contains(out, `"fakecloud_instance.vm[1] (create)"`) {
		t.Fatalf("apply graph: %s", out)
	}
	if code, out, errOut := h.run("", "apply", "-dir", h.dir, planFile); code != 0 || !strings.Contains(out, "1 added") {
		t.Fatalf("apply plan file: %d %s %s", code, out, errOut)
	}
	if code, _, errOut := h.run("", "apply", "-dir", h.dir, planFile); code != 1 || !strings.Contains(errOut, "stale") {
		t.Fatalf("reapply plan: %d %s", code, errOut)
	}
	_, out, _ = h.run("", h.withVars("plan", "-json", "-var", "n=2")...)
	if !strings.Contains(out, `"resource_changes"`) {
		t.Fatalf("plan -json: %s", out)
	}
	if code, out, _ := h.run("", h.withVars("refresh", "-var", "n=2")...); code != 0 || !strings.Contains(out, "No changes. State matches") {
		t.Fatalf("refresh: %d %s", code, out)
	}
	if err := os.Remove(filepath.Join(h.dir, "hello.txt")); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := h.run("", h.withVars("refresh", "-var", "n=2")...); code != 0 || !strings.Contains(out, "local_file.f has been deleted") {
		t.Fatalf("refresh drift: %d %s", code, out)
	}
	code, out, errOut = h.run("", h.withVars("destroy", "-auto-approve", "-var", "n=2")...)
	if code != 0 || !strings.Contains(out, "3 destroyed") {
		t.Fatalf("destroy: %d %s %s", code, out, errOut)
	}
	if _, out, _ := h.run("", "show", "-dir", h.dir); !strings.Contains(out, "state is empty") {
		t.Fatalf("show empty: %s", out)
	}
	if code, _, errOut := h.run("", "destroy", "-dir", h.dir, planFile); code != 1 || !strings.Contains(errOut, "does not take a plan file") {
		t.Fatalf("destroy with file: %d %s", code, errOut)
	}
}

func TestCLIErrorsAndMisc(t *testing.T) {
	h := newHarness(t)
	if code, out, _ := h.run("", "version"); code != 0 || !strings.HasPrefix(out, "plait v") {
		t.Fatal("version")
	}
	if code, out, _ := h.run("", "help"); code != 0 || !strings.Contains(out, "Core commands") {
		t.Fatal("help")
	}
	if code, _, _ := h.run(""); code != 1 {
		t.Fatal("no args")
	}
	if code, _, errOut := h.run("", "frobnicate"); code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Fatal("unknown command")
	}
	if code, _, errOut := h.run("", "plan", "-dir", h.dir); code != 1 || !strings.Contains(errOut, `no value for required variable "endpoint"`) {
		t.Fatalf("missing var: %s", errOut)
	}
	if code, _, _ := h.run("", "plan", "-h"); code != 0 {
		t.Fatal("-h should exit 0")
	}
	if code, _, errOut := h.run("", "validate", "-dir", t.TempDir()); code != 1 || !strings.Contains(errOut, "no .plait files") {
		t.Fatalf("empty dir: %s", errOut)
	}
	if code, _, _ := h.run("", "state"); code != 1 {
		t.Fatal("state without subcommand")
	}
	if code, _, _ := h.run("", "state", "frob", "-dir", h.dir); code != 1 {
		t.Fatal("state frob")
	}
	if code, _, _ := h.run("", "apply", "-dir", h.dir, "a", "b"); code != 1 {
		t.Fatal("too many args")
	}
	if code, _, errOut := h.run("", "force-unlock", "-dir", h.dir, "abc"); code != 1 || !strings.Contains(errOut, "not locked") {
		t.Fatalf("force-unlock: %s", errOut)
	}
	if code, _, _ := h.run("", "force-unlock", "-dir", h.dir); code != 1 {
		t.Fatal("force-unlock usage")
	}
	// Write a lock by hand and release it by ID.
	lock := `{"id":"deadbeef0000","operation":"apply","who":"x","pid":1,"host":"elsewhere","created":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(h.dir, "plait.state.json.lock"), []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := h.run("", h.withVars("plan")...); code != 1 || !strings.Contains(errOut, "force-unlock deadbeef0000") {
		t.Fatalf("locked plan: %s", errOut)
	}
	if code, out, _ := h.run("", "force-unlock", "-dir", h.dir, "deadbeef0000"); code != 0 || !strings.Contains(out, "released") {
		t.Fatal("force-unlock")
	}
	if code, _, errOut := h.run("", "show", filepath.Join(h.dir, "missing.json")); code != 1 || errOut == "" {
		t.Fatal("show missing plan")
	}
}

func TestFakecloudCommand(t *testing.T) {
	var out bytes.Buffer
	c := &cli{io: IO{Out: &out, Err: &out}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.fakecloud(ctx, []string{"-addr", "127.0.0.1:0", "-latency-ms", "1"}) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.snapshot(&out), "listening") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := c.fakecloud(context.Background(), []string{"-addr", "not-an-address"}); err == nil {
		t.Fatal("bad address accepted")
	}
}

func (c *cli) snapshot(b *bytes.Buffer) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return b.String()
}
