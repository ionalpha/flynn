package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// testExtStore returns an in-memory resource store that admits the Extension kind, so
// the dev-workflow cores are exercised without touching sqlite or the filesystem.
func testExtStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := extension.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return resource.NewMemory(reg)
}

func TestBuildDevSpecProcessSurface(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "token-ext")
	spec, err := buildDevSpec("token", abs,
		[]string{"crypto.token"}, []string{"mint", "verify"}, []string{"rpc.example"}, []string{"--net", "devnet"})
	if err != nil {
		t.Fatalf("buildDevSpec: %v", err)
	}
	if spec.DisplayName != "token" {
		t.Fatalf("display name: %q", spec.DisplayName)
	}
	if got := spec.Capabilities; len(got) != 1 || got[0] != "crypto.token" {
		t.Fatalf("capabilities: %v", got)
	}
	if got := spec.Safety.EgressAllow; len(got) != 1 || got[0] != "rpc.example" {
		t.Fatalf("egress: %v", got)
	}
	raw, ok := spec.Surface(extension.SurfaceProcess)
	if !ok {
		t.Fatal("no process surface")
	}
	var block extension.ProcessBlock
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatalf("decode block: %v", err)
	}
	if block.Dev == nil || block.Dev.Path != abs {
		t.Fatalf("dev path: %+v", block.Dev)
	}
	if block.Release != nil {
		t.Fatal("a dev link must not carry a release source")
	}
	if strings.Join(block.Tools, ",") != "mint,verify" {
		t.Fatalf("tools: %v", block.Tools)
	}
	if strings.Join(block.Args, ",") != "--net,devnet" {
		t.Fatalf("args: %v", block.Args)
	}
}

func TestBuildDevSpecRejectsRelativePath(t *testing.T) {
	if _, err := buildDevSpec("token", "relative/path", nil, nil, nil, nil); err == nil {
		t.Fatal("expected a relative binary path to be rejected")
	}
}

func TestPutDevExtensionPersistsMarkedDev(t *testing.T) {
	ctx := context.Background()
	store := testExtStore(t)
	abs := filepath.Join(t.TempDir(), "ext-bin")

	spec, err := buildDevSpec("demo", abs, []string{"demo.cap"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildDevSpec: %v", err)
	}
	if err := putDevExtension(ctx, store, "demo", spec); err != nil {
		t.Fatalf("putDevExtension: %v", err)
	}

	got, err := store.Get(ctx, extension.Kind, resource.Scope{}, "demo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Labels[catalog.SourceLabel] != catalog.SourceDev {
		t.Fatalf("source label: %q", got.Labels[catalog.SourceLabel])
	}
	decoded, err := extension.DecodeSpec(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := decoded.Surface(extension.SurfaceProcess); !ok {
		t.Fatal("stored spec lost its process surface")
	}
}

func TestPutDevExtensionPreservesStatusOnRelink(t *testing.T) {
	ctx := context.Background()
	store := testExtStore(t)
	abs := filepath.Join(t.TempDir(), "ext-bin")
	spec, err := buildDevSpec("demo", abs, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildDevSpec: %v", err)
	}

	// Seed a resource carrying an observed status (as a prior enable would).
	specRaw, _ := spec.Encode()
	status := json.RawMessage(`{"enabled":true}`)
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: extension.GroupVersion,
		Kind:       extension.Kind,
		Name:       "demo",
		Labels:     map[string]string{catalog.SourceLabel: catalog.SourceDev},
		Spec:       specRaw,
		Status:     status,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Re-link a rebuilt binary: the status must survive.
	if err := putDevExtension(ctx, store, "demo", spec); err != nil {
		t.Fatalf("relink: %v", err)
	}
	got, err := store.Get(ctx, extension.Kind, resource.Scope{}, "demo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status) == 0 || !strings.Contains(string(got.Status), "enabled") {
		t.Fatalf("status not preserved on relink: %q", got.Status)
	}
}

func TestListExtensionsMarksDevUnsigned(t *testing.T) {
	ctx := context.Background()
	store := testExtStore(t)
	abs := filepath.Join(t.TempDir(), "ext-bin")

	spec, _ := buildDevSpec("devext", abs, nil, nil, nil, nil)
	if err := putDevExtension(ctx, store, "devext", spec); err != nil {
		t.Fatalf("put dev: %v", err)
	}
	// A bundled entry to contrast against: it must list as signed.
	bundledSpec, _ := extension.Spec{DisplayName: "bundled"}.Encode()
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: extension.GroupVersion,
		Kind:       extension.Kind,
		Name:       "bundledext",
		Labels:     map[string]string{catalog.SourceLabel: catalog.SourceBundled},
		Spec:       bundledSpec,
	}); err != nil {
		t.Fatalf("put bundled: %v", err)
	}

	var buf bytes.Buffer
	if err := listExtensions(ctx, store, &buf); err != nil {
		t.Fatalf("list: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"devext", "dev", "DEV", "bundledext", "bundled", "yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing missing %q:\n%s", want, out)
		}
	}
	// The dev row must not be mislabeled signed.
	devLine := lineContaining(out, "devext")
	if strings.Contains(devLine, "yes") {
		t.Fatalf("dev extension shown as signed: %q", devLine)
	}
}

func TestDeleteDevExtensionRefusesNonDev(t *testing.T) {
	ctx := context.Background()
	store := testExtStore(t)

	bundledSpec, _ := extension.Spec{}.Encode()
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: extension.GroupVersion,
		Kind:       extension.Kind,
		Name:       "bundledext",
		Labels:     map[string]string{catalog.SourceLabel: catalog.SourceBundled},
		Spec:       bundledSpec,
	}); err != nil {
		t.Fatalf("put bundled: %v", err)
	}
	if err := deleteDevExtension(ctx, store, "bundledext"); err == nil {
		t.Fatal("expected refusal to delete a bundled extension")
	}
	if _, err := store.Get(ctx, extension.Kind, resource.Scope{}, "bundledext"); err != nil {
		t.Fatalf("bundled extension should survive a refused delete: %v", err)
	}
}

func TestDeleteDevExtensionRemovesDevLink(t *testing.T) {
	ctx := context.Background()
	store := testExtStore(t)
	abs := filepath.Join(t.TempDir(), "ext-bin")
	spec, _ := buildDevSpec("devext", abs, nil, nil, nil, nil)
	if err := putDevExtension(ctx, store, "devext", spec); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := deleteDevExtension(ctx, store, "devext"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, extension.Kind, resource.Scope{}, "devext"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("dev extension should be gone, got err=%v", err)
	}
}

func TestDeleteDevExtensionUnknown(t *testing.T) {
	if err := deleteDevExtension(context.Background(), testExtStore(t), "nope"); err == nil {
		t.Fatal("expected an error removing an unknown extension")
	}
}

func TestValidateExtensionName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"token", true},
		{"my-ext_1", true},
		{"", false},
		{"has.dot", false},
		{"has space", false},
		{"has/slash", false},
		{"has\\back", false},
	} {
		err := validateExtensionName(tc.name)
		if tc.ok && err != nil {
			t.Errorf("%q: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%q: expected an error", tc.name)
		}
	}
}

func TestSplitList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a,b, c ", []string{"a", "b", "c"}},
		{",,a,,", []string{"a"}},
	} {
		got := splitList(tc.in)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// fakeTool is a mission.Tool with a fixed name, for the tool-resolution test.
type fakeTool struct{ name string }

func (t fakeTool) Def() llm.Tool { return llm.Tool{Name: t.name} }
func (t fakeTool) Invoke(context.Context, json.RawMessage) (string, error) {
	return "", nil
}

func TestFindExtensionTool(t *testing.T) {
	tools := []mission.Tool{fakeTool{name: "token.mint"}, fakeTool{name: "token.verify"}}

	if findExtensionTool(tools, "token", "mint") == nil {
		t.Error("bare tool name should match the namespaced mount")
	}
	if findExtensionTool(tools, "token", "token.verify") == nil {
		t.Error("namespaced tool name should match directly")
	}
	if findExtensionTool(tools, "token", "burn") != nil {
		t.Error("an unadvertised tool must not match")
	}
	// A bare name that collides with another extension's prefix must not cross over.
	if findExtensionTool(tools, "other", "mint") != nil {
		t.Error("bare name resolved against the wrong extension")
	}
}

func lineContaining(s, sub string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, sub) {
			return ln
		}
	}
	return ""
}
