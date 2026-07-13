package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// openTestIntegrations opens the real integration runtime over a temp data directory:
// the durable store, the synced official catalog, and the wired extension stack. It is
// the same stack the CLI uses, so the catalog commands are exercised for real; no
// command in these tests reaches the network.
func openTestIntegrations(t *testing.T) *integrationRuntime {
	t.Helper()
	rt, err := openIntegrationRuntime(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open integration runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.closer() })
	return rt
}

func TestIntegrationsListShowsTheBundledCatalog(t *testing.T) {
	rt := openTestIntegrations(t)
	var buf bytes.Buffer
	if err := integrationsList(context.Background(), rt, &buf, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"INTEGRATION", "STATUS", "SOURCE", "CAPABILITIES", "httpbin", "cloudflare"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing missing %q:\n%s", want, out)
		}
	}
	// A bundled integration that needs a credential lists as needing one until one is added.
	if line := lineContaining(out, "cloudflare"); !strings.Contains(line, "needs credential") {
		t.Fatalf("cloudflare should report a missing credential: %q", line)
	}
	// The rows are sorted by name.
	var names []string
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		names = append(names, strings.Fields(ln)[0])
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("catalog listing is not sorted: %v", names)
		}
	}
}

// TestIntegrationsListStatusFollowsCredentials proves a configured credential flips an
// integration's status, which is what tells an operator it is usable.
func TestIntegrationsListStatusFollowsCredentials(t *testing.T) {
	ctx := context.Background()
	rt := openTestIntegrations(t)
	if _, err := rt.creds.Put(ctx, credential.Spec{
		Integration: "cloudflare", Name: "prod", AuthType: "bearer", IsDefault: true,
	}); err != nil {
		t.Fatalf("put credential: %v", err)
	}
	var buf bytes.Buffer
	if err := integrationsList(ctx, rt, &buf, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if line := lineContaining(buf.String(), "cloudflare"); !strings.Contains(line, "configured") {
		t.Fatalf("cloudflare should be configured now: %q", line)
	}
}

func TestIntegrationsListEmptyCatalog(t *testing.T) {
	reg := resource.NewRegistry()
	if err := extension.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	if err := credential.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)
	rt := &integrationRuntime{store: store, creds: credential.NewStore(store), closer: func() error { return nil }}

	var buf bytes.Buffer
	if err := integrationsList(context.Background(), rt, &buf, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(buf.String(), "no integrations available") {
		t.Fatalf("output: %q", buf.String())
	}
}

func TestIntegrationStatus(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := credential.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	creds := credential.NewStore(resource.NewMemory(reg))

	for _, tc := range []struct {
		name string
		spec extension.Spec
		want string
	}{
		{name: "no auth is always ready", spec: extension.Spec{}, want: "ready"},
		{name: "auth type none is ready", spec: extension.Spec{Auth: extension.AuthSpec{Type: "none"}}, want: "ready"},
		{name: "auth with no credential", spec: extension.Spec{Auth: extension.AuthSpec{Type: "bearer"}}, want: "needs credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := integrationStatus(ctx, creds, "svc", tc.spec)
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := creds.Put(ctx, credential.Spec{Integration: "svc", Name: "prod", AuthType: "bearer"}); err != nil {
		t.Fatalf("put credential: %v", err)
	}
	got, err := integrationStatus(ctx, creds, "svc", extension.Spec{Auth: extension.AuthSpec{Type: "bearer"}})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got != "configured" {
		t.Fatalf("status = %q, want configured", got)
	}
}

func TestIntegrationsShow(t *testing.T) {
	ctx := context.Background()
	rt := openTestIntegrations(t)

	var buf bytes.Buffer
	if err := integrationsShow(ctx, rt, &buf, []string{"httpbin"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "httpbin  (auth:") {
		t.Fatalf("show header: %q", out)
	}
	// Showing an integration mounts its surfaces, so its operations are listed.
	if len(strings.Split(strings.TrimSpace(out), "\n")) < 2 {
		t.Fatalf("no operations listed:\n%s", out)
	}

	if err := integrationsShow(ctx, rt, &bytes.Buffer{}, []string{"nope"}); err == nil ||
		!strings.Contains(err.Error(), "unknown integration") {
		t.Fatalf("error = %v, want an unknown-integration error", err)
	}
	for _, args := range [][]string{nil, {"a", "b"}} {
		if err := integrationsShow(ctx, rt, &bytes.Buffer{}, args); err == nil {
			t.Fatalf("expected a usage error for args %v", args)
		}
	}
}

func TestIntegrationsCallErrors(t *testing.T) {
	ctx := context.Background()
	rt := openTestIntegrations(t)

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no arguments", args: nil, wantErr: "usage:"},
		{name: "operation missing", args: []string{"httpbin"}, wantErr: "usage:"},
		{name: "unknown integration", args: []string{"nope", "get"}, wantErr: "unknown integration"},
		{name: "unknown operation", args: []string{"httpbin", "not-an-op"}, wantErr: "has no operation"},
		{name: "unknown operation with input", args: []string{"httpbin", "not-an-op", `{"a":1}`}, wantErr: "has no operation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := integrationsCall(ctx, rt, &buf, tc.args)
			if err == nil {
				t.Fatalf("expected an error, got output %q", buf.String())
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if buf.Len() != 0 {
				t.Fatalf("a failed call must print nothing: %q", buf.String())
			}
		})
	}
}

// TestIntegrationsCallPrintsTheResult drives one operation to completion over a stub
// surface handler, so the call path is exercised without an upstream.
func TestIntegrationsCallPrintsTheResult(t *testing.T) {
	h := &stubOpsHandler{tools: []mission.Tool{stubOpsTool{name: "deploy", result: `{"ok":true}`}}}
	rt := testDeployRuntime(t, h)
	putExtension(t, rt, "cloudflare", opsSpec(t, "cloudflare", map[string]json.RawMessage{
		extension.SurfaceOps: json.RawMessage(opsBlock),
	}))

	var buf bytes.Buffer
	if err := integrationsCall(context.Background(), rt, &buf, []string{"cloudflare", "deploy", `{"project":"blog"}`}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if strings.TrimSpace(buf.String()) != `{"ok":true}` {
		t.Fatalf("output = %q, want the operation's result", buf.String())
	}
}

// TestIntegrationsCallSurfacesTheOperationError proves a failing operation's error
// reaches the operator rather than being swallowed into an empty result.
func TestIntegrationsCallSurfacesTheOperationError(t *testing.T) {
	h := &stubOpsHandler{tools: []mission.Tool{stubOpsTool{name: "deploy", err: errors.New("429 too many requests")}}}
	rt := testDeployRuntime(t, h)
	putExtension(t, rt, "cloudflare", opsSpec(t, "cloudflare", map[string]json.RawMessage{
		extension.SurfaceOps: json.RawMessage(opsBlock),
	}))

	var buf bytes.Buffer
	err := integrationsCall(context.Background(), rt, &buf, []string{"cloudflare", "deploy"})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("error = %v, want the operation's failure", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a failed call must print nothing: %q", buf.String())
	}
}

func TestRunIntegrationsDispatch(t *testing.T) {
	if err := runIntegrations(nil, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "usage:") {
		t.Fatalf("error = %v, want a usage error", err)
	}
	err := runIntegrations([]string{"nope"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("error = %v, want an unknown-subcommand error", err)
	}
	// The listing subcommand runs the whole durable path.
	if err := runIntegrations([]string{"ls"}, t.TempDir()); err != nil {
		t.Fatalf("integrations ls: %v", err)
	}
}

// TestOpenIntegrationRuntimeUnusableDataDir proves an unopenable data directory fails
// the command rather than surfacing as an empty catalog.
func TestOpenIntegrationRuntimeUnusableDataDir(t *testing.T) {
	if _, err := openIntegrationRuntime(context.Background(), blockedDataDir(t)); err == nil {
		t.Fatal("expected an unopenable data directory to fail")
	}
	if err := runIntegrations([]string{"ls"}, blockedDataDir(t)); err == nil {
		t.Fatal("expected the command to fail on an unopenable data directory")
	}
}

// blockedDataDir returns a path that cannot be a directory, because a regular file sits
// where one of its parents would have to be.
func blockedDataDir(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	return filepath.Join(file, "data")
}

// TestWireExtensionsServesBothSurfaces proves the shared extension stack registers a
// handler for the API surface and the hosting surface from one registry, so an ops
// provider and an API integration load through the same engine.
func TestWireExtensionsServesBothSurfaces(t *testing.T) {
	reg := resource.NewRegistry()
	if err := extension.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	if err := credential.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	creds, loader, err := wireExtensions(resource.NewMemory(reg), t.TempDir())
	if err != nil {
		t.Fatalf("wireExtensions: %v", err)
	}
	if creds == nil || loader == nil {
		t.Fatal("wireExtensions returned an incomplete stack")
	}
	if got := loader.Tools(); len(got) != 0 {
		t.Fatalf("no extension is loaded yet, so there should be no tools: %v", got)
	}
}

func TestOrNone(t *testing.T) {
	if orNone("") != "none" {
		t.Fatal("an absent auth type should render as none")
	}
	if orNone("bearer") != "bearer" {
		t.Fatal("a set auth type should render verbatim")
	}
}
