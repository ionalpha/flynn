// Package controlplane is the authenticated API over the resource store, through
// which the CLI, a dashboard and remote peers read and act. Every kind reads through
// one generic list, describe, diff and watch path; a kind becomes listable by
// supplying a [Descriptor], so adding a kind never edits this package.
//
// Every request resolves to a [Principal] with a [Scope], and a handler requires a
// minimum scope. Local use authenticates with a bearer token; delegated, attenuable
// capability tokens plug in behind the same [Authenticator].
package controlplane

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// Column is one display column: a header and a projection from a resource to cell
// text. The projection must be deterministic (no clock, no randomness) so a List
// renders the same on every machine and under replay.
type Column struct {
	Header  string
	Project func(resource.Resource) string
}

// Descriptor declares how a kind is presented: its kind name and list columns. Every
// reader projects through it.
type Descriptor struct {
	Kind    string
	Columns []Column
}

// Table is a kind-agnostic result: headers and one row per resource, in stable order.
// Renderers format it; the read model never does.
type Table struct {
	Columns []string
	Rows    []Row
}

// Row is one resource projected to its cells, carrying the resource id and name
// so a renderer can address or link it independently of the displayed columns.
type Row struct {
	ID    string
	Name  string
	Cells []string
}

// List returns the live resources of d's kind whose labels match sel (nil matches
// all), projected through d's columns, in the store's scope-then-name order.
func List(ctx context.Context, store resource.Store, d Descriptor, sel resource.Selector) (Table, error) {
	rs, err := store.ListAll(ctx, d.Kind, sel)
	if err != nil {
		return Table{}, err
	}
	return project(d, rs), nil
}

func project(d Descriptor, rs []resource.Resource) Table {
	t := Table{Columns: make([]string, len(d.Columns))}
	for i, c := range d.Columns {
		t.Columns[i] = c.Header
	}
	t.Rows = make([]Row, 0, len(rs))
	for _, r := range rs {
		cells := make([]string, len(d.Columns))
		for i, c := range d.Columns {
			cells[i] = c.Project(r)
		}
		t.Rows = append(t.Rows, Row{ID: r.ID, Name: r.Name, Cells: cells})
	}
	return t
}

// Detail is one resource, its row projection with headers, and its recent change
// history.
type Detail struct {
	Resource resource.Resource
	Columns  []string
	Row      Row
	Events   []spine.Event
}

// Describe returns the resource with id and its own mutations from the store's
// stream, in order, capped to the last tail (tail <= 0 returns all). It reads the
// same store and log the system writes, so a describe and a replay agree. A nil log
// yields no history.
func Describe(ctx context.Context, store resource.Store, log spine.Log, d Descriptor, id string, tail int) (Detail, error) {
	r, err := store.GetByID(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	events, err := history(ctx, log, id, tail)
	if err != nil {
		return Detail{}, err
	}
	t := project(d, []resource.Resource{r})
	return Detail{Resource: r, Columns: t.Columns, Row: t.Rows[0], Events: events}, nil
}

// history returns the stream events whose post-image is resource id, capped to the
// last tail.
func history(ctx context.Context, log spine.Log, id string, tail int) ([]spine.Event, error) {
	if log == nil {
		return nil, nil
	}
	all, err := log.Read(ctx, spine.Query{Stream: resource.ResourceStream})
	if err != nil {
		return nil, err
	}
	out := make([]spine.Event, 0, len(all))
	for _, ev := range all {
		r, err := resource.DecodeResource(ev.Payload)
		if err != nil {
			continue
		}
		if r.ID == id {
			out = append(out, ev)
		}
	}
	if tail > 0 && len(out) > tail {
		out = out[len(out)-tail:]
	}
	return out, nil
}

// FieldDelta is one field whose value differs between two resources, addressed by its
// dotted path under spec or status (for example "spec.model", "status.phase").
type FieldDelta struct {
	Field string
	A     string
	B     string
}

// Diff compares the Spec and Status of two resources and returns the differing
// fields sorted by path. A field on one side only compares against the empty string.
// It walks the stored JSON, so it works for every kind. Envelope metadata (ids,
// versions, timestamps) is excluded.
func Diff(a, b resource.Resource) []FieldDelta {
	av, bv := flattenResource(a), flattenResource(b)
	// Size the hint from one side, not the sum, so the capacity cannot overflow.
	keys := make(map[string]struct{}, len(av))
	for k := range av {
		keys[k] = struct{}{}
	}
	for k := range bv {
		keys[k] = struct{}{}
	}
	out := make([]FieldDelta, 0, len(keys))
	for k := range keys {
		if av[k] != bv[k] {
			out = append(out, FieldDelta{Field: k, A: av[k], B: bv[k]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// flattenResource maps Spec and Status to dotted paths and cell strings, under
// separate spec and status prefixes so same-named fields never collide.
func flattenResource(r resource.Resource) map[string]string {
	out := map[string]string{}
	flattenInto(out, "spec", r.Spec)
	flattenInto(out, "status", r.Status)
	return out
}

func flattenInto(out map[string]string, prefix string, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}
	flattenValue(out, prefix, v)
}

// flattenValue records each object leaf under its dotted path. An array renders as
// one compact-JSON cell, so a nested spec diffs per field without one entry per
// element.
func flattenValue(out map[string]string, path string, v any) {
	if m, ok := v.(map[string]any); ok {
		for k, val := range m {
			flattenValue(out, path+"."+k, val)
		}
		return
	}
	out[path] = scalar(v)
}

// Name projects the resource's logical name.
func Name() func(resource.Resource) string {
	return func(r resource.Resource) string { return r.Name }
}

// SpecField projects the Spec JSON value at path as a string, or "" when absent, so a
// Descriptor can show a typed field without this package importing its domain.
func SpecField(path ...string) func(resource.Resource) string {
	return func(r resource.Resource) string { return jsonField(r.Spec, path) }
}

// StatusField is [SpecField] for the Status JSON.
func StatusField(path ...string) func(resource.Resource) string {
	return func(r resource.Resource) string { return jsonField(r.Status, path) }
}

func jsonField(raw json.RawMessage, path []string) string {
	if len(raw) == 0 || len(path) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v, ok = m[key]
		if !ok {
			return ""
		}
	}
	return scalar(v)
}

// scalar renders a decoded JSON value as a cell string. Integral numbers print
// without a trailing fraction; composite values fall back to compact JSON.
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
