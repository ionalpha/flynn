package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/mission"
)

// An MCP server reads its frames from whatever is on the other end of the pipe: a
// client the agent does not control, or a subprocess that has started emitting
// garbage. Serve therefore has to survive arbitrary bytes, and it has to keep the
// JSON-RPC contract while doing it, because a client pairs replies to calls by id
// and a dropped or spurious reply desynchronizes the session for good.

// fuzzTools is the toolset every fuzz case serves: one that echoes, one that
// fails, and one that declares a schema, so tools/list and tools/call both have
// something to render and a call can go either way.
func fuzzTools() []mission.Tool {
	return []mission.Tool{
		echo("echo"),
		fakeTool{name: "boom", fn: func(json.RawMessage) (string, error) { return "", errors.New("tool failed") }},
		fakeTool{
			name:   "schema",
			schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			fn:     func(in json.RawMessage) (string, error) { return string(in), nil },
		},
	}
}

// wantReplies decodes the input exactly as readLoop does and returns the ids of
// the messages that must be answered: every well-formed frame the decoder reaches
// before it fails, minus the notifications. It is the oracle Serve is held to.
func wantReplies(in []byte) []string {
	var ids []string
	dec := json.NewDecoder(bufio.NewReader(bytes.NewReader(in)))
	for {
		var msg inbound
		if err := dec.Decode(&msg); err != nil {
			return ids
		}
		if !msg.isNotification() {
			ids = append(ids, string(msg.ID))
		}
	}
}

// FuzzServeFrames throws arbitrary bytes at the stdio server. The bar is that
// Serve never panics and never hangs, and that what it writes is always a valid
// JSON-RPC reply stream: one reply per request, in order, with the id echoed
// verbatim, and exactly one of result or error set on each.
//
// A frame the decoder cannot parse ends the session with a typed transport fault,
// which is Serve's documented contract: a JSON stream cannot be resynchronized
// after a malformed value, so there is no frame boundary left to answer on. The
// replies written before that point must still be exactly the ones the requests
// before it earned.
func FuzzServeFrames(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"boom"}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":7}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":null,"method":"ping"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":{"deep":[1,2,3]},"method":"ping"}`))
	f.Add([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"ping\"}"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"} trailing garbage`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(""))
	f.Add([]byte("\x00\x00\x00"))

	f.Fuzz(func(t *testing.T, in []byte) {
		srv := NewServer(nil, fuzzTools())
		var out bytes.Buffer
		// A bytes.Reader ends in EOF, so a Serve that respects its read loop always
		// returns; a hang here is a deadlock the fuzzer should surface as a timeout.
		// Any failure must arrive as a typed fault rather than a panic or a raw
		// decoder error leaking out of the transport.
		if err := srv.Serve(context.Background(), bytes.NewReader(in), &out); err != nil {
			if cls := fault.Classify(err); cls != fault.Transient {
				t.Fatalf("Serve failed with class %v, want a transient transport fault: %v", cls, err)
			}
		}

		want := wantReplies(in)
		dec := json.NewDecoder(strings.NewReader(out.String()))
		var got []string
		for {
			var r rawResp
			err := dec.Decode(&r)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("wrote a frame that is not valid JSON: %v (buffer %q)", err, out.String())
			}
			if r.JSONRPC != "2.0" {
				t.Fatalf("reply carries jsonrpc %q, want \"2.0\"", r.JSONRPC)
			}
			// Exactly one of result or error, or a client cannot tell success from
			// failure without guessing.
			hasResult, hasError := len(r.Result) > 0, r.Error != nil
			if hasResult == hasError {
				t.Fatalf("reply has result=%v error=%v, want exactly one (frame %+v)", hasResult, hasError, r)
			}
			got = append(got, string(r.ID))
		}

		if len(got) != len(want) {
			t.Fatalf("wrote %d replies for %d requests\n got %q\nwant %q", len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("reply %d echoes id %s, want %s", i, got[i], want[i])
			}
		}
	})
}

// FuzzHTTPFrame throws arbitrary bytes at the streamable-HTTP transport's POST
// body. A frame it cannot parse must come back as a JSON-RPC parse error with a
// 400, never a panic and never an empty body: an HTTP client that gets neither a
// reply nor an error has nothing to retry against.
func FuzzHTTPFrame(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	f.Add([]byte(`{"jsonrpc":`))
	f.Add([]byte(`[]`))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, body []byte) {
		h := NewServer(nil, fuzzTools()).HTTPHandler(context.Background(), "")
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Host = "127.0.0.1:7000" // the bridge only serves the loopback interface
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		res := rec.Result()
		defer func() { _ = res.Body.Close() }()
		switch res.StatusCode {
		case http.StatusOK, http.StatusAccepted, http.StatusBadRequest:
		default:
			t.Fatalf("status %d on a malformed body, want 200, 202 or 400", res.StatusCode)
		}
		payload, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		// A 202 answers a notification and carries no body, per JSON-RPC over HTTP.
		if res.StatusCode == http.StatusAccepted {
			if len(bytes.TrimSpace(payload)) != 0 {
				t.Fatalf("202 carries a body: %q", payload)
			}
			return
		}
		var r rawResp
		if err := json.Unmarshal(payload, &r); err != nil {
			t.Fatalf("wrote a body that is not valid JSON: %v (%q)", err, payload)
		}
		if r.JSONRPC != "2.0" {
			t.Fatalf("reply carries jsonrpc %q, want \"2.0\"", r.JSONRPC)
		}
		if res.StatusCode == http.StatusBadRequest && r.Error == nil {
			t.Fatalf("400 without a JSON-RPC error object: %q", payload)
		}
	})
}
