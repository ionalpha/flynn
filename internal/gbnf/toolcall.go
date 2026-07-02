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

// ToolCallOrText compiles a grammar whose root matches EITHER a structurally valid
// tool call (exactly as ToolCall) OR a free-text final answer that does not begin
// with "{". A constrained local model can therefore still end its turn with prose,
// while every tool call it does emit stays well-formed by construction. The two
// branches are told apart by the first character: an output that starts with "{" is
// a tool call and nothing else, so there is no ambiguity. This is the form used for
// an agent loop, where a turn is either a tool call or the model's final answer.
func ToolCallOrText(tools []ToolSchema) (*Grammar, error) {
	b, alts, err := buildToolAlts(tools)
	if err != nil {
		return nil, err
	}
	// answer: a first character that is any text character except "{", then any run of
	// text characters. The character classes are negated so they admit any UTF-8 code
	// point, excluding only the NUL byte (and "{" in the leading position, which is what
	// distinguishes a free-text answer from the tool-call branch). A negated class is the
	// portable way to express "any character" in this grammar dialect; a positive range
	// up to the maximum code point is not.
	b.add("text_char", class{ranges: [][2]rune{{0, 0}}, negated: true})
	b.add("text_head", class{ranges: [][2]rune{{0, 0}, {'{', '{'}}, negated: true})
	b.add("answer", seq{[]node{ref{"text_head"}, star{ref{"text_char"}}}})
	alts = append(alts, ref{"answer"})
	b.add("root", alt{alts})
	return b.grammar("root")
}

// buildToolAlts validates the tool set and compiles each tool into a call-envelope
// node, returning a builder primed with the shared whitespace rule and the
// alternatives in a stable, caller-order-independent sequence. Both ToolCall and
// ToolCallOrText build their root from these alternatives.
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

// maxSchemaBytes bounds the size of a raw argument schema. A schema can be supplied
// by a semi-trusted source (an external tool server), so an unbounded one is a
// denial-of-service vector: parsing and compiling it costs time and memory, and a
// pathologically nested one could exhaust the stack. Real tool schemas are far
// smaller than this; an oversized one is refused rather than processed.
const maxSchemaBytes = 1 << 18 // 256 KiB

// maxSchemaDepth bounds how deeply schema compilation may recurse through nested
// objects and arrays, so a deeply nested schema is refused with an error instead of
// overflowing the stack. Tool argument schemas are shallow; this is generous.
const maxSchemaDepth = 64

// decodeSchema parses a raw argument schema into the internal node. An empty raw
// schema is a tool that takes no arguments: a closed, empty object. Malformed JSON,
// or a schema larger than maxSchemaBytes, is an error rather than a silently
// permissive grammar, so every entry point refuses a schema it cannot safely read
// instead of admitting any call.
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
