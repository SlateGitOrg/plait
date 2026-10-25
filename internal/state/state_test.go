package state

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archit2501/plait/internal/addr"
	"github.com/archit2501/plait/internal/value"
)

func mustAddr(t *testing.T, s string) addr.Instance {
	t.Helper()
	a, err := addr.ParseInstance(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func sampleState(t *testing.T) *State {
	st := New()
	st.Set(&Instance{Addr: mustAddr(t, `fakecloud_network.main`), Provider: "fakecloud",
		Current: &Object{Status: StatusReady, Attrs: value.Map(map[string]value.Value{"id": value.String("net-0001")})}})
	st.Set(&Instance{Addr: mustAddr(t, `fakecloud_instance.web[1]`), Provider: "fakecloud",
		Current:      &Object{Status: StatusPending, Attrs: value.Map(nil), CreateToken: "tok"},
		Dependencies: []string{"fakecloud_network.main"}})
	st.Set(&Instance{Addr: mustAddr(t, `fakecloud_bucket.b["logs"]`), Provider: "fakecloud",
		Current: &Object{Status: StatusReady, Attrs: value.Map(nil)},
		Deposed: &Object{Status: StatusTainted, Attrs: value.Map(nil)}})
	st.Outputs["ip"] = value.String("10.0.0.5")
	return st
}

func TestStateJSONRoundTrip(t *testing.T) {
	st := sampleState(t)
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(&back)
	if string(again) != string(data) {
		t.Fatalf("round trip differs:\n%s\n%s", data, again)
	}
	order := []string{}
	for _, i := range back.Sorted() {
		order = append(order, i.Addr.String())
	}
	if strings.Join(order, " ") != `fakecloud_bucket.b["logs"] fakecloud_instance.web[1] fakecloud_network.main` {
		t.Fatalf("order %v", order)
	}
	if !strings.Contains(string(data), `"index_key":1`) || !strings.Contains(string(data), `"create_token":"tok"`) {
		t.Fatalf("expected index key and create token in %s", data)
	}
	if len(back.ResourceInstances(addr.Resource{Type: "fakecloud_instance", Name: "web"})) != 1 {
		t.Fatal("ResourceInstances")
	}
}

func TestStateRejectsInvalid(t *testing.T) {
	st := New()
	st.Set(&Instance{Addr: mustAddr(t, "a_b.c"), Current: &Object{Status: StatusReady, Attrs: value.Map(map[string]value.Value{"x": value.Unknown})}})
	if _, err := json.Marshal(st); err == nil {
		t.Fatal("unknowns must not be persisted")
	}
	for name, doc := range map[string]string{
		"version":   `{"version":9,"lineage":"x","serial":1}`,
		"lineage":   `{"version":1,"serial":1}`,
		"address":   `{"version":1,"lineage":"x","resources":[{"address":"bad","current":{"status":"ready"}}]}`,
		"noobjects": `{"version":1,"lineage":"x","resources":[{"address":"a_b.c"}]}`,
		"status":    `{"version":1,"lineage":"x","resources":[{"address":"a_b.c","current":{"status":"weird"}}]}`,
		"dup":       `{"version":1,"lineage":"x","resources":[{"address":"a_b.c","current":{"status":"ready"}},{"address":"a_b.c","current":{"status":"ready"}}]}`,
		"json":      `{`,
	} {
		var s State
		if err := json.Unmarshal([]byte(doc), &s); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCloneIsIndependent(t *testing.T) {
	st := sampleState(t)
	cp := st.Clone()
	cp.Remove(mustAddr(t, "fakecloud_network.main"))
	cp.Get(mustAddr(t, "fakecloud_instance.web[1]")).Current.Status = StatusReady
	cp.Outputs["ip"] = value.Null
	if st.Get(mustAddr(t, "fakecloud_network.main")) == nil ||
		st.Get(mustAddr(t, "fakecloud_instance.web[1]")).Current.Status != StatusPending ||
		st.Outputs["ip"].IsNull() {
		t.Fatal("clone shares data with original")
	}
	if st.Empty() || !New().Empty() {
		t.Fatal("Empty")
	}
}

func TestStoreSaveLoadAtomic(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "nested", "plait.state.json")}
	st, err := s.Load()
	if err != nil || !st.Empty() || st.Serial != 0 {
		t.Fatalf("missing file should load empty state: %v", err)
	}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	st.Outputs["v"] = value.Int(2)
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	back, err := s.Load()
	if err != nil || back.Serial != 2 || back.Lineage != st.Lineage {
		t.Fatalf("reload: serial=%d err=%v", back.Serial, err)
	}
	if _, err := os.Stat(s.Path + ".backup"); err != nil {
		t.Fatal("backup not written")
	}
	entries, _ := os.ReadDir(filepath.Dir(s.Path))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	// A failed save must not bump the serial.
	st.Set(&Instance{Addr: mustAddr(t, "a_b.c"), Current: &Object{Status: StatusReady, Attrs: value.Unknown}})
	if err := s.Save(st); err == nil || st.Serial != 2 {
		t.Fatalf("save with unknown should fail without bumping serial: %v serial=%d", err, st.Serial)
	}
	if err := os.WriteFile(s.Path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("garbage state should not load")
	}
}

func TestLockLifecycle(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "st.json")}
	l, err := s.Lock("apply", 0)
	if err != nil {
		t.Fatal(err)
	}
	if l.BrokeStale != nil {
		t.Fatal("no stale lock expected")
	}
	_, err = s.Lock("plan", 150*time.Millisecond)
	var le *LockedError
	if !errors.As(err, &le) || le.Info.Operation != "apply" || !strings.Contains(err.Error(), "force-unlock") {
		t.Fatalf("expected LockedError, got %v", err)
	}
	if err := s.ForceUnlock("wrong"); err == nil {
		t.Fatal("force-unlock with wrong ID must fail")
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err == nil {
		t.Fatal("double unlock should fail")
	}
	if err := s.ForceUnlock("x"); err == nil {
		t.Fatal("force-unlock without lock should fail")
	}
	l2, err := s.Lock("apply", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ForceUnlock(l2.Info.ID); err != nil {
		t.Fatal(err)
	}
}

func TestStaleLockFromDeadProcessIsBroken(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "st.json")}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot spawn helper process")
	}
	host, _ := os.Hostname()
	dead := LockInfo{ID: "deadbeef", Operation: "apply", PID: cmd.Process.Pid, Host: host, Created: time.Now()}
	data, _ := json.Marshal(dead)
	if err := os.WriteFile(s.LockPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := s.Lock("apply", 0)
	if err != nil {
		t.Fatalf("stale lock should be broken: %v", err)
	}
	if l.BrokeStale == nil || l.BrokeStale.ID != "deadbeef" {
		t.Fatalf("BrokeStale not reported: %+v", l.BrokeStale)
	}
	_ = l.Unlock()

	// A lock from another host is never broken automatically.
	other := LockInfo{ID: "remote", Operation: "apply", PID: cmd.Process.Pid, Host: host + "-elsewhere"}
	data, _ = json.Marshal(other)
	if err := os.WriteFile(s.LockPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock("apply", 0); err == nil {
		t.Fatal("foreign-host lock must not be broken")
	}
	if err := os.WriteFile(s.LockPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock("apply", 0); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("corrupt lock: %v", err)
	}
}
