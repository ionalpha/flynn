package mission

import (
	"encoding/json"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// maxToolDescription is in runes: about one sentence, because a weaker model struggles to
// weigh long prose against every other tool.
const maxToolDescription = 160

// simplifyTool shortens the description and drops schema documentation, keeping every
// property and requirement, so nothing callable becomes uncallable.
func simplifyTool(t llm.Tool) llm.Tool {
	t.Description = trimDescription(t.Description)
	t.InputSchema = stripSchemaDocs(t.InputSchema)
	return t
}

// trimDescription cuts at the last sentence boundary within the limit when there is one.
func trimDescription(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxToolDescription {
		return s
	}
	// Cut on runes, because a byte cut can hand the model invalid UTF-8.
	clipped := string(r[:maxToolDescription])
	if i := strings.LastIndex(clipped, ". "); i > 0 {
		return clipped[:i+1]
	}
	return strings.TrimSpace(clipped)
}

// docKeys constrain nothing, so dropping them leaves validation identical. default, enum
// and const carry meaning and are kept.
var docKeys = []string{"description", "examples", "example", "title"}

// stripSchemaDocs returns input it cannot decode or re-encode unchanged, so it never
// produces a schema the model could not have received.
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
