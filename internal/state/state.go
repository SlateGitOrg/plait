// Package state models plait's persisted view of managed infrastructure and
// stores it as JSON with atomic writes and an advisory lock file.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/SlateGitOrg/plait/internal/addr"
	"github.com/SlateGitOrg/plait/internal/value"
)

// FormatVersion is the state file format version.
const FormatVersion = 1

// Status is the lifecycle status of a stored object.
type Status string

// Object statuses.
const (
	// StatusReady objects were created successfully.
	StatusReady Status = "ready"
	// StatusPending objects were about to be created when the state was last
	// written. The remote object may or may not exist; refresh resolves it
	// through the provider using the object's create token.
	StatusPending Status = "pending"
	// StatusTainted objects exist but failed part-way through creation and
	// will be replaced.
	StatusTainted Status = "tainted"
)

// Object is one remote object as last recorded.
type Object struct {
	Status      Status      `json:"status"`
	Attrs       value.Value `json:"attributes"`
	CreateToken string      `json:"create_token,omitempty"`
}

// Instance is the stored state of one resource instance.
type Instance struct {
	Addr     addr.Instance
	Provider string
	Current  *Object
	// Deposed is a previous object awaiting deletion after a
	// create_before_destroy replacement.
	Deposed      *Object
	Dependencies []string
}

// State is the full state document.
type State struct {
	Lineage   string
	Serial    uint64
	Instances map[string]*Instance
	Outputs   map[string]value.Value
}

// New returns an empty state with a fresh lineage.
func New() *State {
	return &State{Lineage: newLineage(), Instances: map[string]*Instance{}, Outputs: map[string]value.Value{}}
}

func newLineage() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("state: random lineage: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// NewToken returns a random create token used for idempotent creates.
func NewToken() string { return newLineage() }

// Get returns the instance at a, or nil.
func (s *State) Get(a addr.Instance) *Instance { return s.Instances[a.String()] }

// Set stores inst under its address.
func (s *State) Set(inst *Instance) { s.Instances[inst.Addr.String()] = inst }

// Remove deletes the instance at a.
func (s *State) Remove(a addr.Instance) { delete(s.Instances, a.String()) }

// Sorted returns instances in address order.
func (s *State) Sorted() []*Instance {
	out := make([]*Instance, 0, len(s.Instances))
	for _, i := range s.Instances {
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool { return addr.Less(out[i].Addr, out[j].Addr) })
	return out
}

// ResourceInstances returns the instances of resource r in address order.
func (s *State) ResourceInstances(r addr.Resource) []*Instance {
	var out []*Instance
	for _, i := range s.Sorted() {
		if i.Addr.Resource == r {
			out = append(out, i)
		}
	}
	return out
}

func (o *Object) clone() *Object {
	if o == nil {
		return nil
	}
	cp := *o
	return &cp
}

// Clone returns a deep-enough copy: values are immutable, so only the maps
// and instance structs are copied.
func (s *State) Clone() *State {
	cp := &State{Lineage: s.Lineage, Serial: s.Serial,
		Instances: make(map[string]*Instance, len(s.Instances)),
		Outputs:   make(map[string]value.Value, len(s.Outputs))}
	for k, i := range s.Instances {
		ic := *i
		ic.Current = i.Current.clone()
		ic.Deposed = i.Deposed.clone()
		ic.Dependencies = append([]string(nil), i.Dependencies...)
		cp.Instances[k] = &ic
	}
	for k, v := range s.Outputs {
		cp.Outputs[k] = v
	}
	return cp
}

// Empty reports whether no instances are tracked.
func (s *State) Empty() bool { return len(s.Instances) == 0 }

type fileInstance struct {
	Address      string   `json:"address"`
	Type         string   `json:"type"`
	Name         string   `json:"name"`
	IndexKey     any      `json:"index_key,omitempty"`
	Provider     string   `json:"provider"`
	Current      *Object  `json:"current,omitempty"`
	Deposed      *Object  `json:"deposed,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type fileState struct {
	Version   int                    `json:"version"`
	Lineage   string                 `json:"lineage"`
	Serial    uint64                 `json:"serial"`
	Outputs   map[string]value.Value `json:"outputs"`
	Resources []fileInstance         `json:"resources"`
}

// MarshalJSON encodes the state with instances sorted by address.
func (s *State) MarshalJSON() ([]byte, error) {
	fs := fileState{Version: FormatVersion, Lineage: s.Lineage, Serial: s.Serial, Outputs: s.Outputs, Resources: []fileInstance{}}
	if fs.Outputs == nil {
		fs.Outputs = map[string]value.Value{}
	}
	for _, i := range s.Sorted() {
		for _, o := range []*Object{i.Current, i.Deposed} {
			if o != nil && !o.Attrs.IsWhollyKnown() {
				return nil, fmt.Errorf("state: %s contains unknown values", i.Addr)
			}
		}
		fi := fileInstance{Address: i.Addr.String(), Type: i.Addr.Type, Name: i.Addr.Name,
			Provider: i.Provider, Current: i.Current, Deposed: i.Deposed, Dependencies: i.Dependencies}
		switch i.Addr.Key.Kind {
		case addr.IntKey:
			fi.IndexKey = i.Addr.Key.Int
		case addr.StringKey:
			fi.IndexKey = i.Addr.Key.Str
		}
		fs.Resources = append(fs.Resources, fi)
	}
	return json.MarshalIndent(fs, "", "  ")
}

// UnmarshalJSON decodes a state document.
func (s *State) UnmarshalJSON(data []byte) error {
	var fs fileState
	if err := json.Unmarshal(data, &fs); err != nil {
		return err
	}
	if fs.Version != FormatVersion {
		return fmt.Errorf("unsupported state format version %d (want %d)", fs.Version, FormatVersion)
	}
	if fs.Lineage == "" {
		return fmt.Errorf("state has no lineage")
	}
	*s = State{Lineage: fs.Lineage, Serial: fs.Serial, Instances: map[string]*Instance{}, Outputs: fs.Outputs}
	if s.Outputs == nil {
		s.Outputs = map[string]value.Value{}
	}
	for _, fi := range fs.Resources {
		a, err := addr.ParseInstance(fi.Address)
		if err != nil {
			return err
		}
		if fi.Current == nil && fi.Deposed == nil {
			return fmt.Errorf("state: %s has no objects", fi.Address)
		}
		for _, o := range []*Object{fi.Current, fi.Deposed} {
			if o == nil {
				continue
			}
			switch o.Status {
			case StatusReady, StatusPending, StatusTainted:
			default:
				return fmt.Errorf("state: %s has invalid status %q", fi.Address, o.Status)
			}
		}
		if _, dup := s.Instances[a.String()]; dup {
			return fmt.Errorf("state: duplicate instance %s", a)
		}
		s.Instances[a.String()] = &Instance{Addr: a, Provider: fi.Provider, Current: fi.Current, Deposed: fi.Deposed, Dependencies: fi.Dependencies}
	}
	return nil
}
