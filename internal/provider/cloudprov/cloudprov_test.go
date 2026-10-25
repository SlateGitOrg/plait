package cloudprov

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/archit2501/plait/internal/fakecloud"
	"github.com/archit2501/plait/internal/provider"
	"github.com/archit2501/plait/internal/value"
)

func setup(t *testing.T, token string) (provider.Provider, *fakecloud.Server) {
	t.Helper()
	srv := fakecloud.New("secret")
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	p := New()
	cfg := map[string]value.Value{"endpoint": value.String(hs.URL + "/"), "retry_base_ms": value.Int(1), "max_retries": value.Int(2)}
	if token != "" {
		cfg["token"] = value.String(token)
	}
	if err := p.Configure(context.Background(), value.Map(cfg)); err != nil {
		t.Fatal(err)
	}
	return p, srv
}

func obj(kv ...any) value.Value {
	m := map[string]value.Value{}
	for i := 0; i < len(kv); i += 2 {
		v, ok := kv[i+1].(value.Value)
		if !ok {
			v, _ = value.FromGo(kv[i+1])
		}
		m[kv[i].(string)] = v
	}
	return value.Map(m)
}

func plan(t *testing.T, p provider.Provider, typ string, prior, cfg value.Value) provider.PlanResponse {
	t.Helper()
	norm, err := provider.ValidateConfig(p.Schema().Resources[typ], typ, cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.PlanResource(context.Background(), provider.PlanRequest{Type: typ, Prior: prior, Config: norm})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNetworkInstanceLifecycle(t *testing.T) {
	p, srv := setup(t, "secret")
	ctx := context.Background()
	np := plan(t, p, "fakecloud_network", value.Null, obj("name", "n", "cidr", "10.0.0.0/24"))
	nr, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "fakecloud_network", Prior: value.Null, Planned: np.Planned, CreateToken: "tok-net"})
	if err != nil {
		t.Fatal(err)
	}
	netID := provider.StringAttr(nr.New, "id")
	ip := plan(t, p, "fakecloud_instance", value.Null, obj("name", "vm", "network_id", netID, "tags", map[string]any{"a": "b"}))
	if sz, _ := ip.Planned.Get("size"); sz.AsString() != "small" {
		t.Fatalf("default size should be known at plan time: %s", sz.GoString())
	}
	ir, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "fakecloud_instance", Prior: value.Null, Planned: ip.Planned, CreateToken: "tok-vm"})
	if err != nil {
		t.Fatal(err)
	}
	if ipaddr := provider.StringAttr(ir.New, "private_ip"); ipaddr != "10.0.0.4" {
		t.Fatalf("ip %s", ipaddr)
	}
	// In-place update sends only changed mutable fields.
	up := plan(t, p, "fakecloud_instance", ir.New, obj("name", "vm2", "network_id", netID, "size", "large", "tags", map[string]any{"a": "b"}))
	if len(up.RequiresReplace) != 0 {
		t.Fatalf("unexpected replace %v", up.RequiresReplace)
	}
	ur, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "fakecloud_instance", Prior: ir.New, Planned: up.Planned})
	if err != nil || provider.StringAttr(ur.New, "size") != "large" || provider.StringAttr(ur.New, "name") != "vm2" {
		t.Fatalf("update: %v %s", err, ur.New.GoString())
	}
	// An update with nothing to change makes no API call.
	before := srv.Stats().Requests
	if _, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "fakecloud_instance", Prior: ur.New, Planned: ur.New}); err != nil || srv.Stats().Requests != before {
		t.Fatalf("no-op update called the API: %v", err)
	}
	// Changing the network forces replacement.
	rp := plan(t, p, "fakecloud_instance", ur.New, obj("name", "vm2", "network_id", "net-9999"))
	if len(rp.RequiresReplace) != 1 {
		t.Fatalf("replace: %v", rp.RequiresReplace)
	}
	// Pending lookup by token.
	found, err := p.ReadResource(ctx, provider.ReadRequest{Type: "fakecloud_instance", Prior: ip.Planned, Pending: true, CreateToken: "tok-vm"})
	if err != nil || provider.StringAttr(found.Current, "id") != provider.StringAttr(ir.New, "id") {
		t.Fatalf("pending lookup: %v %s", err, found.Current.GoString())
	}
	missing, err := p.ReadResource(ctx, provider.ReadRequest{Type: "fakecloud_instance", Prior: ip.Planned, Pending: true, CreateToken: "nope"})
	if err != nil || !missing.Current.IsNull() {
		t.Fatalf("missing pending: %v", err)
	}
	// The network cannot be deleted while the instance is attached.
	if _, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "fakecloud_network", Prior: nr.New, Planned: value.Null}); err == nil || !strings.Contains(err.Error(), "attached") {
		t.Fatalf("expected 409: %v", err)
	}
	for _, o := range []struct {
		typ string
		v   value.Value
	}{{"fakecloud_instance", ur.New}, {"fakecloud_network", nr.New}, {"fakecloud_network", nr.New}} {
		if _, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: o.typ, Prior: o.v, Planned: value.Null}); err != nil {
			t.Fatalf("delete %s: %v", o.typ, err)
		}
	}
	gone, err := p.ReadResource(ctx, provider.ReadRequest{Type: "fakecloud_network", Prior: nr.New})
	if err != nil || !gone.Current.IsNull() {
		t.Fatalf("read deleted: %v", err)
	}
}

func TestRetriesAndIdempotency(t *testing.T) {
	p, srv := setup(t, "secret")
	ctx := context.Background()
	srv.SetFaults(fakecloud.Faults{FailBefore: map[string]int{"create:buckets": 1}, FailAfterCommit: map[string]int{"create:buckets": 1}})
	bp := plan(t, p, "fakecloud_bucket", value.Null, obj("name", "b1"))
	r, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "fakecloud_bucket", Prior: value.Null, Planned: bp.Planned, CreateToken: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Objects("buckets")); n != 1 {
		t.Fatalf("retried create duplicated the bucket: %d", n)
	}
	if arn := provider.StringAttr(r.New, "arn"); arn != "fc:bucket:b1" {
		t.Fatalf("arn %s", arn)
	}
	// Persistent failures give up after max_retries.
	srv.SetFaults(fakecloud.Faults{FailBefore: map[string]int{"update:buckets": 10}})
	up := plan(t, p, "fakecloud_bucket", r.New, obj("name", "b1", "versioning", true))
	if _, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "fakecloud_bucket", Prior: r.New, Planned: up.Planned}); err == nil || !strings.Contains(err.Error(), "giving up after 3 attempts") {
		t.Fatalf("expected give-up error: %v", err)
	}
	// Non-retryable errors surface immediately.
	dup := plan(t, p, "fakecloud_bucket", value.Null, obj("name", "b1"))
	if _, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "fakecloud_bucket", Prior: value.Null, Planned: dup.Planned, CreateToken: "t2"}); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("expected conflict: %v", err)
	}
}

func TestConfigurationAndAuth(t *testing.T) {
	ctx := context.Background()
	p := New()
	for _, bad := range []value.Value{
		obj("endpoint", "ftp://x"),
		obj("endpoint", "http://x", "max_retries", -1),
	} {
		if err := p.Configure(ctx, bad); err == nil {
			t.Errorf("accepted %s", bad.GoString())
		}
	}
	if _, err := New().ReadResource(ctx, provider.ReadRequest{Type: "fakecloud_bucket", Prior: obj("id", "x")}); err == nil {
		t.Fatal("unconfigured provider must refuse")
	}
	noAuth, _ := setup(t, "")
	if _, err := noAuth.ReadResource(ctx, provider.ReadRequest{Type: "fakecloud_bucket", Prior: obj("id", "x")}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401: %v", err)
	}
	authed, _ := setup(t, "secret")
	if _, err := authed.PlanResource(ctx, provider.PlanRequest{Type: "fakecloud_instance", Config: obj("name", "x", "network_id", "n", "size", "huge")}); err == nil {
		t.Fatal("invalid size accepted at plan time")
	}
	for _, typ := range []string{"fakecloud_nope"} {
		if _, err := authed.PlanResource(ctx, provider.PlanRequest{Type: typ}); err == nil {
			t.Fatal("unknown type planned")
		}
		if _, err := authed.ApplyResource(ctx, provider.ApplyRequest{Type: typ}); err == nil {
			t.Fatal("unknown type applied")
		}
		if _, err := authed.ReadResource(ctx, provider.ReadRequest{Type: typ}); err == nil {
			t.Fatal("unknown type read")
		}
	}
	// Reads and deletes of objects without an id are no-ops.
	r, err := authed.ReadResource(ctx, provider.ReadRequest{Type: "fakecloud_bucket", Prior: obj("name", "x")})
	if err != nil || !r.Current.IsNull() {
		t.Fatal("read without id")
	}
	if _, err := authed.ApplyResource(ctx, provider.ApplyRequest{Type: "fakecloud_bucket", Prior: obj("name", "x"), Planned: value.Null}); err != nil {
		t.Fatal(err)
	}
}
