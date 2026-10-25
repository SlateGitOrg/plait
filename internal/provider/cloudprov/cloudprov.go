// Package cloudprov is the provider for the in-repo fake cloud HTTP API
// (see internal/fakecloud). It is written the way a real cloud provider is:
// it translates schema objects to API calls, retries transient failures,
// sends an idempotency key with every create, and can find an object whose
// create was interrupted by looking it up through that key.
//
// Resources:
//
//	fakecloud_network:  name, cidr (force new); computed id
//	fakecloud_instance: name, size, network_id (force new), tags;
//	                    computed id, private_ip, status
//	fakecloud_bucket:   name (force new), versioning; computed id, arn
package cloudprov

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SlateGitOrg/plait/internal/provider"
	"github.com/SlateGitOrg/plait/internal/value"
)

// Name is the provider name (resource type prefix).
const Name = "fakecloud"

var (
	tString    = value.Type{Kind: value.TypeString}
	tNumber    = value.Type{Kind: value.TypeNumber}
	tBool      = value.Type{Kind: value.TypeBool}
	tStringMap = value.Type{Kind: value.TypeMap, Elem: &value.Type{Kind: value.TypeString}}
)

// kinds maps resource types to API collection names.
var kinds = map[string]string{
	"fakecloud_network":  "networks",
	"fakecloud_instance": "instances",
	"fakecloud_bucket":   "buckets",
}

var schema = provider.Schema{
	Provider: provider.ResourceSchema{Attributes: []provider.Attribute{
		{Name: "endpoint", Type: tString, Required: true, Description: "base URL of the fake cloud API, e.g. http://127.0.0.1:8787"},
		{Name: "token", Type: tString, Optional: true, Description: "bearer token, if the server requires one"},
		{Name: "max_retries", Type: tNumber, Optional: true, Description: "retries for transient (429/5xx) failures; default 4"},
		{Name: "retry_base_ms", Type: tNumber, Optional: true, Description: "base backoff between retries in milliseconds; default 50"},
	}},
	Resources: map[string]provider.ResourceSchema{
		"fakecloud_network": {Description: "an isolated IPv4 network", Attributes: []provider.Attribute{
			{Name: "name", Type: tString, Required: true},
			{Name: "cidr", Type: tString, Required: true, ForceNew: true},
			{Name: "id", Type: tString, Computed: true},
		}},
		"fakecloud_instance": {Description: "a virtual machine attached to a network", Attributes: []provider.Attribute{
			{Name: "name", Type: tString, Required: true},
			{Name: "size", Type: tString, Optional: true, Computed: true},
			{Name: "network_id", Type: tString, Required: true, ForceNew: true},
			{Name: "tags", Type: tStringMap, Optional: true, Computed: true},
			{Name: "id", Type: tString, Computed: true},
			{Name: "private_ip", Type: tString, Computed: true},
			{Name: "status", Type: tString, Computed: true},
		}},
		"fakecloud_bucket": {Description: "an object storage bucket with a globally unique name", Attributes: []provider.Attribute{
			{Name: "name", Type: tString, Required: true, ForceNew: true},
			{Name: "versioning", Type: tBool, Optional: true, Computed: true},
			{Name: "id", Type: tString, Computed: true},
			{Name: "arn", Type: tString, Computed: true},
		}},
	},
}

// mutable lists the attributes the API can change in place, per type.
var mutable = map[string][]string{
	"fakecloud_network":  {"name"},
	"fakecloud_instance": {"name", "size", "tags"},
	"fakecloud_bucket":   {"versioning"},
}

// Provider implements provider.Provider for the fake cloud.
type Provider struct {
	endpoint   string
	token      string
	maxRetries int
	retryBase  time.Duration
	client     *http.Client
	log        *slog.Logger
}

// New returns an unconfigured provider.
func New() provider.Provider {
	return &Provider{client: &http.Client{Timeout: 30 * time.Second}, log: slog.Default()}
}

// Schema implements provider.Provider.
func (p *Provider) Schema() provider.Schema { return schema }

// Configure implements provider.Provider.
func (p *Provider) Configure(_ context.Context, cfg value.Value) error {
	ep := strings.TrimRight(provider.StringAttr(cfg, "endpoint"), "/")
	u, err := url.Parse(ep)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("endpoint %q must be an http(s) URL", ep)
	}
	p.endpoint = ep
	p.token = provider.StringAttr(cfg, "token")
	p.maxRetries = 4
	if v, ok := cfg.Get("max_retries"); ok && v.Kind() == value.KindNumber {
		if v.AsNumber() < 0 {
			return errors.New("max_retries must not be negative")
		}
		p.maxRetries = int(v.AsNumber())
	}
	p.retryBase = 50 * time.Millisecond
	if v, ok := cfg.Get("retry_base_ms"); ok && v.Kind() == value.KindNumber {
		p.retryBase = time.Duration(v.AsNumber()) * time.Millisecond
	}
	return nil
}

// PlanResource implements provider.Provider.
func (p *Provider) PlanResource(_ context.Context, req provider.PlanRequest) (provider.PlanResponse, error) {
	rs, ok := schema.Resources[req.Type]
	if !ok {
		return provider.PlanResponse{}, fmt.Errorf("unsupported resource type %q", req.Type)
	}
	if req.Type == "fakecloud_instance" {
		if sz, _ := req.Config.Get("size"); sz.Kind() == value.KindString {
			switch sz.AsString() {
			case "small", "medium", "large":
			default:
				return provider.PlanResponse{}, fmt.Errorf("size %q is not one of small, medium, large", sz.AsString())
			}
		}
	}
	resp := provider.PlanFromSchema(rs, req.Prior, req.Config)
	// The API applies documented defaults, so they are known at plan time.
	creating := req.Prior.IsNull() || len(resp.RequiresReplace) > 0
	defaults := map[string]value.Value{}
	switch req.Type {
	case "fakecloud_instance":
		defaults["size"] = value.String("small")
		defaults["tags"] = value.Map(nil)
		defaults["status"] = value.String("running")
	case "fakecloud_bucket":
		defaults["versioning"] = value.False
	}
	for k, d := range defaults {
		cv, _ := req.Config.Get(k)
		if pv, _ := resp.Planned.Get(k); creating && !pv.IsKnown() && cv.IsNull() {
			resp.Planned = resp.Planned.With(k, d)
		}
	}
	return resp, nil
}

// ApplyResource implements provider.Provider.
func (p *Provider) ApplyResource(ctx context.Context, req provider.ApplyRequest) (provider.ApplyResponse, error) {
	kind, ok := kinds[req.Type]
	if !ok {
		return provider.ApplyResponse{}, fmt.Errorf("unsupported resource type %q", req.Type)
	}
	switch {
	case req.Planned.IsNull():
		id := provider.StringAttr(req.Prior, "id")
		if id == "" {
			return provider.ApplyResponse{New: value.Null}, nil
		}
		status, _, err := p.do(ctx, http.MethodDelete, "/v1/"+kind+"/"+url.PathEscape(id), nil, "")
		if err != nil && status != http.StatusNotFound {
			return provider.ApplyResponse{New: req.Prior}, err
		}
		return provider.ApplyResponse{New: value.Null}, nil
	case req.Prior.IsNull():
		body, err := createBody(req.Type, req.Planned)
		if err != nil {
			return provider.ApplyResponse{}, err
		}
		_, obj, err := p.do(ctx, http.MethodPost, "/v1/"+kind, body, req.CreateToken)
		if err != nil {
			return provider.ApplyResponse{}, err
		}
		return provider.ApplyResponse{New: toState(req.Type, obj)}, nil
	default:
		id := provider.StringAttr(req.Prior, "id")
		patch := map[string]any{}
		for _, k := range mutable[req.Type] {
			pv, _ := req.Prior.Get(k)
			nv, _ := req.Planned.Get(k)
			if nv.IsNull() || nv.Equal(pv) {
				continue
			}
			g, err := nv.ToGo()
			if err != nil {
				return provider.ApplyResponse{New: req.Prior}, fmt.Errorf("attribute %s: %w", k, err)
			}
			patch[k] = g
		}
		if len(patch) == 0 {
			return provider.ApplyResponse{New: req.Prior}, nil
		}
		_, obj, err := p.do(ctx, http.MethodPatch, "/v1/"+kind+"/"+url.PathEscape(id), patch, "")
		if err != nil {
			return provider.ApplyResponse{New: req.Prior}, err
		}
		return provider.ApplyResponse{New: toState(req.Type, obj)}, nil
	}
}

// ReadResource implements provider.Provider.
func (p *Provider) ReadResource(ctx context.Context, req provider.ReadRequest) (provider.ReadResponse, error) {
	kind, ok := kinds[req.Type]
	if !ok {
		return provider.ReadResponse{}, fmt.Errorf("unsupported resource type %q", req.Type)
	}
	if req.Pending {
		if req.CreateToken == "" {
			return provider.ReadResponse{Current: value.Null}, nil
		}
		_, raw, err := p.doRaw(ctx, http.MethodGet, "/v1/"+kind+"?create_token="+url.QueryEscape(req.CreateToken), nil, "")
		if err != nil {
			return provider.ReadResponse{}, err
		}
		var list []map[string]any
		if err := json.Unmarshal(raw, &list); err != nil {
			return provider.ReadResponse{}, fmt.Errorf("decoding lookup response: %w", err)
		}
		switch len(list) {
		case 0:
			return provider.ReadResponse{Current: value.Null}, nil
		case 1:
			return provider.ReadResponse{Current: toState(req.Type, list[0])}, nil
		}
		return provider.ReadResponse{}, fmt.Errorf("create token %s matches %d objects", req.CreateToken, len(list))
	}
	id := provider.StringAttr(req.Prior, "id")
	if id == "" {
		return provider.ReadResponse{Current: value.Null}, nil
	}
	status, obj, err := p.do(ctx, http.MethodGet, "/v1/"+kind+"/"+url.PathEscape(id), nil, "")
	if status == http.StatusNotFound {
		return provider.ReadResponse{Current: value.Null}, nil
	}
	if err != nil {
		return provider.ReadResponse{}, err
	}
	return provider.ReadResponse{Current: toState(req.Type, obj)}, nil
}

// createBody builds the POST body from the planned object's known,
// non-computed attributes.
func createBody(typ string, planned value.Value) (map[string]any, error) {
	body := map[string]any{}
	for _, a := range schema.Resources[typ].Attributes {
		if a.Computed && !a.Optional {
			continue
		}
		v, _ := planned.Get(a.Name)
		if v.IsNull() || !v.IsKnown() {
			continue
		}
		g, err := v.ToGo()
		if err != nil {
			return nil, fmt.Errorf("attribute %s: %w", a.Name, err)
		}
		body[a.Name] = g
	}
	return body, nil
}

// toState projects an API object onto the resource schema.
func toState(typ string, obj map[string]any) value.Value {
	out := map[string]value.Value{}
	for _, a := range schema.Resources[typ].Attributes {
		v, err := value.FromGo(obj[a.Name])
		if err != nil {
			v = value.Null
		}
		if conv, err := value.Convert(v, a.Type); err == nil {
			v = conv
		}
		out[a.Name] = v
	}
	return value.Map(out)
}

// APIError is a non-success response from the fake cloud.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("fakecloud API: %d %s", e.Status, e.Msg) }

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// do performs a request and decodes a JSON object response.
func (p *Provider) do(ctx context.Context, method, path string, body any, idemKey string) (int, map[string]any, error) {
	status, raw, err := p.doRaw(ctx, method, path, body, idemKey)
	if err != nil || len(raw) == 0 {
		return status, nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return status, nil, fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return status, obj, nil
}

// doRaw performs a request with retries. Retrying a create is safe because
// it carries the same idempotency key: if the first attempt committed, the
// server returns the original object instead of creating a duplicate.
func (p *Provider) doRaw(ctx context.Context, method, path string, body any, idemKey string) (int, []byte, error) {
	if p.endpoint == "" {
		return 0, nil, errors.New("provider fakecloud is not configured")
	}
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = b
	}
	var lastErr error
	lastStatus := 0
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		if attempt > 0 {
			delay := p.retryBase << (attempt - 1)
			p.log.Debug("fakecloud retry", "method", method, "path", path, "attempt", attempt, "delay", delay, "err", lastErr)
			select {
			case <-ctx.Done():
				return lastStatus, nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, p.endpoint+path, bytes.NewReader(payload))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if p.token != "" {
			req.Header.Set("Authorization", "Bearer "+p.token)
		}
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		resp, err := p.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			lastErr, lastStatus = err, 0
			continue
		}
		raw, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			lastErr, lastStatus = rerr, resp.StatusCode
			continue
		}
		if resp.StatusCode < 300 {
			return resp.StatusCode, raw, nil
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		apiErr := &APIError{Status: resp.StatusCode, Msg: e.Error}
		// A non-idempotent mutation (PATCH/DELETE) that failed after commit
		// is safe to retry too: PATCH sets absolute values and DELETE of a
		// missing object is treated as success by the caller.
		if !retryable(resp.StatusCode) {
			return resp.StatusCode, nil, apiErr
		}
		lastErr, lastStatus = apiErr, resp.StatusCode
	}
	return lastStatus, nil, fmt.Errorf("%s %s: giving up after %d attempts: %w", method, path, p.maxRetries+1, lastErr)
}
