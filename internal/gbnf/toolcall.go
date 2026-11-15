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

// ToolCall compiles a grammar whose root matches one tool-call object,
// {"name": <tool name>, "arguments": <that tool's arguments>}. Name and argument
// shape are bound together, so a call naming one tool cannot carry another's
// arguments. An empty tool set or a schema that fails to compile is an error.
func ToolCall(tools []ToolSchema) (*Grammar, error) {
	b, alts, err := buildToolAlts(tools)
	if err != nil {
		return nil, err
	}
	root := "root"
	if len(alts) == 1 {
		b.add(root, alts[0])
	} else {
		b.add(root, alt{alts})
	}
	return b.grammar(root)
}

// ToolCallOrText compiles a grammar whose root matches either a tool call, as
// [ToolCall], or a free-text answer that does not begin with "{". An agent loop uses
// it so a constrained model can still end its turn with prose. The first character
// tells the branches apart, so there is no ambiguity.
func ToolCallOrText(tools []ToolSchema) (*Grammar, error) {
	b, alts, err := buildToolAlts(tools)
	if err != nil {
		return nil, err
	}
	// A negated class admits any code point except NUL (and "{" in the first
	// position). It is the portable "any character" in this dialect; a positive
	// range up to the maximum code point is not.
	b.add("text_char", class{ranges: [][2]rune{{0, 0}}, negated: true})
	b.add("text_head", class{ranges: [][2]rune{{0, 0}, {'{', '{'}}, negated: true})
	b.add("answer", seq{[]node{ref{"text_head"}, star{ref{"text_char"}}}})
	alts = append(alts, ref{"answer"})
	b.add("root", alt{alts})
	return b.grammar("root")
}

// buildToolAlts validates the tool set and compiles each tool into a call envelope,
// in an order independent of the caller's, so the grammar is deterministic.
func buildToolAlts(tools []ToolSchema) (*builder, []node, error) {
	if len(tools) == 0 {
		return nil, nil, errors.New("gbnf: no tools to constrain")
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
			return nil, nil, errors.New("gbnf: tool with empty name")
		}
		if seen[t.Name] {
			return nil, nil, fmt.Errorf("gbnf: duplicate tool name %q", t.Name)
		}
		seen[t.Name] = true

		s, err := decodeSchema(t.Schema)
		if err != nil {
			return nil, nil, fmt.Errorf("gbnf: tool %q: %w", t.Name, err)
		}
		argsRule, err := b.compile(s, b.fresh("args"), 0)
		if err != nil {
			return nil, nil, fmt.Errorf("gbnf: tool %q: %w", t.Name, err)
		}
		alts = append(alts, callEnvelope(t.Name, argsRule))
	}
	return b, alts, nil
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

// maxSchemaBytes bounds a raw argument schema. Schemas can come from an external
// tool server, and an unbounded one costs unbounded parse time and memory.
const maxSchemaBytes = 1 << 18 // 256 KiB

// maxSchemaDepth bounds compile recursion, so a deeply nested schema is an error
// instead of a stack overflow.
const maxSchemaDepth = 64

// decodeSchema parses a raw argument schema. An empty schema is a tool with no
// arguments, a closed empty object. Malformed JSON or a schema over
// [maxSchemaBytes] is an error, so no entry point admits a schema it cannot read.
func decodeSchema(raw json.RawMessage) (*schemaNode, error) {
	if len(raw) == 0 {
		closed := false
		return &schemaNode{Type: "object", AdditionalProperties: &closed}, nil
	}
	if len(raw) > maxSchemaBytes {
		return nil, fmt.Errorf("gbnf: schema is %d bytes, over the %d-byte limit", len(raw), maxSchemaBytes)
	}
	var s schemaNode
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("gbnf: parse schema: %w", err)
	}
	return &s, nil
}
