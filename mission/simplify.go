package mission

import (
	"encoding/json"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// maxToolDescription is the length a simplified tool description is trimmed to. It keeps the
// first sentence or so, enough to convey purpose, without the long prose a weaker model
// struggles to weigh against every other tool.
const maxToolDescription = 160

// simplifyTool returns a leaner copy of a tool definition for a model with limited
// instruction-following: the standing description is shortened and the input schema is stripped
// of per-field documentation (descriptions, examples, titles, defaults) while every property and
// the set of required properties are preserved. The callable surface is therefore unchanged, so
// nothing the model could call before becomes uncallable; only the volume of prose it must reason
// over shrinks. A schema that cannot be parsed as a JSON object is passed through untouched.
func simplifyTool(t llm.Tool) llm.Tool {
	t.Description = trimDescription(t.Description)
	t.InputSchema = stripSchemaDocs(t.InputSchema)
	return t
}

// trimDescription shortens prose to maxToolDescription, cutting at the first sentence boundary
// within the limit when there is one so the result reads as a complete thought rather than a
// hard truncation.
func trimDescription(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxToolDescription {
		return s
	}
	clipped := s[:maxToolDescription]
	if i := strings.LastIndex(clipped, ". "); i > 0 {
		return clipped[:i+1]
	}
	return strings.TrimSpace(clipped)
}

// docKeys are the schema annotation fields that document a property for a human or a strong
// model but add reasoning load for a weak one. They are pure documentation: none constrains what
// is callable or what a value means, so dropping them leaves the schema's validation behavior
// identical. Keys that can carry meaning (default, enum, const) are deliberately excluded.
var docKeys = []string{"description", "examples", "example", "title"}

// stripSchemaDocs walks a JSON-schema document and removes the documentation keys at every level
// while leaving structural keywords (type, properties, required, items, enum, and the like) in
// place. Input that is not a JSON object, or that fails to re-encode, is returned unchanged so the
// transform can never produce a schema the model could not have received originally.
func stripSchemaDocs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return raw
	}
	pruned := pruneDocs(node)
	out, err := json.Marshal(pruned)
	if err != nil {
		return raw
	}
	return out
}

// pruneDocs recurses through decoded JSON, dropping documentation keys from every object and
// descending into arrays, so annotations nested under properties or items are removed too.
func pruneDocs(node any) any {
	switch v := node.(type) {
	case map[string]any:
		for _, k := range docKeys {
			delete(v, k)
		}
		for key, child := range v {
			v[key] = pruneDocs(child)
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = pruneDocs(child)
		}
		return v
	default:
		return node
	}
}
