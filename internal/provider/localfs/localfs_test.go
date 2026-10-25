package localfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/archit2501/plait/internal/provider"
	"github.com/archit2501/plait/internal/value"
)

func configured(t *testing.T) (provider.Provider, string) {
	t.Helper()
	dir := t.TempDir()
	p := NewAt(dir)
	if err := p.Configure(context.Background(), value.Map(map[string]value.Value{"root": value.String("sub")})); err != nil {
		t.Fatal(err)
	}
	return p, filepath.Join(dir, "sub")
}

func cfg(kv ...string) value.Value {
	m := map[string]value.Value{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = value.String(kv[i+1])
	}
	return value.Map(m)
}

func TestFileLifecycle(t *testing.T) {
	p, root := configured(t)
	ctx := context.Background()
	plan, err := p.PlanResource(ctx, provider.PlanRequest{Type: "local_file", Prior: value.Null, Config: cfg("path", "a/b.txt", "content", "hi")})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Planned.IsWhollyKnown() {
		t.Fatalf("file plans should be fully known with known content: %s", plan.Planned.GoString())
	}
	resp, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "local_file", Prior: value.Null, Planned: plan.Planned})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.New.Equal(plan.Planned) {
		t.Fatalf("result differs from plan:\n%s\n%s", resp.New.GoString(), plan.Planned.GoString())
	}
	data, err := os.ReadFile(filepath.Join(root, "a/b.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("file content %q %v", data, err)
	}
	// Update content and permission in place.
	upd, err := p.PlanResource(ctx, provider.PlanRequest{Type: "local_file", Prior: resp.New,
		Config: cfg("path", "a/b.txt", "content", "bye", "file_permission", "0600")})
	if err != nil || len(upd.RequiresReplace) != 0 {
		t.Fatalf("update plan: %v %v", err, upd.RequiresReplace)
	}
	resp2, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "local_file", Prior: resp.New, Planned: upd.Planned})
	if err != nil {
		t.Fatal(err)
	}
	if fp, _ := resp2.New.Get("file_permission"); fp.AsString() != "0600" {
		t.Fatalf("perm %s", fp.GoString())
	}
	// Moving the file forces replacement.
	mv, _ := p.PlanResource(ctx, provider.PlanRequest{Type: "local_file", Prior: resp2.New, Config: cfg("path", "c.txt", "content", "bye")})
	if len(mv.RequiresReplace) != 1 || mv.RequiresReplace[0] != "path" {
		t.Fatalf("replace: %v", mv.RequiresReplace)
	}
	// Drift: content changed out of band is visible on read.
	if err := os.WriteFile(filepath.Join(root, "a/b.txt"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	rd, err := p.ReadResource(ctx, provider.ReadRequest{Type: "local_file", Prior: resp2.New})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := rd.Current.Get("content"); c.AsString() != "edited" {
		t.Fatalf("read content %s", c.GoString())
	}
	// Pending reads find the object by path.
	pend, err := p.ReadResource(ctx, provider.ReadRequest{Type: "local_file", Prior: cfg("path", "a/b.txt"), Pending: true})
	if err != nil || pend.Current.IsNull() {
		t.Fatalf("pending read: %v", err)
	}
	if _, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "local_file", Prior: resp2.New, Planned: value.Null}); err != nil {
		t.Fatal(err)
	}
	gone, _ := p.ReadResource(ctx, provider.ReadRequest{Type: "local_file", Prior: resp2.New})
	if !gone.Current.IsNull() {
		t.Fatal("file should be gone")
	}
	// Deleting an already-missing file is not an error.
	if _, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "local_file", Prior: resp2.New, Planned: value.Null}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryAndValidation(t *testing.T) {
	p, root := configured(t)
	ctx := context.Background()
	plan, err := p.PlanResource(ctx, provider.PlanRequest{Type: "local_directory", Prior: value.Null, Config: cfg("path", "d")})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.ApplyResource(ctx, provider.ApplyRequest{Type: "local_directory", Prior: value.Null, Planned: plan.Planned})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(root, "d")); err != nil || !st.IsDir() {
		t.Fatalf("dir: %v", err)
	}
	if perm, _ := resp.New.Get("permission"); perm.AsString() != "0755" {
		t.Fatalf("perm %s", perm.GoString())
	}
	for _, tc := range []struct {
		typ  string
		cfg  value.Value
		want string
	}{
		{"local_file", cfg("path", "../escape.txt", "content", "x"), "escapes the provider root"},
		{"local_file", cfg("path", "x", "content", "x", "file_permission", "999"), "invalid permission"},
		{"local_directory", cfg("path", "/etc"), "escapes"},
		{"local_nope", cfg("path", "x"), "unsupported resource type"},
	} {
		if _, err := p.PlanResource(ctx, provider.PlanRequest{Type: tc.typ, Prior: value.Null, Config: tc.cfg}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: want %q, got %v", tc.typ, tc.cfg.GoString(), tc.want, err)
		}
	}
	// A file where a directory is expected is reported, not overwritten.
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadResource(ctx, provider.ReadRequest{Type: "local_directory", Prior: cfg("path", "f")}); err == nil {
		t.Fatal("expected not-a-directory error")
	}
	if _, err := New().PlanResource(ctx, provider.PlanRequest{Type: "local_file", Config: cfg("path", "x", "content", "y")}); err == nil {
		t.Fatal("unconfigured provider must refuse")
	}
	if len(p.Schema().Resources) != 2 {
		t.Fatal("schema")
	}
}
