package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
)

// postRPC sends one JSON-RPC message to the handler and returns the HTTP status,
// the session header, and the decoded response (zero response for a 202).
func postRPC(t *testing.T, h http.Handler, token, body string) (int, string, rawResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r rawResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("decode response: %v (body %q)", err, rec.Body.String())
		}
	}
	return rec.Code, rec.Header().Get(mcpSessionHeader), r
}

func TestHTTPInitializeAndCall(t *testing.T) {
	srv := NewServer(nil, []mission.Tool{echo("echo")})
	h := srv.HTTPHandler(context.Background(), "")

	code, session, resp := postRPC(t, h, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if code != http.StatusOK {
		t.Fatalf("initialize status %d", code)
	}
	if session == "" {
		t.Errorf("no session id assigned")
	}
	var init initializeResult
	_ = json.Unmarshal(resp.Result, &init)
	if init.Capabilities.Tools == nil {
		t.Errorf("tools capability missing")
	}

	code, _, resp = postRPC(t, h, "",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"k":"v"}}}`)
	if code != http.StatusOK {
		t.Fatalf("call status %d", code)
	}
	var res callResult
	_ = json.Unmarshal(resp.Result, &res)
	if res.IsError || res.Content[0].Text != `{"k":"v"}` {
		t.Errorf("unexpected call result: %+v", res)
	}
}

func TestHTTPNotificationReturns202(t *testing.T) {
	srv := NewServer(nil, nil)
	h := srv.HTTPHandler(context.Background(), "")
	code, _, _ := postRPC(t, h, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted {
		t.Fatalf("notification status %d, want 202", code)
	}
}

func TestHTTPBearerTokenEnforced(t *testing.T) {
	srv := NewServer(nil, nil)
	h := srv.HTTPHandler(context.Background(), "sekret")

	// No token: rejected.
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status %d, want 401", rec.Code)
	}

	// Correct token: served.
	code, _, _ := postRPC(t, h, "sekret", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if code != http.StatusOK {
		t.Fatalf("authorized status %d", code)
	}
}

func TestHTTPParseErrorIs400(t *testing.T) {
	srv := NewServer(nil, nil)
	h := srv.HTTPHandler(context.Background(), "")
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("parse error status %d, want 400", rec.Code)
	}
	var r response
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if r.Error == nil || r.Error.Code != codeParse {
		t.Errorf("want parse error object, got %+v", r)
	}
}

func TestHTTPGetIsMethodNotAllowed(t *testing.T) {
	srv := NewServer(nil, nil)
	h := srv.HTTPHandler(context.Background(), "")
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d, want 405", rec.Code)
	}
}

// TestHTTPCallGovernedByBaseContext proves the HTTP transport routes through the
// same waist as stdio: the grant bound on the base context is enforced, and a
// denial is an error result recorded on the spine.
func TestHTTPCallGovernedByBaseContext(t *testing.T) {
	sink := &dispatch.MemorySink{}
	d := dispatch.New(dispatch.WithAdmitter(capability.Admitter{}), dispatch.WithEventSink(sink))
	srv := NewServer(d, []mission.Tool{echo("secret")})

	base := capability.Into(context.Background(), capability.NewGrant("other"))
	h := srv.HTTPHandler(base, "")

	_, _, resp := postRPC(t, h, "",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"secret","arguments":{}}}`)
	var res callResult
	_ = json.Unmarshal(resp.Result, &res)
	if !res.IsError {
		t.Fatalf("ungranted call should be denied: %+v", res)
	}
	var rejected bool
	for _, e := range sink.Events() {
		if e.Type == dispatch.EventRejected && e.Action == "secret" {
			rejected = true
		}
	}
	if !rejected {
		t.Errorf("denial not recorded on the spine")
	}
}

// blockingTool blocks in Invoke until its context is cancelled or it is released,
// signalling entry so a test can cancel the base context and observe propagation.
type blockingTool struct {
	name    string
	entered chan struct{}
	release chan struct{}
}

func (b blockingTool) Def() llm.Tool { return llm.Tool{Name: b.name} }

func (b blockingTool) Invoke(ctx context.Context, _ json.RawMessage) (string, error) {
	close(b.entered)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-b.release:
		return "done", nil
	}
}

// TestHTTPBaseContextCancelPropagates proves a run-level halt (base context
// cancelled) aborts an in-flight call served over HTTP.
func TestHTTPBaseContextCancelPropagates(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := NewServer(nil, []mission.Tool{blockingTool{name: "block", entered: entered, release: release}})

	base, cancel := context.WithCancel(context.Background())
	h := srv.HTTPHandler(base, "")

	done := make(chan callResult, 1)
	go func() {
		_, _, r := postRPC(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"block","arguments":{}}}`)
		var res callResult
		_ = json.Unmarshal(r.Result, &res)
		done <- res
	}()

	<-entered
	cancel() // run-level halt
	select {
	case res := <-done:
		if !res.IsError {
			t.Errorf("cancelled call should error: %+v", res)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatalf("tool was not cancelled by base context")
	}
}
