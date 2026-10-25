// Package localfs is a provider that manages files and directories on the
// local filesystem, confined to a configured root directory.
//
// Resources:
//
//	local_directory: path (force new), permission; computed id
//	local_file:      path (force new), content, file_permission;
//	                 computed id, content_sha256, size
package localfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SlateGitOrg/plait/internal/provider"
	"github.com/SlateGitOrg/plait/internal/value"
)

// Name is the provider name (resource type prefix).
const Name = "local"

var (
	tString = value.Type{Kind: value.TypeString}
	tNumber = value.Type{Kind: value.TypeNumber}
)

var schema = provider.Schema{
	Provider: provider.ResourceSchema{Attributes: []provider.Attribute{
		{Name: "root", Type: tString, Optional: true, Description: "directory all managed paths must stay inside (default: current directory)"},
	}},
	Resources: map[string]provider.ResourceSchema{
		"local_directory": {Description: "a directory", Attributes: []provider.Attribute{
			{Name: "path", Type: tString, Required: true, ForceNew: true},
			{Name: "permission", Type: tString, Optional: true, Computed: true},
			{Name: "id", Type: tString, Computed: true},
		}},
		"local_file": {Description: "a regular file with literal content", Attributes: []provider.Attribute{
			{Name: "path", Type: tString, Required: true, ForceNew: true},
			{Name: "content", Type: tString, Required: true},
			{Name: "file_permission", Type: tString, Optional: true, Computed: true},
			{Name: "id", Type: tString, Computed: true},
			{Name: "content_sha256", Type: tString, Computed: true},
			{Name: "size", Type: tNumber, Computed: true},
		}},
	},
}

// Provider implements provider.Provider for the local filesystem.
type Provider struct {
	base string // directory a relative root resolves against
	root string
}

// New returns an unconfigured provider whose relative root resolves
// against the process working directory.
func New() provider.Provider { return &Provider{} }

// NewAt returns an unconfigured provider whose relative root (and default
// root) resolves against base, normally the configuration directory.
func NewAt(base string) provider.Provider { return &Provider{base: base} }

// Schema implements provider.Provider.
func (p *Provider) Schema() provider.Schema { return schema }

// Configure implements provider.Provider.
func (p *Provider) Configure(_ context.Context, cfg value.Value) error {
	root := provider.StringAttr(cfg, "root")
	if root == "" {
		root = "."
	}
	if !filepath.IsAbs(root) && p.base != "" {
		root = filepath.Join(p.base, root)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	p.root = abs
	return nil
}

// resolve maps a configured path to an absolute path inside the root.
func (p *Provider) resolve(path string) (string, error) {
	if p.root == "" {
		return "", errors.New("provider local is not configured")
	}
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(p.root, path)
	}
	full = filepath.Clean(full)
	rel, err := filepath.Rel(p.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the provider root %s", path, p.root)
	}
	return full, nil
}

func parsePerm(s, def string) (fs.FileMode, error) {
	if s == "" {
		s = def
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n > 0o777 {
		return 0, fmt.Errorf("invalid permission %q: want an octal mode such as 0644", s)
	}
	return fs.FileMode(n), nil
}

func digest(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// PlanResource implements provider.Provider. Files know their digest and
// size at plan time whenever the content is known.
func (p *Provider) PlanResource(_ context.Context, req provider.PlanRequest) (provider.PlanResponse, error) {
	rs, ok := schema.Resources[req.Type]
	if !ok {
		return provider.PlanResponse{}, fmt.Errorf("unsupported resource type %q", req.Type)
	}
	if path, _ := req.Config.Get("path"); path.Kind() == value.KindString {
		if _, err := p.resolve(path.AsString()); err != nil {
			return provider.PlanResponse{}, err
		}
	}
	permAttr, def := "permission", "0755"
	if req.Type == "local_file" {
		permAttr, def = "file_permission", "0644"
	}
	if pv, _ := req.Config.Get(permAttr); pv.Kind() == value.KindString {
		if _, err := parsePerm(pv.AsString(), def); err != nil {
			return provider.PlanResponse{}, err
		}
	}
	resp := provider.PlanFromSchema(rs, req.Prior, req.Config)
	planned := resp.Planned
	if pv, _ := planned.Get(permAttr); !pv.IsKnown() {
		cv, _ := req.Config.Get(permAttr)
		if cv.IsNull() && (req.Prior.IsNull() || len(resp.RequiresReplace) > 0) {
			planned = planned.With(permAttr, value.String(def))
		}
	}
	if path, _ := planned.Get("path"); path.Kind() == value.KindString {
		planned = planned.With("id", value.String(filepath.Clean(path.AsString())))
	}
	if req.Type == "local_file" {
		if c, _ := planned.Get("content"); c.Kind() == value.KindString {
			planned = planned.With("content_sha256", value.String(digest(c.AsString())))
			planned = planned.With("size", value.Int(len(c.AsString())))
		} else {
			planned = planned.With("content_sha256", value.Unknown).With("size", value.Unknown)
		}
	}
	resp.Planned = planned
	return resp, nil
}

// ApplyResource implements provider.Provider.
func (p *Provider) ApplyResource(_ context.Context, req provider.ApplyRequest) (provider.ApplyResponse, error) {
	if req.Planned.IsNull() {
		full, err := p.resolve(provider.StringAttr(req.Prior, "path"))
		if err != nil {
			return provider.ApplyResponse{}, err
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return provider.ApplyResponse{New: req.Prior}, err
		}
		return provider.ApplyResponse{New: value.Null}, nil
	}
	path := provider.StringAttr(req.Planned, "path")
	full, err := p.resolve(path)
	if err != nil {
		return provider.ApplyResponse{}, err
	}
	switch req.Type {
	case "local_directory":
		mode, err := parsePerm(provider.StringAttr(req.Planned, "permission"), "0755")
		if err != nil {
			return provider.ApplyResponse{}, err
		}
		if err := os.MkdirAll(full, mode); err != nil {
			return provider.ApplyResponse{}, err
		}
		if err := os.Chmod(full, mode); err != nil {
			return provider.ApplyResponse{}, err
		}
	case "local_file":
		mode, err := parsePerm(provider.StringAttr(req.Planned, "file_permission"), "0644")
		if err != nil {
			return provider.ApplyResponse{}, err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return provider.ApplyResponse{}, err
		}
		if err := writeFile(full, provider.StringAttr(req.Planned, "content"), mode); err != nil {
			return provider.ApplyResponse{}, err
		}
	default:
		return provider.ApplyResponse{}, fmt.Errorf("unsupported resource type %q", req.Type)
	}
	cur, err := p.read(req.Type, path)
	if err != nil {
		return provider.ApplyResponse{}, err
	}
	if cur.IsNull() {
		return provider.ApplyResponse{}, fmt.Errorf("%s vanished right after being written", path)
	}
	return provider.ApplyResponse{New: cur}, nil
}

func writeFile(full, content string, mode fs.FileMode) error {
	tmp := full + ".plait-tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, full)
}

// ReadResource implements provider.Provider. Paths are the identity of
// local objects, so a pending create is recovered simply by reading the
// planned path.
func (p *Provider) ReadResource(_ context.Context, req provider.ReadRequest) (provider.ReadResponse, error) {
	cur, err := p.read(req.Type, provider.StringAttr(req.Prior, "path"))
	return provider.ReadResponse{Current: cur}, err
}

func (p *Provider) read(typ, path string) (value.Value, error) {
	full, err := p.resolve(path)
	if err != nil {
		return value.Null, err
	}
	info, err := os.Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return value.Null, nil
	}
	if err != nil {
		return value.Null, err
	}
	perm := fmt.Sprintf("%04o", info.Mode().Perm())
	attrs := map[string]value.Value{"path": value.String(path), "id": value.String(filepath.Clean(path))}
	switch typ {
	case "local_directory":
		if !info.IsDir() {
			return value.Null, fmt.Errorf("%s exists but is not a directory", path)
		}
		attrs["permission"] = value.String(perm)
	case "local_file":
		if !info.Mode().IsRegular() {
			return value.Null, fmt.Errorf("%s exists but is not a regular file", path)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return value.Null, err
		}
		attrs["content"] = value.String(string(data))
		attrs["file_permission"] = value.String(perm)
		attrs["content_sha256"] = value.String(digest(string(data)))
		attrs["size"] = value.Int(len(data))
	default:
		return value.Null, fmt.Errorf("unsupported resource type %q", typ)
	}
	return value.Map(attrs), nil
}
