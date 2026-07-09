package signalcli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/inbox"
)

// FuzzDispatch drives one JSON-RPC line from the signal-cli daemon through the
// routing every received frame takes. The daemon is a subprocess whose output the
// process does not control, and dispatch is the only thing standing between its
// bytes and an inbox entry, so the bar is that no line panics and that the routing
// rules hold for every input: a frame carrying an id is a response and can never
// become an inbox entry, a frame without one is only an inbox entry if it is a
// "receive" notification carrying a non-empty text message, and a response for a
// call nobody is waiting on is dropped rather than blocking the reader.
func FuzzDispatch(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+15550100","dataMessage":{"message":"hi"}}}}`,
		`{"jsonrpc":"2.0","method":"receive","params":{"result":{"envelope":{"source":"+15550100","dataMessage":{"message":"wrapped"}}}}}`,
		`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+15550100","dataMessage":{"message":"g","groupInfo":{"groupId":"abc"}}}}}`,
		`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+15550100","dataMessage":{"message":""}}}}`,
		`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+15550100"}}}`,
		`{"jsonrpc":"2.0","method":"receive","params":null}`,
		`{"jsonrpc":"2.0","method":"receive"}`,
		`{"jsonrpc":"2.0","id":1,"result":{"timestamp":1}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad params"}}`,
		`{"jsonrpc":"2.0","id":99,"result":null}`, // response nobody awaits
		`{"jsonrpc":"2.0","id":1,"method":"receive","params":{"envelope":{"source":"s","dataMessage":{"message":"m"}}}}`,
		`{"jsonrpc":"2.0","method":"sync"}`,
		`{"id":"not a number"}`,
		`{}`,
		`banner text from the daemon`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, line []byte) {
		c, err := New("127.0.0.1:7583")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		// One waiter, so a response frame for id 1 has somewhere to land and every
		// other id exercises the drop path. The channel is buffered exactly as call
		// buffers it, so a delivery never blocks the reader.
		waiter := make(chan rpcResult, 1)
		c.pending[1] = waiter

		out := make(chan inbox.Spec, 1)
		c.dispatch(context.Background(), line, out)

		var frame struct {
			ID     *uint64         `json:"id"`
			Method string          `json:"method"`
			Error  *rpcError       `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		decoded := json.Unmarshal(line, &frame) == nil

		select {
		case spec := <-out:
			if !decoded {
				t.Fatalf("emitted a spec for a line that is not a JSON-RPC frame: %q", line)
			}
			if frame.ID != nil {
				t.Fatalf("emitted a spec for a response frame (id %d): %q", *frame.ID, line)
			}
			if frame.Method != "receive" {
				t.Fatalf("emitted a spec for method %q, want only \"receive\": %q", frame.Method, line)
			}
			// An empty message is not an inbox entry: it would surface a typing
			// indicator or a read receipt as if the peer had said something.
			if spec.Content == "" {
				t.Fatalf("emitted a spec with empty Content: %q", line)
			}
			if spec.Type != "message" {
				t.Fatalf("emitted a spec of type %q, want \"message\": %q", spec.Type, line)
			}
		default:
			// No entry. Correct unless this was a receive notification that carried a
			// non-empty text message, which specFromParams is the authority on.
			if decoded && frame.ID == nil && frame.Method == "receive" {
				if _, ok := specFromParams(frame.Params); ok {
					t.Fatalf("dropped a receive notification carrying a message: %q", line)
				}
			}
		}

		select {
		case res := <-waiter:
			if !decoded || frame.ID == nil || *frame.ID != 1 {
				t.Fatalf("woke the waiter for a frame that is not a response for id 1: %q", line)
			}
			// The error object is the only thing that decides success from failure;
			// a caller that cannot tell them apart retries a permanent failure.
			if (res.err != nil) != (frame.Error != nil) {
				t.Fatalf("delivered err=%v for a frame whose error object is %v: %q", res.err, frame.Error, line)
			}
		default:
			if decoded && frame.ID != nil && *frame.ID == 1 {
				t.Fatalf("a response for the pending id 1 never reached its waiter: %q", line)
			}
		}
	})
}
