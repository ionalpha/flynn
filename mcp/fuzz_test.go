package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/mission"
)

// FuzzHandleFrame drives one JSON-RPC frame end to end: the same inbound decode
// readLoop performs, then handle, which for initialize and tools/call decodes the
// untrusted params (initializeParams, callParams) and dispatches through the
// governance waist. The whole frame is attacker-controlled, so the bar is that no
// frame panics and the invariant of response shape holds: a written reply always
// echoes the request id and carries exactly one of Result or Error.
func FuzzHandleFrame(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":123}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":""}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"missing"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":"not an object"}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, // notification: no reply
		`{"jsonrpc":"2.0","id":8,"method":"unknown/method"}`,
		`{"jsonrpc":"2.0","id":null,"method":"initialize"}`,
		`{}`,
		`not json`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	srv := NewServer(nil, []mission.Tool{echo("echo")})
	f.Fuzz(func(t *testing.T, frame []byte) {
		var in inbound
		if err := json.Unmarshal(frame, &in); err != nil {
			return // an undecodable frame never reaches handle in readLoop
		}
		resp, write := srv.handle(context.Background(), in)
		if !write {
			return // notifications produce no response
		}
		if (resp.Result == nil) == (resp.Error == nil) {
			t.Fatalf("response for %q set both or neither of Result/Error", frame)
		}
		if string(resp.ID) != string(in.ID) {
			t.Fatalf("response id %q does not echo request id %q", resp.ID, in.ID)
		}
	})
}
