package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpenAI is a scriptable, in-process OpenAI Chat Completions server. It lets the
// e2e suite drive the real flynn binary against a deterministic model without a live
// API key or network: the binary is run with OPENAI_BASE_URL pointed at this server
// (a loopback address the provider adapter dials with a plain client) and any dummy
// key. Each POST to /chat/completions is answered by the responder, and every decoded
// request is recorded so a test can assert what the binary sent (tools advertised,
// system prompt, injected content).
//
// The server binds an ephemeral port via httptest, so parallel tests never collide on
// a fixed port. It speaks the exact wire shape the openai adapter expects: a single
// JSON response (no streaming), choices[0].message.{content,tool_calls} with a
// finish_reason, and a usage block.
type fakeOpenAI struct {
	srv *httptest.Server

	mu        sync.Mutex
	requests  []oaiRequest
	plans     []oaiRequest // planning-phase requests, answered automatically (see autoPlanOff)
	responder func(req oaiRequest, n int) oaiReply

	// autoPlanOff disables the transparent planning-phase responder. The goal command
	// plans before it builds, so by default the server answers a planning request (the
	// one carrying the planner's standing prompt) with a canned one-item plan and does
	// NOT record it, count it, or offer it to block: a test scripts only the build turns,
	// and count()/request()/blockAt address those exactly as they did before planning
	// existed. A test that wants to script or block the plan turn itself sets this true.
	autoPlanOff bool

	// plan overrides the canned answer to a planning request, so a scenario can hand the
	// run a plan whose check fails without taking over the build turns as well.
	plan *oaiReply

	// block, when set, makes the handler for a request whose zero-based index it
	// selects hang until the server is torn down, so a test can catch the binary
	// mid-run (the request is recorded first, so count() still advances) and kill it.
	block   func(n int) bool
	closing chan struct{}
}

// oaiReply is one scripted model turn. When ToolCalls is non-empty the reply is a
// tool-use turn (finish_reason "tool_calls"); otherwise Text is the assistant's answer
// and the turn ends. A non-zero Status makes the server answer with that HTTP status
// and an OpenAI-shaped error body instead of a completion, which drives the typed
// provider-failure scenarios (a 401 or an insufficient_quota 400 must fail fast, a 5xx
// is transient).
type oaiReply struct {
	Text      string
	ToolCalls []oaiToolCall
	Status    int
	ErrType   string
	ErrCode   string
	ErrMsg    string
	Usage     *oaiUsage
}

type oaiToolCall struct {
	ID   string
	Name string
	Args string // a JSON-encoded arguments string
}

type oaiUsage struct {
	Prompt     int
	Completion int
}

// oaiRequest is the decoded subset of a Chat Completions request the suite asserts on.
type oaiRequest struct {
	Model    string
	System   string
	Messages []oaiMessage
	Tools    []string // advertised tool names, in order
	Grammar  string   // set when the request carries a decode-time grammar (local path)
	Raw      map[string]any
}

type oaiMessage struct {
	Role       string
	Content    string
	ToolCallID string
}

// newFakeOpenAI starts a server whose every turn returns reply. Use newFakeOpenAIFunc
// for multi-turn or content-dependent scripting.
func newFakeOpenAI(t *testing.T, reply oaiReply) *fakeOpenAI {
	return newFakeOpenAIFunc(t, func(oaiRequest, int) oaiReply { return reply })
}

// newFakeOpenAIQueue answers the i-th request with replies[i], and the last reply for
// any request past the end, so a fixed conversation can be scripted turn by turn.
func newFakeOpenAIQueue(t *testing.T, replies ...oaiReply) *fakeOpenAI {
	if len(replies) == 0 {
		t.Fatal("newFakeOpenAIQueue: need at least one reply")
	}
	return newFakeOpenAIFunc(t, func(_ oaiRequest, n int) oaiReply {
		if n < len(replies) {
			return replies[n]
		}
		return replies[len(replies)-1]
	})
}

// newFakeOpenAIFunc starts a server that computes each reply from the request and its
// zero-based turn index. The server is torn down at test end.
func newFakeOpenAIFunc(t *testing.T, responder func(req oaiRequest, n int) oaiReply) *fakeOpenAI {
	f := &fakeOpenAI{responder: responder, closing: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", f.handle)
	f.srv = httptest.NewServer(mux)
	// Cleanups run last-in-first-out: unblock any hung handler before Close waits on it.
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.closing) })
	return f
}

// blockAt makes the server hang (until teardown) when the request at the given
// zero-based index arrives, after recording it. It is used to freeze the binary mid-run
// so a test can kill it at a known point. Returns the server for chaining.
func (f *fakeOpenAI) blockAt(idx int) *fakeOpenAI {
	f.mu.Lock()
	f.block = func(n int) bool { return n == idx }
	f.mu.Unlock()
	return f
}

// baseURL is the value to pass as OPENAI_BASE_URL. The path the adapter appends is
// /chat/completions, so the server's root is the base with no version segment.
func (f *fakeOpenAI) baseURL() string { return f.srv.URL }

func (f *fakeOpenAI) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := decodeRequest(body)

	f.mu.Lock()
	autoPlan := !f.autoPlanOff && isPlanRequest(req)
	if autoPlan {
		// A planning turn: answer it ourselves and leave the build-turn bookkeeping
		// (requests, count, block indices) untouched, so the test's scripting still
		// lines up one-to-one with the build turns.
		f.plans = append(f.plans, req)
		f.mu.Unlock()
		writeCompletion(w, f.planReply())
		return
	}
	n := len(f.requests)
	f.requests = append(f.requests, req)
	responder := f.responder
	block := f.block
	f.mu.Unlock()

	if block != nil && block(n) {
		<-f.closing // hang until the server is torn down; the test kills the binary meanwhile
		return
	}

	reply := responder(req, n)
	if reply.Status != 0 {
		writeError(w, reply)
		return
	}
	writeCompletion(w, reply)
}

// isPlanRequest reports whether a request is the planning phase's call, identified by
// the planner's standing prompt (mission.planSystem). The goal command plans before it
// builds; the fake answers that turn itself so a test scripts only the build turns.
func isPlanRequest(req oaiRequest) bool {
	return strings.Contains(req.System, "You are the planning phase")
}

// cannedPlan is the fake's automatic answer to a planning request: a single ledger item
// carrying a verify clause (the ledger refuses an item without one). Its content does not
// steer the build — convergence is still the model's to declare on a build turn — so one
// generic item is enough to satisfy the planning gate and let the scripted build turns run.
//
// The clause is a real command that exits 0, not prose, because the run now executes it:
// the item's check is what settles it, and a scripted suite whose plans were unrunnable
// would exercise only the degraded path.
var cannedPlan = oaiReply{Text: `[{"item":"accomplish the objective","verify":"` + passingCheck + `"}]`}

// failingPlan answers a planning request with an item whose check runs and fails, which is
// how a scenario drives the case where a run's own plan contradicts its claim of success.
var failingPlan = oaiReply{Text: `[{"item":"accomplish the objective","verify":"` + failingCheck + `"}]`}

// passingCheck and failingCheck are shell one-liners that work on every platform the suite
// runs on, so an item's verdict comes from a real execution rather than from a stub.
const (
	passingCheck = "cd ."
	failingCheck = "cd this-directory-does-not-exist"
)

// planWith replaces the canned plan for this fake, so a scenario chooses what the run's
// ledger commits to while still scripting only the build turns.
func (f *fakeOpenAI) planWith(reply oaiReply) *fakeOpenAI {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plan = &reply
	return f
}

// planReply is the answer to a planning request: the scenario's own plan when it set one,
// else the canned single-item plan.
func (f *fakeOpenAI) planReply() oaiReply {
	if f.plan != nil {
		return *f.plan
	}
	return cannedPlan
}

// count returns how many requests the binary has made so far.
func (f *fakeOpenAI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// waitForCount blocks until the binary has made at least n requests, or fails the test
// after timeout. It is how a crash scenario waits for the run to reach the point just
// before the blocked request.
func (f *fakeOpenAI) waitForCount(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	const interval = 20 * time.Millisecond
	iterations := int(timeout/interval) + 1
	for range iterations {
		if f.count() >= n {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("timed out waiting for %d model calls; saw %d", n, f.count())
}

// request returns the i-th recorded request (0-based). It fails the test if there is no
// such request, so an assertion about "what the binary sent" cannot silently pass on an
// absent call.
func (f *fakeOpenAI) request(t *testing.T, i int) oaiRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.requests) {
		t.Fatalf("no request %d; the binary made %d request(s)", i, len(f.requests))
	}
	return f.requests[i]
}

func decodeRequest(body []byte) oaiRequest {
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	req := oaiRequest{Raw: raw}
	if m, ok := raw["model"].(string); ok {
		req.Model = m
	}
	if g, ok := raw["grammar"].(string); ok {
		req.Grammar = g
	}
	if tools, ok := raw["tools"].([]any); ok {
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			fn, _ := tm["function"].(map[string]any)
			if name, ok := fn["name"].(string); ok {
				req.Tools = append(req.Tools, name)
			}
		}
	}
	if msgs, ok := raw["messages"].([]any); ok {
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			role, _ := mm["role"].(string)
			content := stringContent(mm["content"])
			id, _ := mm["tool_call_id"].(string)
			if role == "system" && req.System == "" {
				req.System = content
			}
			req.Messages = append(req.Messages, oaiMessage{Role: role, Content: content, ToolCallID: id})
		}
	}
	return req
}

// stringContent flattens a message's content into a string: OpenAI content is either a
// plain string or an array of typed parts (the multimodal form), and the suite only
// asserts on the text.
func stringContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if txt, ok := pm["text"].(string); ok {
				b.WriteString(txt)
			}
		}
		return b.String()
	default:
		return ""
	}
}

func writeCompletion(w http.ResponseWriter, reply oaiReply) {
	msg := map[string]any{}
	finish := "stop"
	if len(reply.ToolCalls) > 0 {
		finish = "tool_calls"
		var calls []map[string]any
		for _, tc := range reply.ToolCalls {
			args := tc.Args
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":       tc.ID,
				"type":     "function",
				"function": map[string]any{"name": tc.Name, "arguments": args},
			})
		}
		msg["tool_calls"] = calls
		msg["content"] = reply.Text // usually empty on a tool turn
	} else {
		msg["content"] = reply.Text
	}
	usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 5}
	if reply.Usage != nil {
		usage = map[string]any{"prompt_tokens": reply.Usage.Prompt, "completion_tokens": reply.Usage.Completion}
	}
	resp := map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": finish}},
		"usage":   usage,
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func writeError(w http.ResponseWriter, reply oaiReply) {
	body := map[string]any{"error": map[string]any{
		"type":    reply.ErrType,
		"code":    reply.ErrCode,
		"message": reply.ErrMsg,
	}}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(reply.Status)
	_ = json.NewEncoder(w).Encode(body)
}

// finalText builds a plain end-turn reply.
func finalText(s string) oaiReply { return oaiReply{Text: s} }

// toolCall builds a single-tool-call turn.
func toolCall(id, name, args string) oaiReply {
	return oaiReply{ToolCalls: []oaiToolCall{{ID: id, Name: name, Args: args}}}
}
