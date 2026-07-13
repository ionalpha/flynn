package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// stubOpsTool is a hosting operation with a fixed result, so a deploy is exercised
// end to end without a provider or a network call.
type stubOpsTool struct {
	name   string
	result string
	err    error
}

func (t stubOpsTool) Def() llm.Tool { return llm.Tool{Name: t.name, Description: "stub " + t.name} }

func (t stubOpsTool) Invoke(_ context.Context, in json.RawMessage) (string, error) {
	if t.err != nil {
		return "", t.err
	}
	if len(in) == 0 {
		return "", errors.New("stub tool: no input")
	}
	return t.result, nil
}

// stubOpsHandler serves the ops surface with a fixed tool set, standing in for the real
// ops handler so the deploy command's own logic is what is under test.
type stubOpsHandler struct {
	tools   []mission.Tool
	loadErr error
	lastIn  json.RawMessage
}

func (h *stubOpsHandler) Capability() string { return extension.SurfaceOps }

func (h *stubOpsHandler) OnLoad(_ context.Context, m extension.Mount) error {
	h.lastIn = m.Block
	return h.loadErr
}

func (h *stubOpsHandler) OnUnload(context.Context, string) error { return nil }

func (h *stubOpsHandler) Tools(string) []mission.Tool { return h.tools }

// testDeployRuntime builds an integration runtime over an in-memory store whose ops
// surface is served by the stub handler.
func testDeployRuntime(t *testing.T, h *stubOpsHandler) *integrationRuntime {
	t.Helper()
	reg := resource.NewRegistry()
	for _, register := range []func(*resource.Registry) error{
		extension.RegisterKind, service.RegisterKind, credential.RegisterKind,
	} {
		if err := register(reg); err != nil {
			t.Fatalf("register kind: %v", err)
		}
	}
	store := resource.NewMemory(reg)
	ereg := extension.NewRegistry()
	if err := ereg.Register(h); err != nil {
		t.Fatalf("register handler: %v", err)
	}
	return &integrationRuntime{
		store:  store,
		creds:  credential.NewStore(store),
		svc:    service.NewStore(store),
		loader: extension.NewLoader(ereg),
		closer: func() error { return nil },
	}
}

// opsSpec is a hosting extension declaring one static-site target and a deploy
// operation, the minimum a provider must publish.
func opsSpec(t *testing.T, provider string, surfaces map[string]json.RawMessage) json.RawMessage {
	t.Helper()
	raw, err := extension.Spec{
		DisplayName:  provider,
		Provider:     provider,
		Capabilities: []string{"hosting.deploy"},
		Auth:         extension.AuthSpec{Type: "bearer", CredentialRef: "cf/prod"},
		Surfaces:     surfaces,
	}.Encode()
	if err != nil {
		t.Fatalf("encode spec: %v", err)
	}
	return raw
}

func putExtension(t *testing.T, rt *integrationRuntime, name string, spec json.RawMessage) {
	t.Helper()
	if _, err := rt.store.Put(context.Background(), resource.Resource{
		APIVersion: extension.GroupVersion,
		Kind:       extension.Kind,
		Name:       name,
		Spec:       spec,
	}); err != nil {
		t.Fatalf("put extension %q: %v", name, err)
	}
}

// testClock is the deterministic time source the deploy path records with, so the
// stored LastDeploy is asserted exactly rather than approximated.
func testClock() clock.Clock {
	return clock.NewManual(time.Date(2031, 5, 4, 3, 2, 1, 0, time.UTC))
}

const opsBlock = `{"targets":["static-site","container"],"operations":[{"name":"deploy"}]}`

func TestParseDeployArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    deployArgs
		wantErr bool
	}{
		{
			name: "extension only defaults the input to an empty object",
			args: []string{"cloudflare"},
			want: deployArgs{ext: "cloudflare", input: json.RawMessage(`{}`)},
		},
		{
			name: "flags and a json payload",
			args: []string{"cloudflare", "--name", "site", "--target", "static-site", `{"project":"blog"}`},
			want: deployArgs{ext: "cloudflare", name: "site", target: "static-site", input: json.RawMessage(`{"project":"blog"}`)},
		},
		{
			name: "a payload split by the shell is rejoined",
			args: []string{"cf", `{"a":`, `1}`},
			want: deployArgs{ext: "cf", input: json.RawMessage(`{"a": 1}`)},
		},
		{name: "no arguments", args: nil, wantErr: true},
		{name: "a flag cannot stand in for the extension", args: []string{"--name", "site"}, wantErr: true},
		{name: "an unknown flag fails parsing", args: []string{"cf", "--nope"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDeployArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDeployArgs: %v", err)
			}
			if got.ext != tc.want.ext || got.name != tc.want.name || got.target != tc.want.target {
				t.Fatalf("parsed = %+v, want %+v", got, tc.want)
			}
			if string(got.input) != string(tc.want.input) {
				t.Fatalf("input = %q, want %q", got.input, tc.want.input)
			}
		})
	}
}

func TestDeployExtensionRegistersService(t *testing.T) {
	ctx := context.Background()
	h := &stubOpsHandler{tools: []mission.Tool{stubOpsTool{
		name:   "deploy",
		result: `{"result":{"id":"dep-9","url":"https://blog.example","address":{"account":"acct-1","project":"blog","empty":""}}}`,
	}}}
	rt := testDeployRuntime(t, h)
	putExtension(t, rt, "cloudflare", opsSpec(t, "cloudflare", map[string]json.RawMessage{
		extension.SurfaceOps: json.RawMessage(opsBlock),
	}))

	var buf bytes.Buffer
	args := deployArgs{ext: "cloudflare", name: "blog", input: json.RawMessage(`{"project":"blog"}`)}
	if err := deployExtension(ctx, rt, &buf, testClock(), args); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	svc, err := rt.svc.Get(ctx, "blog")
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Provider != "cloudflare" || svc.Spec.URL != "https://blog.example" || svc.Spec.ExternalID != "dep-9" {
		t.Fatalf("service spec: %+v", svc.Spec)
	}
	// No --target was given, so the provider's first declared target is recorded.
	if svc.Spec.Target != service.TargetStaticSite {
		t.Fatalf("target = %q, want the provider's first declared target", svc.Spec.Target)
	}
	if svc.Spec.DesiredState != service.StateRunning {
		t.Fatalf("desired state = %q", svc.Spec.DesiredState)
	}
	if svc.Spec.Credential != "cf/prod" {
		t.Fatalf("credential ref = %q", svc.Spec.Credential)
	}
	// The provider's opaque addressing is recorded, minus the empty value.
	if svc.Spec.Address["account"] != "acct-1" || svc.Spec.Address["project"] != "blog" {
		t.Fatalf("address: %+v", svc.Spec.Address)
	}
	if _, ok := svc.Spec.Address["empty"]; ok {
		t.Fatalf("an empty address value must not be recorded: %+v", svc.Spec.Address)
	}
	// The deploy time comes from the injected clock, not the wall clock.
	if svc.Status.Phase != "deployed" || svc.Status.LastDeploy != "2031-05-04T03:02:01Z" {
		t.Fatalf("status: %+v", svc.Status)
	}
	out := buf.String()
	for _, want := range []string{"Deployed blog via cloudflare", "-> https://blog.example", `Registered service "blog"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	// The declared block reaches the surface handler unaltered.
	if !strings.Contains(string(h.lastIn), "static-site") {
		t.Fatalf("ops block not passed to the handler: %q", h.lastIn)
	}
}

// TestDeployExtensionExplicitTargetAndDefaults proves an explicit --target wins over the
// provider's declared one, the service name defaults to the extension name, and a
// provider-less spec falls back to the extension name.
func TestDeployExtensionExplicitTargetAndDefaults(t *testing.T) {
	ctx := context.Background()
	h := &stubOpsHandler{tools: []mission.Tool{stubOpsTool{name: "deploy", result: `{"deployment_id":"srv-1"}`}}}
	rt := testDeployRuntime(t, h)
	spec, err := extension.Spec{
		Surfaces: map[string]json.RawMessage{extension.SurfaceOps: json.RawMessage(opsBlock)},
	}.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	putExtension(t, rt, "render", spec)

	var buf bytes.Buffer
	args := deployArgs{ext: "render", target: "container", input: json.RawMessage(`{}`)}
	if err := deployExtension(ctx, rt, &buf, testClock(), args); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	svc, err := rt.svc.Get(ctx, "render")
	if err != nil {
		t.Fatalf("the service should be named after the extension: %v", err)
	}
	if svc.Spec.Target != service.TargetContainer {
		t.Fatalf("an explicit target must win, got %q", svc.Spec.Target)
	}
	if svc.Spec.Provider != "render" {
		t.Fatalf("a spec with no provider falls back to the extension name, got %q", svc.Spec.Provider)
	}
	if svc.Spec.ExternalID != "srv-1" || svc.Spec.URL != "" {
		t.Fatalf("spec: %+v", svc.Spec)
	}
	// With no URL the arrow is omitted.
	if strings.Contains(buf.String(), "->") {
		t.Fatalf("no URL was returned, so no arrow should be printed:\n%s", buf.String())
	}
}

func TestDeployExtensionErrors(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		handler *stubOpsHandler
		specs   map[string]json.RawMessage
		args    deployArgs
		wantErr string
	}{
		{
			name:    "unknown extension",
			handler: &stubOpsHandler{},
			args:    deployArgs{ext: "nope", input: json.RawMessage(`{}`)},
			wantErr: "unknown extension",
		},
		{
			name:    "not a hosting provider",
			handler: &stubOpsHandler{},
			specs:   map[string]json.RawMessage{extension.SurfaceTool: json.RawMessage(`{}`)},
			args:    deployArgs{ext: "cloudflare", input: json.RawMessage(`{}`)},
			wantErr: "not a hosting provider",
		},
		{
			name:    "unknown target",
			handler: &stubOpsHandler{},
			specs:   map[string]json.RawMessage{extension.SurfaceOps: json.RawMessage(opsBlock)},
			args:    deployArgs{ext: "cloudflare", target: "toaster", input: json.RawMessage(`{}`)},
			wantErr: `unknown --target "toaster"`,
		},
		{
			name:    "surface refuses to load",
			handler: &stubOpsHandler{loadErr: errors.New("bad ops block")},
			specs:   map[string]json.RawMessage{extension.SurfaceOps: json.RawMessage(opsBlock)},
			args:    deployArgs{ext: "cloudflare", input: json.RawMessage(`{}`)},
			wantErr: "bad ops block",
		},
		{
			name:    "provider declares no deploy operation",
			handler: &stubOpsHandler{tools: []mission.Tool{stubOpsTool{name: "status"}}},
			specs:   map[string]json.RawMessage{extension.SurfaceOps: json.RawMessage(opsBlock)},
			args:    deployArgs{ext: "cloudflare", input: json.RawMessage(`{}`)},
			wantErr: `declares no "deploy" operation`,
		},
		{
			name:    "the deploy operation fails",
			handler: &stubOpsHandler{tools: []mission.Tool{stubOpsTool{name: "deploy", err: errors.New("402 payment required")}}},
			specs:   map[string]json.RawMessage{extension.SurfaceOps: json.RawMessage(opsBlock)},
			args:    deployArgs{ext: "cloudflare", input: json.RawMessage(`{}`)},
			wantErr: "the deploy operation failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := testDeployRuntime(t, tc.handler)
			if tc.specs != nil {
				putExtension(t, rt, "cloudflare", opsSpec(t, "cloudflare", tc.specs))
			}
			var buf bytes.Buffer
			err := deployExtension(ctx, rt, &buf, testClock(), tc.args)
			if err == nil {
				t.Fatalf("expected an error, got output:\n%s", buf.String())
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if buf.Len() != 0 {
				t.Fatalf("a failed deploy must print no success line: %q", buf.String())
			}
		})
	}
}

// TestDeployExtensionReportsRegistrationFailure proves a workload that deployed but
// could not be recorded is reported as exactly that, rather than as a failed deploy.
// The store here admits no Service kind, so the registration is refused.
func TestDeployExtensionReportsRegistrationFailure(t *testing.T) {
	reg := resource.NewRegistry()
	if err := extension.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)
	ereg := extension.NewRegistry()
	h := &stubOpsHandler{tools: []mission.Tool{stubOpsTool{name: "deploy", result: `{}`}}}
	if err := ereg.Register(h); err != nil {
		t.Fatalf("register handler: %v", err)
	}
	rt := &integrationRuntime{
		store:  store,
		creds:  credential.NewStore(store),
		svc:    service.NewStore(store),
		loader: extension.NewLoader(ereg),
		closer: func() error { return nil },
	}
	putExtension(t, rt, "cloudflare", opsSpec(t, "cloudflare", map[string]json.RawMessage{
		extension.SurfaceOps: json.RawMessage(opsBlock),
	}))

	args := deployArgs{ext: "cloudflare", input: json.RawMessage(`{}`)}
	err := deployExtension(context.Background(), rt, &bytes.Buffer{}, testClock(), args)
	if err == nil || !strings.Contains(err.Error(), "registering the service failed") {
		t.Fatalf("error = %v, want a registration failure", err)
	}
}

// TestDeployExtensionRejectsBadTargetBeforeDeploying proves the target is validated
// before the provider is driven, so an operator typo cannot stand a workload up that
// then goes unregistered.
func TestDeployExtensionRejectsBadTargetBeforeDeploying(t *testing.T) {
	invoked := false
	h := &stubOpsHandler{tools: []mission.Tool{invokeSpy{onInvoke: func() { invoked = true }}}}
	rt := testDeployRuntime(t, h)
	putExtension(t, rt, "cloudflare", opsSpec(t, "cloudflare", map[string]json.RawMessage{
		extension.SurfaceOps: json.RawMessage(opsBlock),
	}))
	args := deployArgs{ext: "cloudflare", target: "toaster", input: json.RawMessage(`{}`)}
	if err := deployExtension(context.Background(), rt, &bytes.Buffer{}, testClock(), args); err == nil {
		t.Fatal("expected an unknown target to be refused")
	}
	if invoked {
		t.Fatal("the provider was driven despite an invalid target")
	}
}

// invokeSpy records whether the deploy operation was ever driven.
type invokeSpy struct{ onInvoke func() }

func (invokeSpy) Def() llm.Tool { return llm.Tool{Name: "deploy"} }

func (s invokeSpy) Invoke(context.Context, json.RawMessage) (string, error) {
	s.onInvoke()
	return `{}`, nil
}

func TestFirstTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec extension.Spec
		want service.Target
	}{
		{name: "no ops surface"},
		{
			name: "malformed ops block",
			spec: extension.Spec{Surfaces: map[string]json.RawMessage{extension.SurfaceOps: json.RawMessage(`"not an object"`)}},
		},
		{
			name: "ops surface declaring no targets",
			spec: extension.Spec{Surfaces: map[string]json.RawMessage{extension.SurfaceOps: json.RawMessage(`{"operations":[]}`)}},
		},
		{
			name: "first declared target wins",
			spec: extension.Spec{Surfaces: map[string]json.RawMessage{extension.SurfaceOps: json.RawMessage(opsBlock)}},
			want: service.TargetStaticSite,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstTarget(tc.spec); got != tc.want {
				t.Fatalf("firstTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractDeployResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		wantURL string
		wantID  string
		wantAdr map[string]string
	}{
		{name: "not json", raw: "boom"},
		{name: "not an object", raw: `["a"]`},
		{name: "empty object", raw: `{}`},
		{
			name:    "bare object",
			raw:     `{"url":"https://a.example","id":"x1"}`,
			wantURL: "https://a.example",
			wantID:  "x1",
		},
		{
			name:    "wrapped under result",
			raw:     `{"result":{"deployment_url":"https://b.example","uid":"u2"}}`,
			wantURL: "https://b.example",
			wantID:  "u2",
		},
		{
			name:    "field preference order",
			raw:     `{"url":"https://first.example","live_url":"https://second.example","id":"i1","deployment_id":"i2"}`,
			wantURL: "https://first.example",
			wantID:  "i1",
		},
		{
			name:    "empty strings are skipped for the next candidate",
			raw:     `{"url":"","live_url":"https://live.example","id":"","external_id":"e9"}`,
			wantURL: "https://live.example",
			wantID:  "e9",
		},
		{
			name:    "non-string address values are dropped",
			raw:     `{"address":{"account":"a1","port":8080}}`,
			wantAdr: map[string]string{"account": "a1"},
		},
		{name: "an address with no usable values is nil", raw: `{"address":{"port":8080}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, id, addr := extractDeployResult(tc.raw)
			if url != tc.wantURL || id != tc.wantID {
				t.Fatalf("url=%q id=%q, want url=%q id=%q", url, id, tc.wantURL, tc.wantID)
			}
			if len(addr) != len(tc.wantAdr) {
				t.Fatalf("address = %+v, want %+v", addr, tc.wantAdr)
			}
			for k, v := range tc.wantAdr {
				if addr[k] != v {
					t.Fatalf("address[%q] = %q, want %q", k, addr[k], v)
				}
			}
		})
	}
}

func TestFirstString(t *testing.T) {
	m := map[string]any{"a": "", "b": "beta", "c": 3}
	if got := firstString(m, "a", "c", "b"); got != "beta" {
		t.Fatalf("firstString = %q, want the first non-empty string value", got)
	}
	if got := firstString(m, "missing"); got != "" {
		t.Fatalf("firstString = %q, want empty", got)
	}
}

func TestOrDash(t *testing.T) {
	if orDash("") != "-" {
		t.Fatal("an empty column should render as a dash")
	}
	if orDash("x") != "x" {
		t.Fatal("a set column should render verbatim")
	}
}

func TestServicesListEmptyAndPopulated(t *testing.T) {
	ctx := context.Background()
	rt := testDeployRuntime(t, &stubOpsHandler{})

	var empty bytes.Buffer
	if err := servicesList(ctx, rt, &empty, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(empty.String(), "no services deployed") {
		t.Fatalf("empty listing: %q", empty.String())
	}

	if _, err := rt.svc.Put(ctx, "blog", service.Spec{
		Provider: "cloudflare", Target: service.TargetStaticSite,
		DesiredState: service.StateRunning, URL: "https://blog.example",
	}, service.Status{Phase: "deployed"}); err != nil {
		t.Fatalf("put blog: %v", err)
	}
	// A service with no target, state, or URL exercises the dash columns.
	if _, err := rt.svc.Put(ctx, "api", service.Spec{Provider: "render"}, service.Status{}); err != nil {
		t.Fatalf("put api: %v", err)
	}

	var buf bytes.Buffer
	if err := servicesList(ctx, rt, &buf, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "PROVIDER", "blog", "cloudflare", "static-site", "running", "https://blog.example", "render"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing missing %q:\n%s", want, out)
		}
	}
	if apiLine := lineContaining(out, "api"); !strings.Contains(apiLine, "-") {
		t.Fatalf("an unset column should render as a dash: %q", apiLine)
	}
}

func TestServicesRemove(t *testing.T) {
	ctx := context.Background()
	rt := testDeployRuntime(t, &stubOpsHandler{})
	if _, err := rt.svc.Put(ctx, "blog", service.Spec{Provider: "cloudflare"}, service.Status{}); err != nil {
		t.Fatalf("put: %v", err)
	}

	var buf bytes.Buffer
	if err := servicesRemove(ctx, rt, &buf, []string{"blog"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(buf.String(), `Removed service "blog"`) {
		t.Fatalf("output: %q", buf.String())
	}
	if _, err := rt.svc.Get(ctx, "blog"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("the record should be gone, got %v", err)
	}
	// Removing an untracked service is not an error: the record is already retired.
	if err := servicesRemove(ctx, rt, &bytes.Buffer{}, []string{"gone"}); err != nil {
		t.Fatalf("removing an unknown service: %v", err)
	}
	// Wrong arity is a usage error.
	for _, args := range [][]string{nil, {"a", "b"}} {
		if err := servicesRemove(ctx, rt, &bytes.Buffer{}, args); err == nil {
			t.Fatalf("expected a usage error for args %v", args)
		}
	}
}

// TestRunServicesUnknownSubcommand proves the dispatch rejects an unknown subcommand
// before it opens the durable store.
func TestRunServicesUnknownSubcommand(t *testing.T) {
	err := runServices([]string{"nope"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("error = %v, want an unknown-subcommand error", err)
	}
}

// TestRunServicesListOnRealStore exercises the durable path the CLI takes: open the
// data directory, sync the catalog, and list an empty service set.
func TestRunServicesListOnRealStore(t *testing.T) {
	if err := runServices(nil, t.TempDir()); err != nil {
		t.Fatalf("services ls: %v", err)
	}
}

// TestRunDeployUnknownExtension drives the real command entry point (durable store,
// synced catalog, wired extension stack) down to the unknown-extension refusal, with no
// provider call.
func TestRunDeployUnknownExtension(t *testing.T) {
	err := runDeploy([]string{"not-an-extension"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unknown extension") {
		t.Fatalf("error = %v, want an unknown-extension error", err)
	}
	if err := runDeploy(nil, t.TempDir()); err == nil {
		t.Fatal("expected a usage error with no arguments")
	}
}
