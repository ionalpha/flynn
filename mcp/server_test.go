package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// fakeTool is a mission.Tool for tests: a name, a schema, and an Invoke body.
type fakeTool struct {
	name   string
	schema json.RawMessage
	fn     func(json.RawMessage) (string, error)
}

func (f fakeTool) Def() llm.Tool {
	return llm.Tool{Name: f.name, Description: "does " + f.name, InputSchema: f.schema}
}

func (f fakeTool) Invoke(_ context.Context, in json.RawMessage) (string, error) {
	return f.fn(in)
}

// trustedTool is a fakeTool that declares a work-trust level, so it is gated by the
// containment gate like a shell tool.
type trustedTool struct {
	fakeTool
	trust sandbox.Trust
}

func (t trustedTool) WorkTrust() sandbox.Trust { return t.trust }

func echo(name string) fakeTool {
	return fakeTool{name: name, fn: func(in json.RawMessage) (string, error) { return string(in), nil }}
}

// rawResp is the wire response decoded back for assertions.
type rawResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// drive runs the server over the given newline-joined requests and returns the
// decoded responses in order.
func drive(ctx context.Context, t *testing.T, srv *Server, reqs ...string) []rawResp {
	t.Helper()
	in := strings.NewReader(strings.Join(reqs, "\n") + "\n")
	var out strings.Builder
	if err := srv.Serve(ctx, in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resps []rawResp
	dec := json.NewDecoder(strings.NewReader(out.String()))
	for dec.More() {
		var r rawResp
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode response: %v (buffer: %q)", err, out.String())
		}
		resps = append(resps, r)
	}
	return resps
}

func TestInitializeNegotiatesVersionAndIdentity(t *testing.T) {
	srv := NewServer(nil, nil, WithInfo(Info{Name: "flynn-test", Version: "1.2.3"}))
	resps := drive(context.Background(), t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	var res initializeResult
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocol version not echoed: got %q", res.ProtocolVersion)
	}
	if res.ServerInfo.Name != "flynn-test" || res.ServerInfo.Version != "1.2.3" {
		t.Errorf("server info wrong: %+v", res.ServerInfo)
	}
	if res.Capabilities.Tools == nil {
		t.Errorf("tools capability not advertised")
	}
}

func TestInitializeDefaultsVersionWhenClientSendsNone(t *testing.T) {
	srv := NewServer(nil, nil)
	resps := drive(context.Background(), t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	var res initializeResult
	_ = json.Unmarshal(resps[0].Result, &res)
	if res.ProtocolVersion != protocolVersion {
		t.Errorf("want default %q, got %q", protocolVersion, res.ProtocolVersion)
	}
}

func TestToolsListReportsDefsInOrderWithSchemaFallback(t *testing.T) {
	withSchema := fakeTool{
		name: "b", schema: json.RawMessage(`{"type":"object","properties":{"x":{}}}`),
		fn: func(json.RawMessage) (string, error) { return "", nil },
	}
	srv := NewServer(nil, []mission.Tool{echo("a"), withSchema})
	resps := drive(context.Background(), t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var res toolsListResult
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(res.Tools))
	}
	if res.Tools[0].Name != "a" || res.Tools[1].Name != "b" {
		t.Errorf("registration order not preserved: %q, %q", res.Tools[0].Name, res.Tools[1].Name)
	}
	// The schemaless tool gets an object schema, never null.
	if string(res.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Errorf("schema fallback wrong: %s", res.Tools[0].InputSchema)
	}
	if !strings.Contains(string(res.Tools[1].InputSchema), `"properties"`) {
		t.Errorf("declared schema dropped: %s", res.Tools[1].InputSchema)
	}
}

func TestToolsCallForwardsArgumentsAndReturnsText(t *testing.T) {
	srv := NewServer(nil, []mission.Tool{echo("echo")})
	resps := drive(context.Background(), t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"hello":"world"}}}`)
	var res callResult
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("want one text block, got %+v", res.Content)
	}
	if res.Content[0].Text != `{"hello":"world"}` {
		t.Errorf("arguments not forwarded verbatim: %q", res.Content[0].Text)
	}
}

func TestToolsCallAbsentArgumentsBecomesEmptyObject(t *testing.T) {
	srv := NewServer(nil, []mission.Tool{echo("echo")})
	resps := drive(context.Background(), t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`)
	var res callResult
	_ = json.Unmarshal(resps[0].Result, &res)
	if res.Content[0].Text != `{}` {
		t.Errorf("want empty object, got %q", res.Content[0].Text)
	}
}

func TestUnknownToolIsErrorResultNotProtocolError(t *testing.T) {
	srv := NewServer(nil, []mission.Tool{echo("echo")})
	resps := drive(context.Background(), t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`)
	if resps[0].Error != nil {
		t.Fatalf("unknown tool should not be a protocol error: %+v", resps[0].Error)
	}
	var res callResult
	_ = json.Unmarshal(resps[0].Result, &res)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "unknown tool") {
		t.Errorf("want isError unknown-tool result, got %+v", res)
	}
}

// TestCapabilityDenialSurfacesAsErrorAndRecordsRejection is the core routing
// property: a tool the grant does not permit is refused at the waist, the client
// sees an ordinary error result (so it can adapt), and the denial lands on the
// spine as a rejected action.
func TestCapabilityDenialSurfacesAsErrorAndRecordsRejection(t *testing.T) {
	sink := &dispatch.MemorySink{}
	d := dispatch.New(dispatch.WithAdmitter(capability.Admitter{}), dispatch.WithEventSink(sink))
	srv := NewServer(d, []mission.Tool{echo("allowed"), echo("secret")})

	// Grant permits only "allowed"; the run's grant rides on the context.
	ctx := capability.Into(context.Background(), capability.NewGrant("allowed"))

	resps := drive(ctx, t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"secret","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"allowed","arguments":{}}}`)

	var denied, ok callResult
	_ = json.Unmarshal(resps[0].Result, &denied)
	_ = json.Unmarshal(resps[1].Result, &ok)
	if !denied.IsError {
		t.Errorf("denied call should be an error result: %+v", denied)
	}
	if ok.IsError {
		t.Errorf("granted call should succeed: %+v", ok)
	}

	// The waist recorded a rejection for the denied call and a start/end for the
	// permitted one.
	var rejected, started bool
	for _, e := range sink.Events() {
		switch e.Type {
		case dispatch.EventRejected:
			if e.Action == "secret" {
				rejected = true
			}
		case dispatch.EventStart:
			if e.Action == "allowed" {
				started = true
			}
		}
	}
	if !rejected {
		t.Errorf("denied call not recorded as rejected on the spine")
	}
	if !started {
		t.Errorf("permitted call not recorded as started on the spine")
	}
}

// nopSandbox is a Sandbox that declares no containment, so it reports the weakest
// level (process jail). It never runs anything; the containment gate only reads its
// level.
type nopSandbox struct{}

func (nopSandbox) Exec(context.Context, sandbox.Command) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (nopSandbox) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (nopSandbox) WriteFile(context.Context, string, []byte) error  { return nil }
func (nopSandbox) Glob(context.Context, string) ([]string, error)   { return nil, nil }
func (nopSandbox) Walk(context.Context, string) ([]string, error)   { return nil, nil }
func (nopSandbox) Close() error                                     { return nil }

// TestContainmentGateRefusesUndercontainedWork proves a bridged tool is gated at
// the same containment level a native call would be: a semi-trusted tool on a host
// that offers only a process jail is refused, and the client sees an error result.
func TestContainmentGateRefusesUndercontainedWork(t *testing.T) {
	d := dispatch.New(dispatch.WithHook(capability.NewContainmentGate(nopSandbox{})))
	semi := trustedTool{fakeTool: echo("shell"), trust: sandbox.TrustSemi}
	srv := NewServer(d, []mission.Tool{semi})

	resps := drive(context.Background(), t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell","arguments":{}}}`)
	var res callResult
	_ = json.Unmarshal(resps[0].Result, &res)
	if !res.IsError {
		t.Fatalf("undercontained work should be refused: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "containment") {
		t.Errorf("want containment refusal, got %q", res.Content[0].Text)
	}
}

// TestTrustAndScopeStampedOnAction verifies the governed action carries the tool's
// declared trust and the session scope, so a bridged action is attributed on the
// spine exactly like a native one.
func TestTrustAndScopeStampedOnAction(t *testing.T) {
	sink := &dispatch.MemorySink{}
	d := dispatch.New(dispatch.WithEventSink(sink))
	semi := trustedTool{fakeTool: echo("shell"), trust: sandbox.TrustSemi}
	scope := state.Scope{Instance: "i1", Project: "p1"}
	srv := NewServer(d, []mission.Tool{semi}, WithScope(scope), WithGoal("g1"))

	drive(context.Background(), t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell","arguments":{}}}`)

	var found bool
	for _, e := range sink.Events() {
		if e.Type == dispatch.EventStart && e.Action == "shell" {
			found = true
			if e.Trust != sandbox.TrustSemi.String() {
				t.Errorf("trust not stamped: got %v", e.Trust)
			}
			if e.Scope != scope {
				t.Errorf("scope not stamped: got %+v", e.Scope)
			}
			if e.Goal != "g1" {
				t.Errorf("goal not stamped: got %q", e.Goal)
			}
		}
	}
	if !found {
		t.Errorf("no start event recorded for the call")
	}
}

func TestNotificationGetsNoReply(t *testing.T) {
	srv := NewServer(nil, nil)
	resps := drive(context.Background(), t, srv,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if len(resps) != 1 {
		t.Fatalf("want only the ping reply, got %d responses", len(resps))
	}
	var id int
	_ = json.Unmarshal(resps[0].ID, &id)
	if id != 1 {
		t.Errorf("want ping reply id 1, got %d", id)
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	srv := NewServer(nil, nil)
	resps := drive(context.Background(), t, srv,
		`{"jsonrpc":"2.0","id":9,"method":"resources/list"}`)
	if resps[0].Error == nil || resps[0].Error.Code != codeMethodNotFound {
		t.Fatalf("want method-not-found, got %+v", resps[0])
	}
}

func TestPresenceIsNotPermission(t *testing.T) {
	// A tool the grant denies still appears in tools/list; authority is decided at
	// call time, not by listing.
	d := dispatch.New(dispatch.WithAdmitter(capability.Admitter{}))
	srv := NewServer(d, []mission.Tool{echo("secret")})
	ctx := capability.Into(context.Background(), capability.NewGrant("other"))
	resps := drive(ctx, t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var res toolsListResult
	_ = json.Unmarshal(resps[0].Result, &res)
	if len(res.Tools) != 1 || res.Tools[0].Name != "secret" {
		t.Errorf("denied tool should still be listed: %+v", res.Tools)
	}
}
