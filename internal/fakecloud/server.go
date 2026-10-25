// Package fakecloud is a small in-memory "cloud" with a JSON/HTTP API. It
// exists so plait's provider protocol, dependency ordering, parallelism,
// idempotent creates and crash recovery can be exercised end to end without
// touching any real infrastructure.
//
// Resource kinds and their API objects:
//
//	networks:  id, name, cidr (immutable)
//	instances: id, name, size, network_id (immutable), tags, private_ip, status
//	buckets:   id, name (immutable, unique), versioning, arn
//
// The server enforces referential integrity the way a real cloud would: an
// instance needs an existing network, and a network cannot be deleted while
// instances are attached. Creates honour an Idempotency-Key header and can be
// looked up by it (GET /v1/{kind}?create_token=KEY).
package fakecloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Object is one stored API object.
type Object map[string]any

// Event records a committed mutation, for ordering assertions and demos.
type Event struct {
	Seq  int    `json:"seq"`
	Op   string `json:"op"` // create, update, delete
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Stats summarizes server activity.
type Stats struct {
	Requests    int            `json:"requests"`
	MaxInFlight int            `json:"max_in_flight"`
	Objects     map[string]int `json:"objects"`
	Events      int            `json:"events"`
}

// Faults configures failure injection. Keys are "<op>:<kind>", e.g.
// "create:instances".
type Faults struct {
	// FailBefore makes the next N matching requests fail with 503 before
	// doing any work (a transient error the client should retry).
	FailBefore map[string]int `json:"fail_before,omitempty"`
	// FailAfterCommit makes the next N matching requests commit their change
	// and then fail with 500, so the client cannot know it succeeded.
	FailAfterCommit map[string]int `json:"fail_after_commit,omitempty"`
	// LatencyMS delays every API request.
	LatencyMS int `json:"latency_ms,omitempty"`
}

// Server is the fake cloud.
type Server struct {
	mu       sync.Mutex
	objs     map[string]map[string]Object
	seq      map[string]int
	tokens   map[string]string // kind + "/" + token -> id
	nextHost map[string]uint32 // network id -> next host offset
	events   []Event
	faults   Faults
	requests int
	inFlight int
	maxIn    int
	token    string

	// OnCommit, if set, runs after a mutation commits and before the
	// response is written. Tests use it to simulate a client crashing at
	// the worst moment.
	OnCommit func(ev Event)
}

var prefixes = map[string]string{"networks": "net", "instances": "i", "buckets": "bkt"}

// New returns an empty server. A non-empty token requires
// "Authorization: Bearer <token>" on every API request.
func New(token string) *Server {
	s := &Server{token: token}
	s.reset()
	return s
}

func (s *Server) reset() {
	s.objs = map[string]map[string]Object{}
	for k := range prefixes {
		s.objs[k] = map[string]Object{}
	}
	s.seq = map[string]int{}
	s.tokens = map[string]string{}
	s.nextHost = map[string]uint32{}
	s.events = nil
	s.faults = Faults{}
}

// SetFaults replaces the fault configuration.
func (s *Server) SetFaults(f Faults) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = f
}

// Events returns the committed mutation log.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// Stats returns activity counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Requests: s.requests, MaxInFlight: s.maxIn, Objects: map[string]int{}, Events: len(s.events)}
	for k, m := range s.objs {
		st.Objects[k] = len(m)
	}
	return st
}

// Objects returns copies of all objects of a kind, sorted by id.
func (s *Server) Objects(kind string) []Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Object
	for _, o := range s.objs[kind] {
		out = append(out, clone(o))
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out
}

func clone(o Object) Object {
	cp := make(Object, len(o))
	for k, v := range o {
		if m, ok := v.(map[string]any); ok {
			mc := make(map[string]any, len(m))
			for mk, mv := range m {
				mc[mk] = mv
			}
			v = mc
		}
		cp[k] = v
	}
	return cp
}

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func errf(code int, format string, args ...any) error {
	return &apiError{code: code, msg: fmt.Sprintf(format, args...)}
}

// ServeHTTP routes API requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	switch {
	case parts[1] == "_stats" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.Stats())
		return
	case parts[1] == "_events" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.Events())
		return
	case parts[1] == "_faults" && r.Method == http.MethodPost:
		var f Faults
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.SetFaults(f)
		writeJSON(w, http.StatusOK, f)
		return
	}
	kind := parts[1]
	if _, ok := prefixes[kind]; !ok || len(parts) > 3 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown resource kind " + kind})
		return
	}
	id := ""
	if len(parts) == 3 {
		id = parts[2]
	}
	s.track(+1)
	defer s.track(-1)
	if d := s.latency(); d > 0 {
		time.Sleep(d)
	}
	var (
		status = http.StatusOK
		body   any
		ev     *Event
		err    error
		op     string
	)
	switch {
	case r.Method == http.MethodPost && id == "":
		op = "create"
		if err = s.fault("FailBefore", op, kind); err == nil {
			body, ev, status, err = s.create(kind, r)
		}
	case r.Method == http.MethodGet && id == "":
		body, err = s.list(kind, r.URL.Query().Get("create_token"))
	case r.Method == http.MethodGet:
		body, err = s.get(kind, id)
	case r.Method == http.MethodPatch:
		op = "update"
		if err = s.fault("FailBefore", op, kind); err == nil {
			body, ev, err = s.update(kind, id, r)
		}
	case r.Method == http.MethodDelete:
		op = "delete"
		if err = s.fault("FailBefore", op, kind); err == nil {
			ev, err = s.delete(kind, id)
			status = http.StatusNoContent
		}
	default:
		err = errf(http.StatusMethodNotAllowed, "method %s not allowed", r.Method)
	}
	if err != nil {
		var ae *apiError
		code := http.StatusInternalServerError
		if errors.As(err, &ae) {
			code = ae.code
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	if ev != nil {
		if hook := s.OnCommit; hook != nil {
			hook(*ev)
		}
		if ferr := s.fault("FailAfterCommit", op, kind); ferr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error after commit"})
			return
		}
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, body)
}

func (s *Server) track(delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if delta > 0 {
		s.requests++
	}
	s.inFlight += delta
	if s.inFlight > s.maxIn {
		s.maxIn = s.inFlight
	}
}

func (s *Server) latency() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Duration(s.faults.LatencyMS) * time.Millisecond
}

// fault consumes one injected failure of the given class if configured.
func (s *Server) fault(class, op, kind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.faults.FailBefore
	code := http.StatusServiceUnavailable
	if class == "FailAfterCommit" {
		m = s.faults.FailAfterCommit
		code = http.StatusInternalServerError
	}
	key := op + ":" + kind
	if m[key] > 0 {
		m[key]--
		return errf(code, "injected fault (%s %s)", class, key)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func decodeBody(r *http.Request) (map[string]any, error) {
	var in map[string]any
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&in); err != nil {
		return nil, errf(http.StatusBadRequest, "invalid JSON body: %v", err)
	}
	return in, nil
}

func (s *Server) commit(op, kind, id string) *Event {
	ev := Event{Seq: len(s.events) + 1, Op: op, Kind: kind, ID: id}
	s.events = append(s.events, ev)
	return &ev
}

func (s *Server) create(kind string, r *http.Request) (any, *Event, int, error) {
	in, err := decodeBody(r)
	if err != nil {
		return nil, nil, 0, err
	}
	key := r.Header.Get("Idempotency-Key")
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		if id, ok := s.tokens[kind+"/"+key]; ok {
			if o, live := s.objs[kind][id]; live {
				return clone(o), nil, http.StatusOK, nil // replayed create
			}
		}
	}
	obj, err := s.validateNew(kind, in)
	if err != nil {
		return nil, nil, 0, err
	}
	s.seq[kind]++
	id := fmt.Sprintf("%s-%04d", prefixes[kind], s.seq[kind])
	obj["id"] = id
	switch kind {
	case "instances":
		ip, err := s.allocateIP(obj["network_id"].(string))
		if err != nil {
			s.seq[kind]--
			return nil, nil, 0, err
		}
		obj["private_ip"] = ip
		obj["status"] = "running"
	case "buckets":
		obj["arn"] = "fc:bucket:" + obj["name"].(string)
	}
	if key != "" {
		obj["create_token"] = key
		s.tokens[kind+"/"+key] = id
	}
	s.objs[kind][id] = obj
	return clone(obj), s.commit("create", kind, id), http.StatusCreated, nil
}

var sizes = map[string]bool{"small": true, "medium": true, "large": true}

func str(in map[string]any, k string, required bool) (string, error) {
	v, ok := in[k]
	if !ok || v == nil {
		if required {
			return "", errf(http.StatusUnprocessableEntity, "%s is required", k)
		}
		return "", nil
	}
	sv, ok := v.(string)
	if !ok || (required && sv == "") {
		return "", errf(http.StatusUnprocessableEntity, "%s must be a non-empty string", k)
	}
	return sv, nil
}

// validateNew checks a create body and returns the stored fields.
func (s *Server) validateNew(kind string, in map[string]any) (Object, error) {
	obj := Object{}
	name, err := str(in, "name", true)
	if err != nil {
		return nil, err
	}
	obj["name"] = name
	switch kind {
	case "networks":
		cidr, err := str(in, "cidr", true)
		if err != nil {
			return nil, err
		}
		ip, n, perr := net.ParseCIDR(cidr)
		if perr != nil || ip.To4() == nil || !ip.Equal(n.IP) {
			return nil, errf(http.StatusUnprocessableEntity, "cidr %q must be an IPv4 network address such as 10.0.0.0/16", cidr)
		}
		if ones, _ := n.Mask.Size(); ones > 28 {
			return nil, errf(http.StatusUnprocessableEntity, "cidr %q is too small (max /28)", cidr)
		}
		obj["cidr"] = cidr
	case "instances":
		netID, err := str(in, "network_id", true)
		if err != nil {
			return nil, err
		}
		if _, ok := s.objs["networks"][netID]; !ok {
			return nil, errf(http.StatusUnprocessableEntity, "network %s does not exist", netID)
		}
		obj["network_id"] = netID
		size, err := str(in, "size", false)
		if err != nil {
			return nil, err
		}
		if size == "" {
			size = "small"
		}
		if !sizes[size] {
			return nil, errf(http.StatusUnprocessableEntity, "size %q is not one of small, medium, large", size)
		}
		obj["size"] = size
		tags, err := tagMap(in["tags"])
		if err != nil {
			return nil, err
		}
		obj["tags"] = tags
	case "buckets":
		for _, o := range s.objs["buckets"] {
			if o["name"] == name {
				return nil, errf(http.StatusConflict, "bucket name %q is already taken by %s", name, o["id"])
			}
		}
		obj["versioning"] = in["versioning"] == true
	}
	return obj, nil
}

func tagMap(v any) (map[string]any, error) {
	out := map[string]any{}
	if v == nil {
		return out, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errf(http.StatusUnprocessableEntity, "tags must be an object of strings")
	}
	for k, tv := range m {
		sv, ok := tv.(string)
		if !ok {
			return nil, errf(http.StatusUnprocessableEntity, "tag %q must be a string", k)
		}
		out[k] = sv
	}
	return out, nil
}

func (s *Server) allocateIP(netID string) (string, error) {
	_, n, err := net.ParseCIDR(s.objs["networks"][netID]["cidr"].(string))
	if err != nil {
		return "", err
	}
	ones, _ := n.Mask.Size()
	size := uint32(1) << (32 - ones)
	off := s.nextHost[netID]
	if off == 0 {
		off = 4 // like most clouds, reserve the first addresses
	}
	if off >= size-1 {
		return "", errf(http.StatusConflict, "network %s has no free addresses", netID)
	}
	s.nextHost[netID] = off + 1
	b := n.IP.To4()
	base := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	ip := base + off
	return net.IPv4(byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip)).String(), nil
}

func (s *Server) list(kind, token string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Object{}
	for _, o := range s.objs[kind] {
		if token == "" || o["create_token"] == token {
			out = append(out, clone(o))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out, nil
}

func (s *Server) get(kind, id string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[kind][id]
	if !ok {
		return nil, errf(http.StatusNotFound, "%s %s not found", kind, id)
	}
	return clone(o), nil
}

var mutable = map[string]map[string]bool{
	"networks":  {"name": true},
	"instances": {"name": true, "size": true, "tags": true},
	"buckets":   {"versioning": true},
}

func (s *Server) update(kind, id string, r *http.Request) (any, *Event, error) {
	in, err := decodeBody(r)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[kind][id]
	if !ok {
		return nil, nil, errf(http.StatusNotFound, "%s %s not found", kind, id)
	}
	next := clone(o)
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := in[k]
		if !mutable[kind][k] {
			if cur, exists := o[k]; exists && fmt.Sprint(cur) == fmt.Sprint(v) {
				continue // unchanged immutable fields are accepted
			}
			return nil, nil, errf(http.StatusBadRequest, "%s.%s cannot be changed in place", kind, k)
		}
		switch k {
		case "name":
			sv, err := str(in, k, true)
			if err != nil {
				return nil, nil, err
			}
			next[k] = sv
		case "size":
			sv, _ := v.(string)
			if !sizes[sv] {
				return nil, nil, errf(http.StatusUnprocessableEntity, "size %q is not one of small, medium, large", v)
			}
			next[k] = sv
		case "tags":
			tags, err := tagMap(v)
			if err != nil {
				return nil, nil, err
			}
			next[k] = tags
		case "versioning":
			next[k] = v == true
		}
	}
	s.objs[kind][id] = next
	return clone(next), s.commit("update", kind, id), nil
}

func (s *Server) delete(kind, id string) (*Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objs[kind][id]; !ok {
		return nil, errf(http.StatusNotFound, "%s %s not found", kind, id)
	}
	if kind == "networks" {
		var attached []string
		for _, inst := range s.objs["instances"] {
			if inst["network_id"] == id {
				attached = append(attached, inst["id"].(string))
			}
		}
		if len(attached) > 0 {
			sort.Strings(attached)
			return nil, errf(http.StatusConflict, "network %s still has attached instances: %s", id, strings.Join(attached, ", "))
		}
	}
	if tok, ok := s.objs[kind][id]["create_token"].(string); ok {
		delete(s.tokens, kind+"/"+tok)
	}
	delete(s.objs[kind], id)
	return s.commit("delete", kind, id), nil
}
