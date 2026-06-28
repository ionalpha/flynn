// Package controlplane is the read model over the resource substrate: one generic
// way to list, describe, and watch any registered kind. The command-line reader,
// a future remote API, and a dashboard all read through this package instead of
// each re-deriving per-kind queries, so there is a single read path to keep
// correct. A kind becomes listable by supplying a Descriptor (its display
// columns); nothing here is hard-coded to a specific kind, so adding a kind never
// edits this package.
package controlplane

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// Column is one display column of a kind's table: a header and a pure projection
// from a resource to its cell text. A projection must be deterministic (no clock,
// no randomness) so a List renders identically on every machine and under replay.
type Column struct {
	Header  string
	Project func(resource.Resource) string
}

// Descriptor declares how a kind is presented in the read model: its resource
// kind name and the columns a list shows. Supplying a Descriptor is the only step
// needed to make a kind appear in list/describe and, later, the remote API and
// dashboard, because every reader projects through it.
type Descriptor struct {
	Kind    string
	Columns []Column
}

// Table is a rendered, kind-agnostic result: the column headers and one row per
// resource, in a stable order. A renderer (text, JSON, HTML) formats it; the read
// model itself never formats.
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

// List returns every live resource of the descriptor's kind whose labels satisfy
// sel (a nil selector matches all), each projected through the descriptor's
// columns. Results follow the store's order (scope then name), so the table is
// deterministic.
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

// Detail is a single resource fully described: the resource itself, its one-row
// projection through the descriptor (with the column headers), and its recent
// change history from the event log.
type Detail struct {
	Resource resource.Resource
	Columns  []string
	Row      Row
	Events   []spine.Event
}

// Describe returns the resource addressed by id together with its recent change
// history. History is the resource's own mutations on the substrate stream (when
// it was created, updated, or deleted), decoded and filtered to this resource, in
// order and capped to the last tail (tail <= 0 returns all). It reads through the
// same store and event log the rest of the system writes, so a describe and a
// replay of the same resource agree. A nil log yields an empty history.
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

// history reads the resource mutation stream and returns the events whose
// post-image is the resource with this id, in stream order, capped to the last
// tail.
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

// Name projects the resource's logical name. It is the column nearly every kind
// lists first.
func Name() func(resource.Resource) string {
	return func(r resource.Resource) string { return r.Name }
}

// SpecField projects a string value from the resource Spec JSON at path, or the
// empty string when the path is absent. Scalars are stringified, so a Descriptor
// can surface a typed field without this package importing the domain that
// defines it.
func SpecField(path ...string) func(resource.Resource) string {
	return func(r resource.Resource) string { return jsonField(r.Spec, path) }
}

// StatusField projects a string value from the resource Status JSON at path, the
// observed-state counterpart of SpecField.
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
