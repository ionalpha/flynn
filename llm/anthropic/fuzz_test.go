package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/llm"
)

// FuzzDecodeResponse drives the provider response decode path from raw bytes,
// mirroring how Generate decodes an HTTP body: unmarshal into apiResponse, then
// project to an llm.Response. Provider/proxy bytes are untrusted, so the bar is
// that no input panics, hangs, or yields a block that is malformed for its kind.
// A malformed frame must surface as a typed error, never a partially-built value.
func FuzzDecodeResponse(f *testing.F) {
	seeds := []string{
		`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
		`{"content":[{"type":"tool_use","id":"t1","name":"do","input":{"a":1}}],"stop_reason":"tool_use"}`,
		`{"content":[{"type":"thinking","thinking":"reasoning"}]}`,
		`{"content":[{"type":"text","text":"a"},{"type":"tool_use","id":"x","name":"y","input":[]}]}`,
		`{"content":[],"stop_reason":"end_turn"}`,
		`{}`,
		`{"content":[{"type":"text","text":123}]}`, // wrong scalar type
		`{"content":[{"type":"tool_use"}]}`,        // missing fields
		`{"content":[{"type":123}]}`,               // head decode fails
		`{"content":[null]}`,
		`{"content":[{"type":"text","text":"deep","input":{"a":{"b":{"c":1}}}}]}`,
		`not valid json`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		var ar apiResponse
		if err := json.Unmarshal(body, &ar); err != nil {
			return // an undecodable body never reaches decodeResponse in Generate
		}
		resp, err := decodeResponse(ar)
		if err != nil {
			return // a typed fault is the correct outcome for a malformed frame
		}
		// On success, every projected block must be well-formed for its kind.
		for _, b := range resp.Message.Blocks {
			switch b.Kind {
			case llm.KindToolUse:
				if b.ToolUse == nil {
					t.Fatalf("decodeResponse returned a tool_use block with nil ToolUse")
				}
			case llm.KindOpaque:
				if len(b.Raw) == 0 {
					t.Fatalf("decodeResponse returned an opaque block with empty Raw")
				}
			}
		}
	})
}
