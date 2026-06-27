package openai

import (
	"github.com/ionalpha/flynn/gbnf"
	"github.com/ionalpha/flynn/llm"
)

// toolCallGrammar compiles the offered tools into a GBNF grammar that admits either
// one structurally valid tool call (an object naming one of the tools bound to
// arguments that satisfy that tool's JSON Schema) or a free-text final answer. A
// local runtime applies it as a decode-time token mask, so the malformed-call failure
// class is removed by construction regardless of model size, while the model can still
// end its turn with prose. It returns an error if any tool's schema is outside the
// supported subset, so the caller can leave the request unconstrained rather than
// constrain it to only some of the tools.
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
