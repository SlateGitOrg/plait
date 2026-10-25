package fakecloud

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type client struct {
	t   *testing.T
	url string
}

func (c client) do(method, path, body string, hdr ...string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.url+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tk")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func newClient(t *testing.T) (client, *Server) {
	s := New("tk")
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	return client{t, hs.URL}, s
}

func TestServerValidationAndIntegrity(t *testing.T) {
	c, s := newClient(t)
	cases := []struct {
		path, body string
		code       int
	}{
		{"/v1/networks", `{"name":"n","cidr":"10.0.0.1/16"}`, 422},
		{"/v1/networks", `{"name":"n","cidr":"10.0.0.0/30"}`, 422},
		{"/v1/networks", `{"cidr":"10.0.0.0/16"}`, 422},
		{"/v1/networks", `not json`, 400},
		{"/v1/instances", `{"name":"i","network_id":"net-9"}`, 422},
		{"/v1/nope", `{}`, 404},
	}
	for _, tc := range cases {
		if code, body := c.do("POST", tc.path, tc.body); code != tc.code {
			t.Errorf("POST %s %s: %d %v", tc.path, tc.body, code, body)
		}
	}
	code, net := c.do("POST", "/v1/networks", `{"name":"n","cidr":"10.0.0.0/28"}`)
	if code != 201 || net["id"] != "net-0001" {
		t.Fatalf("create network %d %v", code, net)
	}
	for i, want := range []int{201, 201, 201, 201, 201, 201, 201, 201, 201, 201, 201, 409} {
		code, body := c.do("POST", "/v1/instances", `{"name":"i","network_id":"net-0001","tags":{"k":"v"}}`)
		if code != want {
			t.Fatalf("instance %d: %d %v", i, code, body)
		}
	}
	if code, _ := c.do("POST", "/v1/instances", `{"name":"i","network_id":"net-0001","size":"huge"}`); code != 422 {
		t.Fatal("size validated")
	}
	if code, _ := c.do("POST", "/v1/instances", `{"name":"i","network_id":"net-0001","tags":{"k":1}}`); code != 422 {
		t.Fatal("tags validated")
	}
	if code, body := c.do("DELETE", "/v1/networks/net-0001", ""); code != 409 || !strings.Contains(body["error"].(string), "i-0001") {
		t.Fatalf("attached delete: %d %v", code, body)
	}
	if code, _ := c.do("PATCH", "/v1/instances/i-0001", `{"network_id":"net-0002"}`); code != 400 {
		t.Fatal("immutable field changed")
	}
	if code, body := c.do("PATCH", "/v1/instances/i-0001", `{"network_id":"net-0001","size":"large","tags":{"a":"b"}}`); code != 200 || body["size"] != "large" {
		t.Fatalf("patch: %d %v", code, body)
	}
	if code, _ := c.do("PATCH", "/v1/instances/i-0001", `{"size":"tiny"}`); code != 422 {
		t.Fatal("patch size validated")
	}
	if code, _ := c.do("GET", "/v1/instances/i-9999", ""); code != 404 {
		t.Fatal("get missing")
	}
	if code, _ := c.do("PUT", "/v1/instances/i-0001", ""); code != 405 {
		t.Fatal("method check")
	}
	if code, _ := c.do("POST", "/v1/buckets", `{"name":"b"}`); code != 201 {
		t.Fatal("bucket")
	}
	if code, _ := c.do("POST", "/v1/buckets", `{"name":"b"}`); code != 409 {
		t.Fatal("bucket names are unique")
	}
	if code, body := c.do("PATCH", "/v1/buckets/bkt-0001", `{"versioning":true}`); code != 200 || body["versioning"] != true {
		t.Fatal("bucket versioning")
	}
	st := s.Stats()
	if st.Objects["instances"] != 11 || st.Objects["networks"] != 1 || st.Events == 0 || st.Requests == 0 {
		t.Fatalf("stats %+v", st)
	}
	if len(s.Objects("instances")) != 11 || len(s.Events()) != st.Events {
		t.Fatal("accessors")
	}
}

func TestIdempotencyFaultsAndAdmin(t *testing.T) {
	c, s := newClient(t)
	c.do("POST", "/v1/networks", `{"name":"n","cidr":"10.0.0.0/16"}`)
	code1, a := c.do("POST", "/v1/buckets", `{"name":"x"}`, "Idempotency-Key", "k1")
	code2, b := c.do("POST", "/v1/buckets", `{"name":"x"}`, "Idempotency-Key", "k1")
	if code1 != 201 || code2 != 200 || a["id"] != b["id"] {
		t.Fatalf("replay: %d %d %v %v", code1, code2, a, b)
	}
	req, _ := http.NewRequest("GET", c.url+"/v1/buckets?create_token=k1", nil)
	req.Header.Set("Authorization", "Bearer tk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	_ = resp.Body.Close()
	if len(list) != 1 {
		t.Fatalf("token lookup %v", list)
	}
	// Admin endpoints: faults, stats, events.
	if code, _ := c.do("POST", "/v1/_faults", `{"fail_before":{"delete:buckets":1},"fail_after_commit":{"update:buckets":1}}`); code != 200 {
		t.Fatal("set faults")
	}
	if code, _ := c.do("DELETE", "/v1/buckets/bkt-0001", ""); code != 503 {
		t.Fatal("fail before")
	}
	if code, _ := c.do("PATCH", "/v1/buckets/bkt-0001", `{"versioning":true}`); code != 500 {
		t.Fatal("fail after commit")
	}
	if code, _ := c.do("DELETE", "/v1/buckets/bkt-0001", ""); code != 204 {
		t.Fatal("delete after faults")
	}
	if code, _ := c.do("DELETE", "/v1/buckets/bkt-0001", ""); code != 404 {
		t.Fatal("double delete")
	}
	// After deletion the token no longer replays.
	if code, _ := c.do("POST", "/v1/buckets", `{"name":"x"}`, "Idempotency-Key", "k1"); code != 201 {
		t.Fatal("token after delete")
	}
	if code, body := c.do("GET", "/v1/_stats", ""); code != 200 || body["requests"] == nil {
		t.Fatal("stats endpoint")
	}
	if code, _ := c.do("POST", "/v1/_faults", `nope`); code != 400 {
		t.Fatal("bad faults body")
	}
	if code, _ := c.do("GET", "/elsewhere", ""); code != 404 {
		t.Fatal("unknown path")
	}
	req, _ = http.NewRequest("GET", c.url+"/v1/_events", nil)
	resp, _ = http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("auth required")
	}
	var seen []Event
	s.SetOnCommit(func(ev Event) { seen = append(seen, ev) })
	c.do("POST", "/v1/buckets", `{"name":"y"}`)
	s.SetOnCommit(nil)
	if len(seen) != 1 || seen[0].Op != "create" {
		t.Fatalf("hook %v", seen)
	}
}
