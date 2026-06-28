package extension

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// res builds an Extension resource with a fixed id and the given surface blocks.
func res(id, name string, surfaces map[string]json.RawMessage) resource.Resource {
	spec := Spec{BaseURL: "https://api.example.com", Surfaces: surfaces}
	body, _ := spec.Encode()
	return resource.Resource{APIVersion: GroupVersion, Kind: Kind, ID: id, Name: name, Spec: body}
}

func block() json.RawMessage { return json.RawMessage(`{}`) }

func TestLoaderMountsSurfacesInSortedOrder(t *testing.T) {
	reg := NewRegistry()
	integ := &recordHandler{capability: SurfaceIntegration}
	tool := &recordHandler{capability: SurfaceTool}
	auth := &recordHandler{capability: SurfaceAuth}
	for _, h := range []*recordHandler{integ, tool, auth} {
		if err := reg.Register(h); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	l := NewLoader(reg)

	mounted, err := l.Load(context.Background(), res("ext-1", "x", map[string]json.RawMessage{
		SurfaceTool:        block(),
		SurfaceIntegration: block(),
		SurfaceAuth:        block(),
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{SurfaceAuth, SurfaceIntegration, SurfaceTool} // sorted
	if len(mounted) != 3 || mounted[0] != want[0] || mounted[1] != want[1] || mounted[2] != want[2] {
		t.Fatalf("mounted not sorted: %v", mounted)
	}
	for _, h := range []*recordHandler{integ, tool, auth} {
		if h.loadCount() != 1 {
			t.Fatalf("handler %s OnLoad called %d times", h.capability, h.loadCount())
		}
	}
}

func TestLoaderFailsClosedOnUnknownSurface(t *testing.T) {
	reg := NewRegistry()
	integ := &recordHandler{capability: SurfaceIntegration}
	if err := reg.Register(integ); err != nil {
		t.Fatalf("register: %v", err)
	}
	l := NewLoader(reg)

	_, err := l.Load(context.Background(), res("ext-1", "x", map[string]json.RawMessage{
		SurfaceIntegration: block(),
		SurfaceOps:         block(), // no handler registered
	}))
	if err == nil {
		t.Fatal("expected load to fail on an unknown surface")
	}
	// Fail-closed before mounting anything: the known surface must NOT have loaded.
	if integ.loadCount() != 0 {
		t.Fatalf("known surface mounted despite an unknown sibling: %d", integ.loadCount())
	}
	if got := l.Mounted("ext-1"); len(got) != 0 {
		t.Fatalf("expected nothing mounted, got %v", got)
	}
}

func TestLoaderRollsBackOnHandlerFailure(t *testing.T) {
	reg := NewRegistry()
	// "auth" sorts first and loads fine; "integration" sorts after and refuses, so
	// the already-mounted auth surface must be rolled back.
	auth := &recordHandler{capability: SurfaceAuth}
	integ := &recordHandler{capability: SurfaceIntegration, loadErr: errLoad}
	for _, h := range []*recordHandler{auth, integ} {
		if err := reg.Register(h); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	l := NewLoader(reg)

	_, err := l.Load(context.Background(), res("ext-1", "x", map[string]json.RawMessage{
		SurfaceAuth:        block(),
		SurfaceIntegration: block(),
	}))
	if err == nil {
		t.Fatal("expected load to fail")
	}
	if auth.loadCount() != 1 {
		t.Fatalf("auth should have mounted once, got %d", auth.loadCount())
	}
	if auth.unloadCount() != 1 {
		t.Fatalf("auth should have been rolled back, unloads=%d", auth.unloadCount())
	}
	if got := l.Mounted("ext-1"); len(got) != 0 {
		t.Fatalf("nothing should remain mounted after rollback, got %v", got)
	}
}

func TestLoaderUnloadReleasesEverySurface(t *testing.T) {
	reg := NewRegistry()
	integ := &recordHandler{capability: SurfaceIntegration}
	tool := &recordHandler{capability: SurfaceTool}
	for _, h := range []*recordHandler{integ, tool} {
		_ = reg.Register(h)
	}
	l := NewLoader(reg)
	ctx := context.Background()

	if _, err := l.Load(ctx, res("ext-1", "x", map[string]json.RawMessage{
		SurfaceIntegration: block(),
		SurfaceTool:        block(),
	})); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := l.Unload(ctx, "ext-1"); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if integ.unloadCount() != 1 || tool.unloadCount() != 1 {
		t.Fatalf("both surfaces should be unloaded: integ=%d tool=%d", integ.unloadCount(), tool.unloadCount())
	}
	// Unloading again is a no-op.
	if err := l.Unload(ctx, "ext-1"); err != nil {
		t.Fatalf("idempotent unload: %v", err)
	}
	if integ.unloadCount() != 1 {
		t.Fatalf("second unload should be a no-op, got %d", integ.unloadCount())
	}
}

func TestLoaderReloadReplaces(t *testing.T) {
	reg := NewRegistry()
	integ := &recordHandler{capability: SurfaceIntegration}
	tool := &recordHandler{capability: SurfaceTool}
	for _, h := range []*recordHandler{integ, tool} {
		_ = reg.Register(h)
	}
	l := NewLoader(reg)
	ctx := context.Background()

	// First load: integration + tool.
	if _, err := l.Load(ctx, res("ext-1", "x", map[string]json.RawMessage{
		SurfaceIntegration: block(),
		SurfaceTool:        block(),
	})); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Reload with only the integration surface: the tool surface must be unmounted.
	mounted, err := l.Load(ctx, res("ext-1", "x", map[string]json.RawMessage{
		SurfaceIntegration: block(),
	}))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(mounted) != 1 || mounted[0] != SurfaceIntegration {
		t.Fatalf("reload should mount only integration, got %v", mounted)
	}
	if tool.unloadCount() != 1 {
		t.Fatalf("tool surface should have been unmounted on reload, got %d", tool.unloadCount())
	}
	// The replaced integration surface unloads then loads again: 2 loads total.
	if integ.loadCount() != 2 {
		t.Fatalf("integration should have reloaded, loads=%d", integ.loadCount())
	}
}

func TestLoaderToolBridgeAggregatesDeterministically(t *testing.T) {
	reg := NewRegistry()
	h := toolHandler{&recordHandler{
		capability: SurfaceTool,
		toolsByID: map[string][]mission.Tool{
			"ext-a": {fakeTool{"a1"}, fakeTool{"a2"}},
			"ext-b": {fakeTool{"b1"}},
		},
	}}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	l := NewLoader(reg)
	ctx := context.Background()

	// Load b before a; Tools() must still be ordered by extension id (a before b).
	if _, err := l.Load(ctx, res("ext-b", "b", map[string]json.RawMessage{SurfaceTool: block()})); err != nil {
		t.Fatalf("load b: %v", err)
	}
	if _, err := l.Load(ctx, res("ext-a", "a", map[string]json.RawMessage{SurfaceTool: block()})); err != nil {
		t.Fatalf("load a: %v", err)
	}

	tools := l.Tools()
	got := make([]string, len(tools))
	for i, tl := range tools {
		got[i] = tl.Def().Name
	}
	want := []string{"a1", "a2", "b1"}
	if len(got) != len(want) {
		t.Fatalf("got tools %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool order not deterministic: got %v want %v", got, want)
		}
	}
}

func TestLoaderRejectsResourceWithoutID(t *testing.T) {
	l := NewLoader(NewRegistry())
	_, err := l.Load(context.Background(), resource.Resource{APIVersion: GroupVersion, Kind: Kind, Name: "x"})
	if err == nil {
		t.Fatal("expected a resource with no id to be refused")
	}
}
