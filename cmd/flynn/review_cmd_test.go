package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// openAITurn is one scripted answer from the model: either a tool call or the final
// text. The reviewer's loop is driven over the real OpenAI-compatible client, so the
// command is exercised through the same provider path the binary uses.
type openAITurn struct {
	toolID string
	tool   string
	args   string // JSON-encoded tool arguments
	text   string // when tool is empty, the assistant's final message
}

// openAIStub serves the chat-completions endpoint the openai provider posts to, handing
// back the scripted turns in order. It never reaches the network: the client is pointed
// at it through OPENAI_BASE_URL, which is loopback.
func openAIStub(t *testing.T, turns ...openAITurn) *httptest.Server {
	t.Helper()
	var (
		mu   sync.Mutex
		next int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		turn := openAITurn{text: "review complete"}
		if next < len(turns) {
			turn = turns[next]
		}
		next++
		mu.Unlock()

		msg := map[string]any{"content": turn.text}
		finish := "stop"
		if turn.tool != "" {
			msg = map[string]any{"content": "", "tool_calls": []map[string]any{{
				"id": turn.toolID, "type": "function",
				"function": map[string]any{"name": turn.tool, "arguments": turn.args},
			}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		}); err != nil {
			t.Errorf("encode model response: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestReviewCommandDrivesThePullRequestToAVerdict is the command end to end, with no
// network: the model is an OpenAI-compatible server on loopback, the GitHub API is a
// fake on loopback, and the credentials come from the environment through the same
// vault-then-environment chain the binary uses. The reviewer fetches the pull request,
// submits REQUEST_CHANGES, and the command maps that verdict to its own sentinel, which
// is what gives the blocking verdict its distinct exit code.
func TestReviewCommandDrivesThePullRequestToAVerdict(t *testing.T) {
	dataDir := fileVaultEnv(t)
	t.Setenv("GITHUB_TOKEN", "test-token")

	var submitted atomic.Value
	gh := fakeGitHubAPI(t, &submitted)
	model := openAIStub(
		t,
		openAITurn{toolID: "t1", tool: "github_pr_fetch", args: `{}`},
		openAITurn{toolID: "t2", tool: "github_submit_review", args: `{"event":"REQUEST_CHANGES","conclusion":"One blocking defect."}`},
		openAITurn{text: "review submitted"},
	)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", model.URL+"/v1")

	err := runReview([]string{"o/r#7", "--api-base", gh.URL}, "openai:gpt-test", dataDir, false)
	if !errors.Is(err, errChangesRequested) {
		t.Fatalf("review = %v, want errChangesRequested", err)
	}
	if got, _ := submitted.Load().(string); got != "REQUEST_CHANGES" {
		t.Fatalf("the API received verdict %q, want REQUEST_CHANGES", got)
	}
}

// TestReviewCommandFailsWhenNoVerdictWasSubmitted: a reviewer that talked and stopped
// without submitting anything must not read as a pass. The run itself succeeded, so
// only the missing verdict can fail the command.
func TestReviewCommandFailsWhenNoVerdictWasSubmitted(t *testing.T) {
	dataDir := fileVaultEnv(t)
	t.Setenv("GITHUB_TOKEN", "test-token")

	var submitted atomic.Value
	gh := fakeGitHubAPI(t, &submitted)
	model := openAIStub(
		t,
		openAITurn{toolID: "t1", tool: "github_pr_fetch", args: `{}`},
		openAITurn{text: "looks fine to me"},
	)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", model.URL+"/v1")

	err := runReview([]string{"https://github.com/o/r/pull/7", "--api-base", gh.URL}, "openai:gpt-test", dataDir, true)
	if err == nil || errors.Is(err, errChangesRequested) {
		t.Fatalf("review = %v, want a failure for submitting no verdict", err)
	}
	if !strings.Contains(err.Error(), "no review verdict") {
		t.Fatalf("the failure must name what is missing, got %q", err)
	}
	if v := submitted.Load(); v != nil {
		t.Fatalf("a review was submitted after all: %v", v)
	}
}

// TestReviewRefusesBeforeItSpendsAnything: every invocation the command cannot honour
// is refused up front, before a model is resolved, a store is opened, or a credential
// is read.
func TestReviewRefusesBeforeItSpendsAnything(t *testing.T) {
	cases := map[string]struct {
		args      []string
		modelSpec string
		wantIn    string
	}{
		"no pull request":       {args: nil, modelSpec: "openai:gpt-test", wantIn: "exactly one pull request"},
		"two pull requests":     {args: []string{"o/r#7", "o/r#8"}, modelSpec: "openai:gpt-test", wantIn: "exactly one pull request"},
		"unknown flag":          {args: []string{"--nope"}, modelSpec: "openai:gpt-test", wantIn: "nope"},
		"unparseable reference": {args: []string{"not-a-pull-request"}, modelSpec: "openai:gpt-test", wantIn: "no repository"},
		"approve without self":  {args: []string{"o/r#7", "--approve"}, modelSpec: "openai:gpt-test", wantIn: "--as"},
		"external harness":      {args: []string{"o/r#7"}, modelSpec: "codex:gpt-5-codex", wantIn: "native loop"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// A data dir that is never written to: a refusal must come before any of it.
			err := runReview(tc.args, tc.modelSpec, t.TempDir(), false)
			if err == nil {
				t.Fatalf("runReview(%v) was accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error %q does not name %q", err, tc.wantIn)
			}
		})
	}
}

// TestReviewRefusesAnUnusableAPIBase: a base the credential cannot safely travel to is
// refused at startup rather than on the first request. The reviewer's token rides every
// call, so plaintext off the machine is not a preference.
func TestReviewRefusesAnUnusableAPIBase(t *testing.T) {
	dataDir := fileVaultEnv(t)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("OPENAI_API_KEY", "test-key")
	model := openAIStub(t)
	t.Setenv("OPENAI_BASE_URL", model.URL+"/v1")

	err := runReview([]string{"o/r#7", "--api-base", "ftp://ghe.example"}, "openai:gpt-test", dataDir, false)
	if err == nil {
		t.Fatal("an API base that is not http(s) was accepted")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("error = %q", err)
	}
}

// TestReviewRefusesWithoutACredential: with no App, no stored credential, and no token
// in the environment, the command says what to configure instead of reviewing
// anonymously.
func TestReviewRefusesWithoutACredential(t *testing.T) {
	dataDir := fileVaultEnv(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	model := openAIStub(t)
	t.Setenv("OPENAI_BASE_URL", model.URL+"/v1")

	err := runReview([]string{"o/r#7"}, "openai:gpt-test", dataDir, false)
	if err == nil || !strings.Contains(err.Error(), "no GitHub credentials") {
		t.Fatalf("review without a credential = %v, want a refusal naming what to configure", err)
	}
}

// TestResolveHostPassesAddressLiteralsThrough: an address literal is used as written,
// so a policy built for it cannot be widened by a resolver, and a name resolves to what
// the machine says it is.
func TestResolveHostPassesAddressLiteralsThrough(t *testing.T) {
	ctx := context.Background()
	addrs, err := resolveHost(ctx, "10.1.2.3")
	if err != nil {
		t.Fatalf("resolveHost: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != netip.MustParseAddr("10.1.2.3") {
		t.Fatalf("an address literal resolved to %v", addrs)
	}

	// localhost is answered from the machine's own configuration, not the network, and
	// every address it names must be loopback.
	local, err := resolveHost(ctx, "localhost")
	if err != nil {
		t.Fatalf("resolveHost(localhost): %v", err)
	}
	if len(local) == 0 {
		t.Fatal("localhost resolved to nothing")
	}
	for _, a := range local {
		if !a.Unmap().IsLoopback() {
			t.Fatalf("localhost resolved to %s, which is not loopback", a)
		}
	}
}
