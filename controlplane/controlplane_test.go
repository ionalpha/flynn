package controlplane_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

const (
	widgetAPI  = "test.ionagent.io/v1"
	widgetKind = "Widget"
)

func newStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	err := reg.Register(resource.Kind{
		APIVersion: widgetAPI,
		Name:       widgetKind,
		Schema:     json.RawMessage(`{"type":"object","properties":{"color":{"type":"string"}}}`),
		Singular:   "widget",
		Plural:     "widgets",
	})
	if err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return resource.NewMemory(reg)
}

func logOf(t *testing.T, store resource.Store) spine.Log {
	t.Helper()
	l, ok := store.(interface{ Log() spine.Log })
	if !ok {
		t.Fatal("store does not expose its event log")
	}
	return l.Log()
}

func putWidget(t *testing.T, store resource.Store, name, color, state string) resource.Resource {
	t.Helper()
	r, err := store.Put(context.Background(), resource.Resource{
		APIVersion: widgetAPI,
		Kind:       widgetKind,
		Name:       name,
		Spec:       json.RawMessage(`{"color":` + quote(color) + `}`),
		Status:     json.RawMessage(`{"state":` + quote(state) + `}`),
	})
	if err != nil {
		t.Fatalf("put widget %s: %v", name, err)
	}
	return r
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func descriptor() controlplane.Descriptor {
	return controlplane.Descriptor{
		Kind: widgetKind,
		Columns: []controlplane.Column{
			{Header: "NAME", Project: controlplane.Name()},
			{Header: "COLOR", Project: controlplane.SpecField("color")},
			{Header: "STATE", Project: controlplane.StatusField("state")},
		},
	}
}

func TestListProjectsColumnsInStableOrder(t *testing.T) {
	store := newStore(t)
	putWidget(t, store, "beta", "blue", "idle")
	putWidget(t, store, "alpha", "red", "working")

	table, err := controlplane.List(context.Background(), store, descriptor(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got, want := table.Columns, []string{"NAME", "COLOR", "STATE"}; !equal(got, want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(table.Rows))
	}
	// Ordered by name, so alpha precedes beta regardless of insertion order.
	if table.Rows[0].Name != "alpha" || table.Rows[1].Name != "beta" {
		t.Fatalf("rows out of order: %s, %s", table.Rows[0].Name, table.Rows[1].Name)
	}
	if got, want := table.Rows[0].Cells, []string{"alpha", "red", "working"}; !equal(got, want) {
		t.Fatalf("alpha cells = %v, want %v", got, want)
	}
	if table.Rows[0].ID == "" {
		t.Fatal("row is missing the resource id")
	}
}

func TestListSelectorFilters(t *testing.T) {
	store := newStore(t)
	labeled, err := store.Put(context.Background(), resource.Resource{
		APIVersion: widgetAPI, Kind: widgetKind, Name: "tagged",
		Labels: map[string]string{"team": "core"},
		Spec:   json.RawMessage(`{"color":"green"}`),
	})
	if err != nil {
		t.Fatalf("put labeled: %v", err)
	}
	putWidget(t, store, "plain", "white", "idle")

	sel, err := resource.ParseSelector("team=core")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	table, err := controlplane.List(context.Background(), store, descriptor(), sel)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(table.Rows) != 1 || table.Rows[0].ID != labeled.ID {
		t.Fatalf("selector did not filter to the labeled resource: %+v", table.Rows)
	}
}

func TestSpecAndStatusFieldMissingReturnsEmpty(t *testing.T) {
	store := newStore(t)
	r, err := store.Put(context.Background(), resource.Resource{
		APIVersion: widgetAPI, Kind: widgetKind, Name: "bare",
	})
	if err != nil {
		t.Fatalf("put bare: %v", err)
	}
	if got := controlplane.SpecField("color")(r); got != "" {
		t.Fatalf("missing spec field = %q, want empty", got)
	}
	if got := controlplane.StatusField("state")(r); got != "" {
		t.Fatalf("missing status field = %q, want empty", got)
	}
}

func TestDescribeReturnsResourceAndOwnHistory(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	created := putWidget(t, store, "subject", "red", "idle")
	putWidget(t, store, "subject", "blue", "working") // update: same id, new event
	putWidget(t, store, "other", "green", "idle")     // unrelated, must be excluded

	detail, err := controlplane.Describe(ctx, store, logOf(t, store), descriptor(), created.ID, 0)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if detail.Resource.ID != created.ID {
		t.Fatalf("describe returned id %s, want %s", detail.Resource.ID, created.ID)
	}
	// Current projection reflects the latest write.
	if got, want := detail.Row.Cells, []string{"subject", "blue", "working"}; !equal(got, want) {
		t.Fatalf("row cells = %v, want %v", got, want)
	}
	if len(detail.Events) != 2 {
		t.Fatalf("history events = %d, want 2 (create + update)", len(detail.Events))
	}
	for _, ev := range detail.Events {
		r, err := resource.DecodeResource(ev.Payload)
		if err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if r.ID != created.ID {
			t.Fatalf("history leaked an event for id %s", r.ID)
		}
	}
}

func TestDescribeTailCapsHistory(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	created := putWidget(t, store, "subject", "c0", "idle")
	putWidget(t, store, "subject", "c1", "idle")
	putWidget(t, store, "subject", "c2", "idle")

	detail, err := controlplane.Describe(ctx, store, logOf(t, store), descriptor(), created.ID, 2)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(detail.Events) != 2 {
		t.Fatalf("tail=2 returned %d events, want 2", len(detail.Events))
	}
}

func TestDescribeUnknownIDErrors(t *testing.T) {
	store := newStore(t)
	_, err := controlplane.Describe(context.Background(), store, logOf(t, store), descriptor(), "does-not-exist", 0)
	if err == nil {
		t.Fatal("describe of unknown id should error")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
