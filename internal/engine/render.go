package engine

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/archit2501/plait/internal/value"
)

// RenderOptions controls plan rendering.
type RenderOptions struct {
	// ShowUnchanged lists unchanged attributes of updated objects instead
	// of summarizing them.
	ShowUnchanged bool
}

// Render writes a human-readable plan in the style of `terraform plan`.
// The output is deterministic, which is what the golden tests rely on.
func Render(w io.Writer, p *Plan, opts RenderOptions) error {
	r := &renderer{w: w, opts: opts}
	r.drift(p.Drift)
	if !p.HasChanges() {
		r.line("No changes. Your infrastructure matches the configuration.")
		return r.err
	}
	var changes []*ResourceChange
	for _, c := range p.Changes {
		if c.Action != NoOp {
			changes = append(changes, c)
		}
	}
	if len(changes) > 0 {
		r.line("plait will perform the following actions:")
		r.line("")
		for _, c := range changes {
			r.change(c)
			r.line("")
		}
	}
	add, change, destroy := p.Counts()
	r.line(fmt.Sprintf("Plan: %d to add, %d to change, %d to destroy.", add, change, destroy))
	var outs []*OutputChange
	for _, o := range p.Outputs {
		if o.Action != NoOp {
			outs = append(outs, o)
		}
	}
	if len(outs) > 0 {
		r.line("")
		r.line("Changes to Outputs:")
		width := 0
		for _, o := range outs {
			width = max(width, len(o.Name))
		}
		for _, o := range outs {
			pad := strings.Repeat(" ", width-len(o.Name))
			switch o.Action {
			case Create:
				r.line(fmt.Sprintf("  + %s%s = %s", o.Name, pad, o.After.GoString()))
			case Update:
				r.line(fmt.Sprintf("  ~ %s%s = %s -> %s", o.Name, pad, o.Before.GoString(), o.After.GoString()))
			case Delete:
				r.line(fmt.Sprintf("  - %s%s = %s -> null", o.Name, pad, o.Before.GoString()))
			}
		}
	}
	return r.err
}

type renderer struct {
	w    io.Writer
	opts RenderOptions
	err  error
}

func (r *renderer) line(s string) {
	if r.err != nil {
		return
	}
	_, r.err = io.WriteString(r.w, s+"\n")
}

func (r *renderer) drift(ds []Drift) {
	if len(ds) == 0 {
		return
	}
	r.line("Note: Objects have changed outside of plait")
	r.line("")
	for _, d := range ds {
		name := d.Addr.String()
		if d.Deposed {
			name += " (deposed object)"
		}
		switch d.Kind {
		case "deleted":
			r.line("  # " + name + " has been deleted")
		case "recovered":
			r.line("  # " + name + " was being created when plait was interrupted; the object exists and is now tracked")
		case "abandoned":
			r.line("  # " + name + " was being created when plait was interrupted; no object was found, so it will be created again")
			r.line("  # (reusing the interrupted request's idempotency key, so a late-arriving original cannot duplicate it)")
		case "changed":
			r.line("  # " + name + " has changed")
			for _, k := range unionKeys(d.Before, d.After) {
				b, _ := d.Before.Get(k)
				a, _ := d.After.Get(k)
				if !b.Equal(a) {
					r.line(fmt.Sprintf("      ~ %s = %s -> %s", k, b.GoString(), a.GoString()))
				}
			}
		}
	}
	r.line("")
}

func header(c *ResourceChange) (symbol, text string) {
	a := c.Addr.String()
	if c.Deposed {
		return "-", "# " + a + " (deposed object) will be destroyed"
	}
	switch c.Action {
	case Create:
		return "+", "# " + a + " will be created"
	case Update:
		return "~", "# " + a + " will be updated in-place"
	case DeleteThenCreate, CreateThenDelete:
		sym := "-/+"
		if c.Action == CreateThenDelete {
			sym = "+/-"
		}
		if c.Reason != "" {
			return sym, "# " + a + " must be replaced because " + c.Reason
		}
		return sym, "# " + a + " must be replaced"
	}
	return "-", "# " + a + " will be destroyed"
}

func (r *renderer) change(c *ResourceChange) {
	sym, text := header(c)
	r.line("  " + text)
	if c.Action == Delete && c.Reason != "" && !c.Deposed {
		r.line("  # (because " + c.Reason + ")")
	}
	lead := fmt.Sprintf("%3s", sym)
	r.line(fmt.Sprintf("%s resource %q %q {", lead, c.Addr.Type, c.Addr.Name))
	keys := unionKeys(c.Before, c.After)
	type row struct{ sym, key, text, note string }
	var rows []row
	hidden := 0
	for _, k := range keys {
		b, _ := c.Before.Get(k)
		a, _ := c.After.Get(k)
		var rw row
		switch {
		case c.Action == Create:
			if a.IsNull() {
				continue
			}
			rw = row{"+", k, a.GoString(), ""}
		case c.Action == Delete:
			if b.IsNull() {
				continue
			}
			rw = row{"-", k, b.GoString() + " -> null", ""}
		case b.Equal(a):
			if !r.opts.ShowUnchanged {
				if !b.IsNull() {
					hidden++
				}
				continue
			}
			if b.IsNull() {
				continue
			}
			rw = row{" ", k, b.GoString(), ""}
		case b.IsNull():
			rw = row{"+", k, a.GoString(), ""}
		case a.IsNull():
			rw = row{"-", k, b.GoString() + " -> null", ""}
		default:
			rw = row{"~", k, b.GoString() + " -> " + a.GoString(), ""}
		}
		if slices.Contains(c.RequiresReplace, k) {
			rw.note = " # forces replacement"
		}
		rows = append(rows, rw)
	}
	width := 0
	for _, rw := range rows {
		width = max(width, len(rw.key))
	}
	for _, rw := range rows {
		r.line(fmt.Sprintf("      %s %s%s = %s%s", rw.sym, rw.key, strings.Repeat(" ", width-len(rw.key)), rw.text, rw.note))
	}
	if hidden > 0 {
		noun := "attributes"
		if hidden == 1 {
			noun = "attribute"
		}
		r.line(fmt.Sprintf("        # (%d unchanged %s hidden)", hidden, noun))
	}
	r.line("    }")
}

func unionKeys(a, b value.Value) []string {
	set := map[string]bool{}
	for _, k := range a.Keys() {
		set[k] = true
	}
	for _, k := range b.Keys() {
		set[k] = true
	}
	return sortedKeys(set)
}
