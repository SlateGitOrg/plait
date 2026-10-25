// Package integration runs plait end to end against the in-repo fake cloud
// (served by httptest) and the local filesystem provider.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SlateGitOrg/plait/internal/config"
	"github.com/SlateGitOrg/plait/internal/engine"
	"github.com/SlateGitOrg/plait/internal/fakecloud"
	"github.com/SlateGitOrg/plait/internal/state"
	"github.com/SlateGitOrg/plait/internal/workspace"
)

// env is one test workspace: a config directory, a fake cloud and state.
type env struct {
	t     *testing.T
	cloud *fakecloud.Server
	http  *httptest.Server
	dir   string
	vars  []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cloud := fakecloud.New("test-token")
	srv := httptest.NewServer(cloud)
	t.Cleanup(srv.Close)
	return &env{t: t, cloud: cloud, http: srv, dir: t.TempDir()}
}

// write replaces the configuration with src. The literal ENDPOINT is
// replaced with the fake cloud URL.
func (e *env) write(src string) {
	e.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(e.dir, "*.plait"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			e.t.Fatal(err)
		}
	}
	src = strings.ReplaceAll(src, "ENDPOINT", e.http.URL)
	if err := os.WriteFile(filepath.Join(e.dir, "main.plait"), []byte(src), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) workspace(parallelism int) *workspace.Workspace {
	e.t.Helper()
	ws, err := workspace.Open(workspace.Options{Dir: e.dir, Parallelism: parallelism,
		Vars: config.VarInputs{Flags: e.vars}, LockTimeout: time.Second,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		e.t.Fatal(err)
	}
	return ws
}

// plan returns a plan and keeps the session open for apply.
func (e *env) plan(opts engine.PlanOptions) (*workspace.Session, *engine.Plan) {
	e.t.Helper()
	s, err := e.workspace(4).Begin("plan")
	if err != nil {
		e.t.Fatal(err)
	}
	p, err := s.Plan(context.Background(), opts)
	if err != nil {
		_ = s.Close()
		e.t.Fatal(err)
	}
	return s, p
}

// apply plans and applies, failing the test on any error.
func (e *env) apply(opts engine.PlanOptions) (*engine.Plan, *engine.ApplyResult) {
	e.t.Helper()
	s, p := e.plan(opts)
	defer func() { _ = s.Close() }()
	res, err := s.Apply(context.Background(), p)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := res.Err(); err != nil {
		e.t.Fatalf("apply failed: %v", err)
	}
	return p, res
}

func (e *env) state() *state.State {
	e.t.Helper()
	st, err := e.workspace(1).ReadState()
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) render(p *engine.Plan) string {
	e.t.Helper()
	var b strings.Builder
	if err := engine.Render(&b, p, engine.RenderOptions{}); err != nil {
		e.t.Fatal(err)
	}
	return b.String()
}

func counts(p *engine.Plan) [3]int {
	a, c, d := p.Counts()
	return [3]int{a, c, d}
}

// cloudDo sends a raw API request to the fake cloud (out-of-band changes).
func (e *env) cloudDo(method, path, body string) (int, error) {
	req, err := http.NewRequest(method, e.http.URL+path, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %d", method, path, resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// idOf returns the remote id of a tracked instance.
func (e *env) idOf(address string) string {
	e.t.Helper()
	inst := e.state().Instances[address]
	if inst == nil || inst.Current == nil {
		e.t.Fatalf("%s not in state", address)
	}
	v, _ := inst.Current.Attrs.Get("id")
	return v.AsString()
}
