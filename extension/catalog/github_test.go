package catalog_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/integrations"
	"github.com/ionalpha/flynn/internal/integrations/request"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/secret"
)

// ghDoer is a fake GitHub: it records the request and returns a canned JSON body.
type ghDoer struct {
	gotURL    string
	gotAuth   string
	gotAccept string
}

func (d *ghDoer) Do(req *http.Request) (*http.Response, error) {
	d.gotURL = req.URL.String()
	d.gotAuth = req.Header.Get("Authorization")
	d.gotAccept = req.Header.Get("Accept")
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`[{"number":1,"title":"hello"}]`)),
	}, nil
}

type ghSecrets map[string]string

func (s ghSecrets) Lookup(_ context.Context, ref string) (secret.Text, error) {
	v, ok := s[ref]
	if !ok {
		return secret.Text{}, secret.ErrNotFound
	}
	return secret.New(v), nil
}

// loadGitHub loads the bundled GitHub spec through the integration handler with a
// credential store, returning the resulting tools by name.
func loadGitHub(t *testing.T, doer *ghDoer, role credential.Role) map[string]mission.Tool {
	t.Helper()
	ctx := context.Background()

	creg := resource.NewRegistry()
	if err := credential.RegisterKind(creg); err != nil {
		t.Fatal(err)
	}
	creds := credential.NewStore(resource.NewMemory(creg))
	if _, err := creds.Put(ctx, credential.Spec{Integration: "github", Name: "default", AuthType: "bearer", Role: role, IsDefault: true}); err != nil {
		t.Fatal(err)
	}

	h := integrations.NewHandler(
		integrations.WithTransport(request.New(request.WithDoer(doer))),
		integrations.WithSecrets(ghSecrets{"github/default": "ghp_token"}),
		integrations.WithCredentials(creds),
	)
	reg := extension.NewRegistry()
	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}
	loader := extension.NewLoader(reg)

	raw := githubSpec(t)
	if _, err := loader.Load(ctx, resource.Resource{
		APIVersion: extension.GroupVersion, Kind: extension.Kind, ID: "github", Name: "github", Spec: raw,
	}); err != nil {
		t.Fatalf("load github: %v", err)
	}
	out := map[string]mission.Tool{}
	for _, tool := range loader.Tools() {
		out[tool.Def().Name] = tool
	}
	return out
}

func githubSpec(t *testing.T) []byte {
	t.Helper()
	entries, err := catalog.Entries()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "github" {
			return e.Raw
		}
	}
	t.Fatal("github spec not found in the catalog")
	return nil
}

func TestGitHubListIssues(t *testing.T) {
	doer := &ghDoer{}
	tools := loadGitHub(t, doer, credential.RoleRead)
	tool, ok := tools["list_issues"]
	if !ok {
		t.Fatal("list_issues not surfaced")
	}
	if _, err := tool.Invoke(context.Background(), []byte(`{"owner":"ionalpha","repo":"flynn"}`)); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if doer.gotURL != "https://api.github.com/repos/ionalpha/flynn/issues?state=open" {
		t.Fatalf("url: %q", doer.gotURL)
	}
	if doer.gotAuth != "Bearer ghp_token" {
		t.Fatalf("auth: %q", doer.gotAuth)
	}
	if doer.gotAccept != "application/vnd.github+json" {
		t.Fatalf("accept header not sent: %q", doer.gotAccept)
	}
}

// TestGitHubCreateIssueRole proves create_issue requires an operator-or-higher
// credential: a read credential is refused, an operator credential is allowed.
func TestGitHubCreateIssueRole(t *testing.T) {
	readDoer := &ghDoer{}
	readTools := loadGitHub(t, readDoer, credential.RoleRead)
	_, err := readTools["create_issue"].Invoke(context.Background(), []byte(`{"owner":"o","repo":"r","title":"t"}`))
	if err == nil || fault.Classify(err) != fault.Forbidden {
		t.Fatalf("a read credential must be refused for create_issue, got %v", err)
	}
	if readDoer.gotURL != "" {
		t.Fatal("no request should be dispatched on a refused role")
	}

	opDoer := &ghDoer{}
	opTools := loadGitHub(t, opDoer, credential.RoleOperator)
	if _, err := opTools["create_issue"].Invoke(context.Background(), []byte(`{"owner":"o","repo":"r","title":"t","body":"b"}`)); err != nil {
		t.Fatalf("an operator credential should be allowed: %v", err)
	}
	if opDoer.gotURL != "https://api.github.com/repos/o/r/issues" {
		t.Fatalf("create_issue url: %q", opDoer.gotURL)
	}
}
