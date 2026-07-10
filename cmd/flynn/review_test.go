package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/archetypes/review"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/tools/github"
)

// fixedResolver resolves every host to the given addresses, so a test can state what a
// name answers with instead of asking the network.
func fixedResolver(addrs ...string) hostResolver {
	return func(context.Context, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

// TestConfigureAPIBaseLeavesTheDefaultAlone: the public API keeps the public-only
// policy the toolset builds for itself. Nothing is widened, and no name is resolved.
func TestConfigureAPIBaseLeavesTheDefaultAlone(t *testing.T) {
	unreachable := func(context.Context, string) ([]netip.Addr, error) {
		t.Fatal("the default API base must not be resolved")
		return nil, nil
	}
	for _, base := range []string{"", "  ", github.DefaultAPIBase, github.DefaultAPIBase + "/"} {
		var cfg github.Config
		if err := configureAPIBase(context.Background(), &cfg, base, unreachable); err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		if cfg.APIBase != "" || cfg.HTTPClient != nil {
			t.Errorf("base %q changed the config: APIBase=%q client=%v", base, cfg.APIBase, cfg.HTTPClient != nil)
		}
	}
}

// TestConfigureAPIBaseWidensEgressToTheNamedHostOnly: an enterprise appliance on a
// private address is reachable, and nothing else private becomes reachable with it.
// The metadata endpoint is the one that matters: a policy that admitted the whole
// private range would admit it too.
func TestConfigureAPIBaseWidensEgressToTheNamedHostOnly(t *testing.T) {
	var cfg github.Config
	err := configureAPIBase(context.Background(), &cfg,
		"https://ghe.internal.example/api/v3", fixedResolver("10.1.2.3"))
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if cfg.APIBase != "https://ghe.internal.example/api/v3" {
		t.Errorf("APIBase = %q", cfg.APIBase)
	}
	if cfg.HTTPClient == nil {
		t.Fatal("an explicit API base must install a client carrying the widened policy")
	}

	policy, err := apiEgressPolicy(context.Background(), "https://ghe.internal.example/api/v3", fixedResolver("10.1.2.3"))
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	allowed := map[string]bool{
		"10.1.2.3":        true,  // the appliance itself
		"10.1.2.4":        false, // its neighbour on the same private network
		"169.254.169.254": false, // cloud metadata
		"127.0.0.1":       false, // loopback
		"140.82.121.5":    false, // a public address: naming a host is not a licence to roam
	}
	for addr, want := range allowed {
		if got := policy.Allows(netip.MustParseAddr(addr)); got != want {
			t.Errorf("policy.Allows(%s) = %v, want %v", addr, got, want)
		}
	}
}

// TestAPIEgressPolicyRefusesPlaintextOffTheMachine: every request carries the
// reviewer's token, so http is permitted only where the request cannot leave the host.
func TestAPIEgressPolicyRefusesPlaintextOffTheMachine(t *testing.T) {
	if _, err := apiEgressPolicy(context.Background(), "http://127.0.0.1:8080", fixedResolver("127.0.0.1")); err != nil {
		t.Errorf("http to loopback must be allowed: %v", err)
	}
	if _, err := apiEgressPolicy(context.Background(), "http://ghe.internal.example", fixedResolver("10.1.2.3")); err == nil {
		t.Error("http to a private address must be refused: the token would cross the network in the clear")
	}
	if _, err := apiEgressPolicy(context.Background(), "https://ghe.internal.example", fixedResolver("10.1.2.3")); err != nil {
		t.Errorf("https to a private address must be allowed: %v", err)
	}
	// A host that answers on both loopback and something else is not a loopback host.
	if _, err := apiEgressPolicy(context.Background(), "http://mixed.example", fixedResolver("127.0.0.1", "10.1.2.3")); err == nil {
		t.Error("http must be refused when any resolved address leaves the machine")
	}
}

// TestAPIEgressPolicyRejectsUnusableBases: a base that names no reachable https host is
// an error at startup, not a confusing failure on the first request.
func TestAPIEgressPolicyRejectsUnusableBases(t *testing.T) {
	cases := map[string]struct {
		base    string
		resolve hostResolver
	}{
		"not a url":       {":// nope", fixedResolver("10.1.2.3")},
		"wrong scheme":    {"ftp://ghe.example", fixedResolver("10.1.2.3")},
		"file scheme":     {"file:///etc/passwd", fixedResolver("10.1.2.3")},
		"no host":         {"https://", fixedResolver("10.1.2.3")},
		"resolves to nil": {"https://ghe.example", fixedResolver()},
		"resolver fails": {"https://ghe.example", func(context.Context, string) ([]netip.Addr, error) {
			return nil, errors.New("no such host")
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := apiEgressPolicy(context.Background(), tc.base, tc.resolve); err == nil {
				t.Errorf("base %q was accepted", tc.base)
			}
		})
	}
}

func TestParsePRRef(t *testing.T) {
	cases := []struct {
		name, arg, repoFlag string
		owner, repo         string
		number              int
		wantErr             bool
	}{
		{name: "url", arg: "https://github.com/ionalpha/flynn/pull/342", owner: "ionalpha", repo: "flynn", number: 342},
		{name: "url trailing slash", arg: "https://github.com/o/r/pull/7/", owner: "o", repo: "r", number: 7},
		{name: "coords", arg: "ionalpha/flynn#12", owner: "ionalpha", repo: "flynn", number: 12},
		{name: "number with repo flag", arg: "9", repoFlag: "o/r", owner: "o", repo: "r", number: 9},
		{name: "number without repo", arg: "9", wantErr: true},
		{name: "url not a pull", arg: "https://github.com/o/r/issues/9", wantErr: true},
		{name: "url extra segments", arg: "https://github.com/o/r/pull/9/files", wantErr: true},
		{name: "coords not owner/name", arg: "flynn#12", wantErr: true},
		{name: "zero number", arg: "o/r#0", wantErr: true},
		{name: "negative number", arg: "o/r#-3", wantErr: true},
		{name: "non-numeric", arg: "o/r#abc", wantErr: true},
		{name: "too many slashes", arg: "a/b/c#1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, number, err := parsePRRef(tc.arg, tc.repoFlag)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePRRef(%q, %q) = %s/%s#%d, want error", tc.arg, tc.repoFlag, owner, repo, number)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePRRef(%q, %q): %v", tc.arg, tc.repoFlag, err)
			}
			if owner != tc.owner || repo != tc.repo || number != tc.number {
				t.Fatalf("parsePRRef(%q, %q) = %s/%s#%d, want %s/%s#%d",
					tc.arg, tc.repoFlag, owner, repo, number, tc.owner, tc.repo, tc.number)
			}
		})
	}
}

// testKeyPEM writes a freshly generated RSA key to a file in PEM form and returns
// the path. The PEM armor is produced at runtime by encoding/pem, so no key-like
// literal ever appears in the source tree.
func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	blob := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mapSource is a fake secret.Source: a map standing in for the vault-then-env
// chain, so the resolution order is tested without a keychain or a real
// environment.
type mapSource map[string]string

func (m mapSource) Lookup(_ context.Context, ref string) (secret.Text, error) {
	if v, ok := m[ref]; ok && v != "" {
		return secret.New(v), nil
	}
	return secret.Text{}, secret.ErrNotFound
}

// noStoredToken is the tokenLookup of a data dir with no github credential.
func noStoredToken(context.Context) (secret.Text, error) {
	return secret.Text{}, secret.ErrNotFound
}

func TestResolveReviewCredentials(t *testing.T) {
	ctx := context.Background()
	pemFile := testKeyPEM(t)
	pemBytes, err := os.ReadFile(pemFile)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("app from key content", func(t *testing.T) {
		src := mapSource{refAppIssuer: "Iv1.abc", refAppInstallation: "12345", refAppKey: string(pemBytes)}
		cfg, err := resolveReviewCredentials(ctx, src, noStoredToken, os.ReadFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.App.Issuer != "Iv1.abc" || cfg.App.InstallationID != 12345 || cfg.App.PrivateKey == nil {
			t.Fatalf("app config not captured: %+v", cfg.App)
		}
	})

	t.Run("app from key file", func(t *testing.T) {
		src := mapSource{refAppIssuer: "Iv1.abc", refAppInstallation: "12345", refAppKeyFile: pemFile}
		cfg, err := resolveReviewCredentials(ctx, src, noStoredToken, os.ReadFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.App.PrivateKey == nil {
			t.Fatal("key file was not read")
		}
	})

	t.Run("app beats stored token and env token", func(t *testing.T) {
		src := mapSource{
			refAppIssuer: "Iv1.abc", refAppInstallation: "12345", refAppKey: string(pemBytes),
			refToken: "ambient",
		}
		stored := func(context.Context) (secret.Text, error) { return secret.New("stored"), nil }
		cfg, err := resolveReviewCredentials(ctx, src, stored, os.ReadFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.App.PrivateKey == nil || !cfg.Token.Empty() {
			t.Fatal("a fully configured App must win over every token")
		}
	})

	t.Run("partial app refused not downgraded", func(t *testing.T) {
		src := mapSource{refAppIssuer: "Iv1.abc", refToken: "ambient"}
		if _, err := resolveReviewCredentials(ctx, src, noStoredToken, os.ReadFile); err == nil {
			t.Fatal("a partial App configuration must error, not fall through to the token")
		}
	})

	t.Run("stored credential beats env token", func(t *testing.T) {
		src := mapSource{refToken: "ambient"}
		stored := func(context.Context) (secret.Text, error) { return secret.New("from-vault"), nil }
		cfg, err := resolveReviewCredentials(ctx, src, stored, os.ReadFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Token.Expose() != "from-vault" {
			t.Fatalf("token = %s, want the stored credential", cfg.Token)
		}
	})

	t.Run("env token is the last resort", func(t *testing.T) {
		src := mapSource{refToken: "ambient"}
		cfg, err := resolveReviewCredentials(ctx, src, noStoredToken, os.ReadFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Token.Expose() != "ambient" {
			t.Fatal("ambient token not used")
		}
	})

	t.Run("nothing configured refused", func(t *testing.T) {
		if _, err := resolveReviewCredentials(ctx, mapSource{}, noStoredToken, os.ReadFile); err == nil {
			t.Fatal("no credentials anywhere, want error")
		}
	})

	t.Run("bad installation id refused", func(t *testing.T) {
		src := mapSource{refAppIssuer: "Iv1.abc", refAppInstallation: "not-a-number", refAppKey: string(pemBytes)}
		if _, err := resolveReviewCredentials(ctx, src, noStoredToken, os.ReadFile); err == nil {
			t.Fatal("non-numeric installation id, want error")
		}
	})

	t.Run("stored lookup failure surfaces", func(t *testing.T) {
		stored := func(context.Context) (secret.Text, error) { return secret.Text{}, errors.New("vault locked") }
		if _, err := resolveReviewCredentials(ctx, mapSource{refToken: "ambient"}, stored, os.ReadFile); err == nil {
			t.Fatal("a broken vault must surface, not be masked by the env fallback")
		}
	})
}

// TestStoredGitHubToken pins the two absence behaviours: an explicitly named
// credential that does not exist is a loud error (the operator asked for that
// one), while an absent default is ErrNotFound so resolution falls through to
// the environment.
func TestStoredGitHubToken(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	creds := credential.NewStore(store.Resources(reg))
	vaultStore := vault.New(t.TempDir())

	if _, err := storedGitHubToken(creds, vaultStore, "github/vouchbot")(ctx); err == nil || errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("a named credential that does not exist must be a loud error, got %v", err)
	}
	if _, err := storedGitHubToken(creds, vaultStore, "")(ctx); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("an absent default must be ErrNotFound so the env fallback applies, got %v", err)
	}
}

// event helpers for the verdict tracker tests.
func submitCall(id, verdict string) session.Event {
	return session.Event{
		Kind: session.KindToolCall, Tool: "github_submit_review", ToolUseID: id,
		Input: json.RawMessage(`{"event":"` + verdict + `","body":"b"}`),
	}
}

func toolResult(id string, isErr bool) session.Event {
	return session.Event{Kind: session.KindToolResult, ToolUseID: id, IsError: isErr}
}

func TestVerdictTracker(t *testing.T) {
	t.Run("refused submission does not count", func(t *testing.T) {
		v := newVerdictTracker()
		// An ungated APPROVE errors; the model falls back to COMMENT, which lands.
		v.observe(submitCall("s1", "APPROVE"))
		v.observe(toolResult("s1", true))
		v.observe(submitCall("s2", "COMMENT"))
		v.observe(toolResult("s2", false))
		if err := v.outcome(io.Discard); err != nil {
			t.Fatalf("clean COMMENT verdict, got %v", err)
		}
	})

	t.Run("changes requested maps to the sentinel", func(t *testing.T) {
		v := newVerdictTracker()
		v.observe(submitCall("s1", "REQUEST_CHANGES"))
		v.observe(toolResult("s1", false))
		if err := v.outcome(io.Discard); !errors.Is(err, errChangesRequested) {
			t.Fatalf("want errChangesRequested, got %v", err)
		}
	})

	t.Run("only a refused submission is no verdict", func(t *testing.T) {
		v := newVerdictTracker()
		v.observe(submitCall("s1", "APPROVE"))
		v.observe(toolResult("s1", true))
		if err := v.outcome(io.Discard); err == nil || errors.Is(err, errChangesRequested) {
			t.Fatalf("a refused submission must not count as a verdict, got %v", err)
		}
	})

	t.Run("no verdict is a failure", func(t *testing.T) {
		v := newVerdictTracker()
		if err := v.outcome(io.Discard); err == nil || errors.Is(err, errChangesRequested) {
			t.Fatalf("a run with no submitted review must fail, got %v", err)
		}
	})

	t.Run("unrelated tools are ignored", func(t *testing.T) {
		v := newVerdictTracker()
		v.observe(session.Event{Kind: session.KindToolCall, Tool: "github_comment", ToolUseID: "c1", Input: json.RawMessage(`{"body":"x"}`)})
		v.observe(toolResult("c1", false))
		if err := v.outcome(io.Discard); err == nil {
			t.Fatal("a comment is not a verdict")
		}
	})
}

// reviewScript is a deterministic model for the review e2e. Turn one it tries to
// run a shell command, which the reviewer's toolset does not offer and its grant
// does not name; the refusal must come back as an ordinary tool error. Turn two it
// fetches the pull request; turn three it submits REQUEST_CHANGES; then it stops.
type reviewScript struct {
	turn atomic.Int64
}

func (m *reviewScript) Generate(_ context.Context, _ llm.Request) (llm.Response, error) {
	call := func(id, name, input string) llm.Response {
		return llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
				Kind:    llm.KindToolUse,
				ToolUse: &llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)},
			}}},
			StopReason: llm.StopToolUse,
		}
	}
	switch m.turn.Add(1) {
	case 1:
		return call("t1", "exec", `{"command":"cat /etc/passwd"}`), nil
	case 2:
		return call("t2", "github_pr_fetch", `{"number":7}`), nil
	case 3:
		return call("t3", "github_submit_review", `{"number":7,"event":"REQUEST_CHANGES","body":"one blocking defect"}`), nil
	default:
		return llm.Response{Message: llm.Text(llm.RoleAssistant, "review submitted"), StopReason: llm.StopEndTurn}, nil
	}
}

// fakeGitHubAPI serves the minimal review surface: pull request metadata, its
// files and comment threads, and review submission.
func fakeGitHubAPI(t *testing.T, submitted *atomic.Value) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode fake response: %v", err)
		}
	}
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"number": 7, "title": "t", "body": "b", "state": "open",
			"user": map[string]any{"login": "author"}, "changed_files": 1,
			"additions": 1, "deletions": 0,
			"head": map[string]any{"sha": "abc123"}, "base": map[string]any{"ref": "main"},
		})
	})
	mux.HandleFunc("GET /repos/o/r/pulls/7/files", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"filename": "a.go", "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1 @@"}})
	})
	mux.HandleFunc("GET /repos/o/r/pulls/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{})
	})
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{})
	})
	mux.HandleFunc("POST /repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Event string `json:"event"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode review submission: %v", err)
		}
		submitted.Store(body.Event)
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]any{"id": 1})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestReviewRunSubmitsVerdictAndRefusesShell drives the reviewer end to end: the
// scripted model first asks for a shell, which the reviewer's authority does not
// include, then reviews and submits REQUEST_CHANGES against a fake GitHub API. It
// asserts the shell attempt failed as an ordinary tool error (the run survives),
// the verdict was submitted to the API, the tracker read the same verdict off the
// run's own events, and it maps to the changes-requested sentinel.
func TestReviewRunSubmitsVerdictAndRefusesShell(t *testing.T) {
	ctx := context.Background()
	var submitted atomic.Value
	srv := fakeGitHubAPI(t, &submitted)

	cfg := reviewTestConfig(srv)
	toolset, err := review.Tools(cfg)
	if err != nil {
		t.Fatal(err)
	}

	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}

	verdict := newVerdictTracker()
	// Record the shell attempt's fate off the run's own events: the exec call must
	// come back as an error result, never as an executed command.
	var execErrored, execSucceeded bool
	observe := func(ev session.Event) {
		verdict.observe(ev)
		if ev.Kind == session.KindToolResult && ev.ToolUseID == "t1" {
			if ev.IsError {
				execErrored = true
			} else {
				execSucceeded = true
			}
		}
	}
	var out bytes.Buffer
	result, _, _, err := drive(
		ctx, &out, &reviewScript{}, harness.Plan{}, "",
		"Review pull request #7.", review.SystemPrompt,
		store.Resources(reg), store.Jobs(), store.Log(), true, "", nil,
		withToolset(&boundToolset{tools: toolset, grant: review.Grant()}),
		withEventObserver(observe),
	)
	if err != nil {
		t.Fatalf("drive: %v\noutput:\n%s", err, out.String())
	}
	if result == "" {
		t.Fatal("run produced no result")
	}

	if got, _ := submitted.Load().(string); got != "REQUEST_CHANGES" {
		t.Fatalf("API received verdict %q, want REQUEST_CHANGES", got)
	}
	if err := verdict.outcome(io.Discard); !errors.Is(err, errChangesRequested) {
		t.Fatalf("tracker outcome = %v, want errChangesRequested", err)
	}
	// The shell attempt shows up on the run's events as a failed tool call, not as
	// a run failure and not as an executed command.
	if execSucceeded {
		t.Fatal("the reviewer executed a shell command; its authority must not include one")
	}
	if !execErrored {
		t.Fatalf("expected the shell attempt to come back as an error result\noutput:\n%s", out.String())
	}
}

// reviewTestConfig is the repository-bound toolset config every review e2e uses:
// token auth against the fake server, approval not enabled.
func reviewTestConfig(srv *httptest.Server) (cfg github.Config) {
	cfg.Token = secret.New("test-token")
	cfg.Owner, cfg.Repo = "o", "r"
	cfg.APIBase = srv.URL
	cfg.HTTPClient = srv.Client()
	return cfg
}
