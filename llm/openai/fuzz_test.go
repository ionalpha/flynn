package openai

import (
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/llm"
)

// FuzzDecodeResponse drives the OpenAI response decode path from raw bytes,
// mirroring how Generate decodes an HTTP body: unmarshal into chatResponse, then
// project to an llm.Response. Enabling a grammar-tool set exercises the
// grammar-constrained tool-call branch, which decodes a model-controlled
// arguments object out of the message content. Provider/proxy/model bytes are all
// untrusted, so the bar is that no input panics, hangs, or yields a tool_use block
// with a nil ToolUse; a malformed frame must surface as a typed error or be
// cleanly rejected, never a partially-built value.
func FuzzDecodeResponse(f *testing.F) {
	seeds := []string{
		`{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		`{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"content":"{\"name\":\"search\",\"arguments\":{\"q\":\"y\"}}"},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"content":"{\"name\":\"unknown\"}"}}]}`, // grammar call naming an untooled name
		`{"choices":[{"message":{"content":"{ not valid json"}}]}`,       // grammar branch, bad json
		`{"choices":[{"message":{"content":"{\"name\":\"search\"}"}}]}`,  // grammar call, empty arguments
		`{"choices":[]}`, // no choices -> terminal
		`{}`,
		`{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"[]"}}]}}]}`,
		`not json`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	// A fixed constrained-tool set so the grammar branch and parseGrammarToolCall
	// (the model-controlled arguments decode) are on the hot path for every input.
	grammarTools := map[string]bool{"search": true, "do": true}

	f.Fuzz(func(t *testing.T, body []byte) {
		var cr chatResponse
		if err := json.Unmarshal(body, &cr); err != nil {
			return // an undecodable body never reaches decodeResponse in Generate
		}
		resp, err := decodeResponse(cr, grammarTools)
		if err != nil {
			return // a typed fault (e.g. no choices) is the correct outcome
		}
		for _, b := range resp.Message.Blocks {
			if b.Kind == llm.KindToolUse && b.ToolUse == nil {
				t.Fatalf("decodeResponse returned a tool_use block with nil ToolUse")
			}
		}
	})
}
