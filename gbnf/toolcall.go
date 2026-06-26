package gbnf

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// ToolSchema names a callable tool and the JSON Schema of its arguments. It is the
// input ToolCall constrains a model's output against.
type ToolSchema struct {
	Name   string
	Schema json.RawMessage
}

// ToolCall compiles a grammar whose root matches a single tool-call object of the
// form {"name": <one of the tool names>, "arguments": <object matching that tool's
// schema>}. A runtime applies it so a local model can only ever name a real tool and
// can only ever emit arguments that satisfy that specific tool's schema. The tool
// name and its argument shape are bound together: a call naming one tool cannot
// borrow another's arguments.
//
// The tool set must be non-empty and every tool's argument schema must compile;
// either failure is reported rather than producing a permissive grammar.
func ToolCall(tools []ToolSchema) (*Grammar, error) {
	if len(tools) == 0 {
		return nil, errors.New("gbnf: no tools to constrain")
	}
	// Compile in a stable order so the grammar is deterministic regardless of how the
	// caller ordered the tools.
	ordered := append([]ToolSchema(nil), tools...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	b := newBuilder()
	b.needWS()
	seen := map[string]bool{}
	alts := make([]node, 0, len(ordered))
	for _, t := range ordered {
		if t.Name == "" {
			return nil, errors.New("gbnf: tool with empty name")
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("gbnf: duplicate tool name %q", t.Name)
		}
		seen[t.Name] = true

		s, err := decodeSchema(t.Schema)
		if err != nil {
			return nil, fmt.Errorf("gbnf: tool %q: %w", t.Name, err)
		}
		argsRule, err := b.compile(s, b.fresh("args"))
		if err != nil {
			return nil, fmt.Errorf("gbnf: tool %q: %w", t.Name, err)
		}
		alts = append(alts, callEnvelope(t.Name, argsRule))
	}
	root := "root"
	if len(alts) == 1 {
		b.add(root, alts[0])
	} else {
		b.add(root, alt{alts})
	}
	return b.grammar(root)
}

// Tool compiles a grammar for a single tool's argument object, the unit a runtime
// constrains once it has committed to calling that tool. It is ToolCall narrowed to
// one tool with no name envelope.
func Tool(schema json.RawMessage) (*Grammar, error) {
	return Arguments(schema)
}

// callEnvelope builds the object node binding a fixed tool name to its argument
// rule: {"name":"<name>","arguments":<args>} with insignificant whitespace allowed.
func callEnvelope(name, argsRule string) node {
	return seq{[]node{
		lit{"{"},
		ref{"json_ws"},
		lit{`"name"`},
		ref{"json_ws"},
		lit{":"},
		ref{"json_ws"},
		lit{strconv.Quote(name)},
		ref{"json_ws"},
		lit{","},
		ref{"json_ws"},
		lit{`"arguments"`},
		ref{"json_ws"},
		lit{":"},
		ref{"json_ws"},
		ref{argsRule},
		ref{"json_ws"},
		lit{"}"},
	}}
}

// decodeSchema parses a raw argument schema into the internal node. An empty raw
// schema is a tool that takes no arguments: a closed, empty object. Malformed JSON
// is an error rather than a silently permissive grammar, so every entry point
// refuses a schema it cannot read instead of admitting any call.
func decodeSchema(raw json.RawMessage) (*schemaNode, error) {
	if len(raw) == 0 {
		closed := false
		return &schemaNode{Type: "object", AdditionalProperties: &closed}, nil
	}
	var s schemaNode
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("gbnf: parse schema: %w", err)
	}
	return &s, nil
}
