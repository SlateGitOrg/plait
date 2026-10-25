// Package provider defines the in-process provider plugin interface that
// plait's engine drives, plus schema-driven helpers that most providers use
// to validate configuration and compute planned states.
//
// The engine never talks to infrastructure directly. For each resource
// instance it asks the provider to plan (prior + config -> planned state with
// unknowns and replacement paths), to apply (create, update or delete), and
// to read (refresh the real state of an object, or recover an object whose
// create was interrupted).
package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/SlateGitOrg/plait/internal/value"
)

// Attribute describes one attribute of a resource or provider schema.
type Attribute struct {
	Name     string
	Type     value.Type
	Required bool
	Optional bool
	// Computed attributes are set by the provider. Optional+Computed
	// attributes take the configured value when set, otherwise the
	// provider's choice.
	Computed bool
	// ForceNew means a change to this attribute requires replacing the
	// object.
	ForceNew    bool
	Description string
}

// ResourceSchema describes a resource type.
type ResourceSchema struct {
	Description string
	Attributes  []Attribute
}

// Attr returns the named attribute schema.
func (s ResourceSchema) Attr(name string) (Attribute, bool) {
	for _, a := range s.Attributes {
		if a.Name == name {
			return a, true
		}
	}
	return Attribute{}, false
}

// Schema is a provider's full schema.
type Schema struct {
	Provider  ResourceSchema
	Resources map[string]ResourceSchema
}

// PlanRequest asks a provider to plan one resource instance.
type PlanRequest struct {
	Type   string
	Prior  value.Value // null when the object does not exist yet
	Config value.Value // validated config; may contain unknowns
}

// PlanResponse is the provider's planned new state.
type PlanResponse struct {
	Planned value.Value
	// RequiresReplace lists attributes whose change forces replacement.
	RequiresReplace []string
}

// ApplyRequest asks a provider to converge one object. Prior null means
// create; Planned null means delete; otherwise update.
type ApplyRequest struct {
	Type    string
	Prior   value.Value
	Planned value.Value
	// CreateToken is a unique idempotency key for creates. A provider must
	// make a repeated create with the same token return the original object,
	// and must be able to look an object up by it (see ReadRequest).
	CreateToken string
}

// ApplyResponse carries the new state; null after a delete. A provider may
// return a non-null New together with an error when the object was created
// but could not be fully configured; the engine records it as tainted.
type ApplyResponse struct {
	New value.Value
}

// ReadRequest asks a provider for the current state of an object.
type ReadRequest struct {
	Type  string
	Prior value.Value
	// Pending is set when the object's create was interrupted: Prior holds
	// only the planned attributes and the provider should look the object
	// up by CreateToken.
	Pending     bool
	CreateToken string
}

// ReadResponse holds the current state, or null if the object no longer
// exists (or, for a pending read, was never created).
type ReadResponse struct {
	Current value.Value
}

// Provider is the plugin interface.
type Provider interface {
	Schema() Schema
	Configure(ctx context.Context, config value.Value) error
	PlanResource(ctx context.Context, req PlanRequest) (PlanResponse, error)
	ApplyResource(ctx context.Context, req ApplyRequest) (ApplyResponse, error)
	ReadResource(ctx context.Context, req ReadRequest) (ReadResponse, error)
}

// Factory creates a fresh provider instance.
type Factory func() Provider

// Registry maps provider names (the resource type prefix) to factories.
type Registry map[string]Factory

// Names returns the registered provider names, sorted.
func (r Registry) Names() []string {
	out := make([]string, 0, len(r))
	for n := range r {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ValidateConfig checks a configuration object against a schema and returns
// a normalized object containing every schema attribute (null when unset)
// with values converted to the declared types.
func ValidateConfig(s ResourceSchema, what string, cfg value.Value) (value.Value, error) {
	if cfg.IsNull() {
		cfg = value.Map(nil)
	}
	if cfg.Kind() != value.KindMap {
		return value.Null, fmt.Errorf("%s: configuration must be an object", what)
	}
	var problems []string
	for _, k := range cfg.Keys() {
		a, ok := s.Attr(k)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("unsupported argument %q", k))
		case a.Computed && !a.Optional && !a.Required:
			problems = append(problems, fmt.Sprintf("%q is computed by the provider and cannot be set", k))
		}
	}
	out := map[string]value.Value{}
	for _, a := range s.Attributes {
		v, set := cfg.Get(a.Name)
		if !set {
			v = value.Null
		}
		if a.Required && v.IsNull() {
			problems = append(problems, fmt.Sprintf("missing required argument %q", a.Name))
			continue
		}
		conv, err := value.Convert(v, a.Type)
		if err != nil {
			problems = append(problems, fmt.Sprintf("argument %q: %v", a.Name, err))
			continue
		}
		out[a.Name] = conv
	}
	if len(problems) > 0 {
		return value.Null, fmt.Errorf("%s: %s", what, strings.Join(problems, "; "))
	}
	return value.Map(out), nil
}

// PlanFromSchema implements the standard planning rules:
//
//   - configured attributes take their configured value (possibly unknown);
//   - unset Optional+Computed attributes keep their prior value, or become
//     unknown on create;
//   - pure Computed attributes keep their prior value on update and become
//     unknown on create or replace;
//   - a ForceNew attribute whose planned value differs from the prior value
//     (including becoming unknown) requires replacement.
func PlanFromSchema(s ResourceSchema, prior, config value.Value) PlanResponse {
	creating := prior.IsNull()
	out := map[string]value.Value{}
	var replace []string
	for _, a := range s.Attributes {
		cv, _ := config.Get(a.Name)
		pv := value.Null
		if !creating {
			pv, _ = prior.Get(a.Name)
		}
		var planned value.Value
		switch {
		case a.Computed && !a.Optional && !a.Required:
			planned = pv
			if creating {
				planned = value.Unknown
			}
		case a.Computed && cv.IsNull():
			planned = pv
			if creating || pv.IsNull() {
				planned = value.Unknown
			}
		default:
			planned = cv
		}
		out[a.Name] = planned
		if !creating && a.ForceNew && !planned.Equal(pv) {
			replace = append(replace, a.Name)
		}
	}
	if len(replace) > 0 {
		// The replacement is a new object: every provider-chosen value is
		// unknown again.
		for _, a := range s.Attributes {
			cv, _ := config.Get(a.Name)
			if a.Computed && (cv.IsNull() || (!a.Optional && !a.Required)) {
				out[a.Name] = value.Unknown
			}
		}
	}
	return PlanResponse{Planned: value.Map(out), RequiresReplace: replace}
}

// StringAttr fetches a known string attribute from an object, returning ""
// when it is null.
func StringAttr(obj value.Value, name string) string {
	v, ok := obj.Get(name)
	if !ok || v.Kind() != value.KindString {
		return ""
	}
	return v.AsString()
}
