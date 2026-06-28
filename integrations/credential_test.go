package integrations

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/credential"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/integrations/request"
	"github.com/ionalpha/flynn/resource"
)

func credStore(t *testing.T, specs ...credential.Spec) *credential.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := credential.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	s := credential.NewStore(resource.NewMemory(reg))
	for _, sp := range specs {
		if _, err := s.Put(context.Background(), sp); err != nil {
			t.Fatalf("put credential %s/%s: %v", sp.Integration, sp.Name, err)
		}
	}
	return s
}

// TestCredentialResolvedByDefault proves a bare integration reference selects the
// integration's default credential and signs the request with its vault secret.
func TestCredentialResolvedByDefault(t *testing.T) {
	store := credStore(
		t,
		credential.Spec{Integration: "cf", Name: "prod", AuthType: "bearer", Role: credential.RoleAdmin, IsDefault: true},
		credential.Spec{Integration: "cf", Name: "staging", AuthType: "bearer", Role: credential.RoleRead},
	)
	doer := &fakeDoer{reply: func(_ *http.Request) (int, string) { return 200, `{}` }}
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"cf/prod": "prod-token", "cf/staging": "staging-token"}),
		WithCredentials(store),
	)
	// AuthSpec credential ref is the bare integration, so the default is chosen.
	loader, _ := loadExtension(t, h,
		extension.AuthSpec{Type: "bearer", CredentialRef: "cf"},
		"https://api.example.com",
		`{"operations":[{"name":"op","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/x"}},{"op":"return","return":{"value":"{{steps.r.status}}"}}]}}]}`)

	if _, err := loader.Tools()[0].Invoke(context.Background(), nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if doer.gotAuth != "Bearer prod-token" {
		t.Fatalf("expected the default credential's token, got %q", doer.gotAuth)
	}
}

// TestCredentialResolvedByName proves a "integration/name" reference selects that
// credential specifically.
func TestCredentialResolvedByName(t *testing.T) {
	store := credStore(
		t,
		credential.Spec{Integration: "cf", Name: "prod", AuthType: "bearer", Role: credential.RoleAdmin, IsDefault: true},
		credential.Spec{Integration: "cf", Name: "staging", AuthType: "bearer", Role: credential.RoleAdmin},
	)
	doer := &fakeDoer{reply: func(_ *http.Request) (int, string) { return 200, `{}` }}
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"cf/prod": "prod-token", "cf/staging": "staging-token"}),
		WithCredentials(store),
	)
	loader, _ := loadExtension(t, h,
		extension.AuthSpec{Type: "bearer", CredentialRef: "cf/staging"},
		"https://api.example.com",
		`{"operations":[{"name":"op","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/x"}},{"op":"return","return":{"value":"ok"}}]}}]}`)

	if _, err := loader.Tools()[0].Invoke(context.Background(), nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if doer.gotAuth != "Bearer staging-token" {
		t.Fatalf("expected the named credential's token, got %q", doer.gotAuth)
	}
}

// TestRoleRefused proves a credential below the operation's required role is refused
// before any request, the refusal is a Forbidden fault, and the denial is audited
// with the caller's principal.
func TestRoleRefused(t *testing.T) {
	store := credStore(
		t,
		credential.Spec{Integration: "cf", Name: "ro", AuthType: "bearer", Role: credential.RoleRead, IsDefault: true},
	)
	doer := &fakeDoer{}
	var denials []Denial
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"cf/ro": "tok"}),
		WithCredentials(store),
		WithAudit(func(d Denial) { denials = append(denials, d) }),
	)
	// The operation requires the operator role.
	loader, _ := loadExtension(t, h,
		extension.AuthSpec{Type: "bearer", CredentialRef: "cf"},
		"https://api.example.com",
		`{"operations":[{"name":"deploy","role":"operator","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/x"}},{"op":"return","return":{"value":"ok"}}]}}]}`)

	ctx := capability.WithPrincipal(context.Background(), "alice")
	_, err := loader.Tools()[0].Invoke(ctx, nil)
	if err == nil {
		t.Fatal("expected a role refusal")
	}
	if fault.Classify(err) != fault.Forbidden {
		t.Fatalf("expected Forbidden, got %v (%q)", fault.Classify(err), err)
	}
	if doer.gotReq != nil {
		t.Fatal("no request should be dispatched on a refused role")
	}
	if len(denials) != 1 {
		t.Fatalf("expected one audited denial, got %d", len(denials))
	}
	d := denials[0]
	if d.Principal != "alice" || d.CredentialRole != credential.RoleRead || d.RequiredRole != credential.RoleOperator {
		t.Fatalf("denial mismatch: %+v", d)
	}
}

// TestNoAuthWithCredentialStore proves an integration that needs no credential (a
// public API authenticating with "none") works even when a credential store is
// configured: there is no reference to resolve, so the call is not blocked.
func TestNoAuthWithCredentialStore(t *testing.T) {
	doer := &fakeDoer{reply: func(_ *http.Request) (int, string) { return 200, `{"ok":true}` }}
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithCredentials(credStore(t)),
	)
	loader, _ := loadExtension(t, h, extension.AuthSpec{}, "https://api.example.com",
		`{"operations":[{"name":"get","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/x"}},{"op":"return","return":{"value":"{{steps.r.status}}"}}]}}]}`)
	if _, err := loader.Tools()[0].Invoke(context.Background(), nil); err != nil {
		t.Fatalf("a none-auth integration must work even with a credential store: %v", err)
	}
	if doer.gotReq == nil {
		t.Fatal("the request should have been dispatched")
	}
}

// TestRolePermitted proves a credential at or above the required role is allowed.
func TestRolePermitted(t *testing.T) {
	store := credStore(
		t,
		credential.Spec{Integration: "cf", Name: "admin", AuthType: "bearer", Role: credential.RoleAdmin, IsDefault: true},
	)
	doer := &fakeDoer{reply: func(_ *http.Request) (int, string) { return 200, `{}` }}
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"cf/admin": "tok"}),
		WithCredentials(store),
	)
	loader, _ := loadExtension(t, h,
		extension.AuthSpec{Type: "bearer", CredentialRef: "cf"},
		"https://api.example.com",
		`{"operations":[{"name":"deploy","role":"operator","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/x"}},{"op":"return","return":{"value":"ok"}}]}}]}`)

	if _, err := loader.Tools()[0].Invoke(context.Background(), nil); err != nil {
		t.Fatalf("an admin credential should satisfy an operator action: %v", err)
	}
	if doer.gotReq == nil {
		t.Fatal("the request should have been dispatched")
	}
}

// TestUnknownOperationRoleRejectedAtLoad proves an operation with an invalid role is
// rejected when the extension loads.
func TestUnknownOperationRoleRejectedAtLoad(t *testing.T) {
	reg := extension.NewRegistry()
	h := NewHandler(WithTransport(request.New(request.WithDoer(&fakeDoer{}))))
	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}
	loader := extension.NewLoader(reg)
	spec := extension.Spec{
		BaseURL: "https://x",
		Surfaces: map[string]json.RawMessage{extension.SurfaceIntegration: json.RawMessage(
			`{"operations":[{"name":"op","role":"root","flow":{"steps":[{"op":"return","return":{"value":"1"}}]}}]}`,
		)},
	}
	body, _ := spec.Encode()
	if _, err := loader.Load(context.Background(), resource.Resource{APIVersion: extension.GroupVersion, Kind: extension.Kind, ID: "e", Name: "n", Spec: body}); err == nil {
		t.Fatal("expected an unknown operation role to be rejected at load")
	}
}
