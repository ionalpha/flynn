package openai

import (
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/secret"
)

// benchTools is a representative tool set: a handful of tools with typed argument
// schemas, the shape an agent loop offers turn after turn.
func benchTools() []llm.Tool {
	return []llm.Tool{
		{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "write_file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)},
		{Name: "list_dir", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"depth":{"type":"integer"}}}`)},
		{Name: "search", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`)},
	}
}

// BenchmarkToolGrammarUncached measures the cost the memoization removes: compiling
// and rendering the tool-call grammar from scratch, which is what happened on every
// Generate before the cache.
func BenchmarkToolGrammarUncached(b *testing.B) {
	tools := benchTools()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := toolCallGrammar(tools); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkToolGrammarCached measures the steady-state cost once the tool set is
// unchanged turn to turn: a hash of the tools plus a single-entry lookup, no
// recompile. This is the per-turn grammar cost on the local-model path.
func BenchmarkToolGrammarCached(b *testing.B) {
	c := New(secret.New("k"), WithToolGrammar())
	tools := benchTools()
	// Prime the cache so the loop measures the hit path.
	if _, ok := c.toolGrammarCached(tools); !ok {
		b.Fatal("grammar did not compile")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, ok := c.toolGrammarCached(tools); !ok {
			b.Fatal("grammar cache miss")
		}
	}
}
