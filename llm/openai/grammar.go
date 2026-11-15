package openai

import (
	"github.com/ionalpha/flynn/internal/gbnf"
	"github.com/ionalpha/flynn/llm"
)

// toolCallGrammar compiles the tools into a GBNF grammar that admits one valid tool call
// or a free-text answer. A local runtime applies it as a decode-time mask, so a malformed
// call cannot be sampled at any model size. It errors if any schema is outside the
// supported subset, so the caller leaves the request unconstrained instead of
// constraining it to only some tools.
func toolCallGrammar(tools []llm.Tool) (string, error) {
	specs := make([]gbnf.ToolSchema, len(tools))
	for i, t := range tools {
		specs[i] = gbnf.ToolSchema{Name: t.Name, Schema: t.InputSchema}
	}
	g, err := gbnf.ToolCallOrText(specs)
	if err != nil {
		return "", err
	}
	return g.String(), nil
}
