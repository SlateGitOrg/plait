package engine

import (
	"encoding/json"
	"fmt"

	"github.com/archit2501/plait/internal/addr"
	"github.com/archit2501/plait/internal/state"
	"github.com/archit2501/plait/internal/value"
)

// PlanFormatVersion is the saved plan format version.
const PlanFormatVersion = 1

type changeJSON struct {
	Address string `json:"address"`
	*ResourceChange
}

type driftJSON struct {
	Address string      `json:"address"`
	Deposed bool        `json:"deposed,omitempty"`
	Kind    string      `json:"kind"`
	Before  value.Value `json:"before"`
	After   value.Value `json:"after"`
}

type planJSON struct {
	FormatVersion int                    `json:"format_version"`
	Destroy       bool                   `json:"destroy"`
	ConfigDigest  string                 `json:"config_digest"`
	Variables     map[string]value.Value `json:"variables"`
	Changes       []changeJSON           `json:"resource_changes"`
	Outputs       []*OutputChange        `json:"output_changes"`
	Drift         []driftJSON            `json:"drift,omitempty"`
	Expanded      map[string][]string    `json:"expanded"`
	PriorState    *state.State           `json:"prior_state"`
}

// MarshalPlan encodes a plan as indented JSON. The encoding is complete:
// a saved plan can be applied later with UnmarshalPlan.
func MarshalPlan(p *Plan) ([]byte, error) {
	pj := planJSON{FormatVersion: PlanFormatVersion, Destroy: p.Destroy, ConfigDigest: p.ConfigDigest,
		Variables: p.Variables, Outputs: p.Outputs, PriorState: p.PriorState,
		Changes: []changeJSON{}, Expanded: map[string][]string{}}
	if pj.Outputs == nil {
		pj.Outputs = []*OutputChange{}
	}
	for _, c := range p.Changes {
		pj.Changes = append(pj.Changes, changeJSON{Address: c.Addr.String(), ResourceChange: c})
	}
	for _, d := range p.Drift {
		pj.Drift = append(pj.Drift, driftJSON{Address: d.Addr.String(), Deposed: d.Deposed, Kind: d.Kind, Before: d.Before, After: d.After})
	}
	for ra, keys := range p.Expanded {
		r, err := parseResource(ra)
		if err != nil {
			return nil, err
		}
		list := make([]string, len(keys))
		for i, k := range keys {
			list[i] = addr.Instance{Resource: r, Key: k}.String()
		}
		pj.Expanded[ra] = list
	}
	return json.MarshalIndent(pj, "", "  ")
}

// UnmarshalPlan decodes a plan written by MarshalPlan.
func UnmarshalPlan(data []byte) (*Plan, error) {
	var pj planJSON
	if err := json.Unmarshal(data, &pj); err != nil {
		return nil, fmt.Errorf("reading plan: %w", err)
	}
	if pj.FormatVersion != PlanFormatVersion {
		return nil, fmt.Errorf("unsupported plan format version %d (want %d)", pj.FormatVersion, PlanFormatVersion)
	}
	if pj.PriorState == nil {
		return nil, fmt.Errorf("plan has no prior state")
	}
	p := &Plan{Destroy: pj.Destroy, ConfigDigest: pj.ConfigDigest, Variables: pj.Variables,
		PriorState: pj.PriorState, Outputs: pj.Outputs, Expanded: map[string][]addr.Key{}}
	for _, cj := range pj.Changes {
		if cj.ResourceChange == nil {
			return nil, fmt.Errorf("plan change %q is empty", cj.Address)
		}
		a, err := addr.ParseInstance(cj.Address)
		if err != nil {
			return nil, err
		}
		cj.Addr = a
		p.Changes = append(p.Changes, cj.ResourceChange)
	}
	for _, dj := range pj.Drift {
		a, err := addr.ParseInstance(dj.Address)
		if err != nil {
			return nil, err
		}
		p.Drift = append(p.Drift, Drift{Addr: a, Deposed: dj.Deposed, Kind: dj.Kind, Before: dj.Before, After: dj.After})
	}
	for ra, list := range pj.Expanded {
		keys := make([]addr.Key, len(list))
		for i, s := range list {
			a, err := addr.ParseInstance(s)
			if err != nil {
				return nil, err
			}
			if a.Resource.String() != ra {
				return nil, fmt.Errorf("plan expansion for %s lists foreign instance %s", ra, s)
			}
			keys[i] = a.Key
		}
		p.Expanded[ra] = keys
	}
	return p, nil
}

func parseResource(s string) (addr.Resource, error) {
	a, err := addr.ParseInstance(s)
	if err != nil {
		return addr.Resource{}, err
	}
	if a.Key.Kind != addr.NoKey {
		return addr.Resource{}, fmt.Errorf("%q is an instance address, not a resource address", s)
	}
	return a.Resource, nil
}
