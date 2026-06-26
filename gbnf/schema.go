package gbnf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Arguments compiles a tool's JSON Schema into a grammar whose root matches a
// single JSON object: the arguments of one call to that tool. A runtime applies it
// so the argument object is structurally valid and type-correct by construction.
//
// The supported subset is what tool argument schemas use in practice: a top-level
// object with typed properties (string, integer, number, boolean, enum, array, and
// nested object), a required list, and either closed (additionalProperties false)
// or open extra properties. Anything outside the subset is a compile error so a
// runtime is never handed a grammar that quietly permits an invalid call.
func Arguments(schema json.RawMessage) (*Grammar, error) {
	s, err := decodeSchema(schema)
	if err != nil {
		return nil, err
	}
	b := newBuilder()
	root, err := b.compile(s, "root")
	if err != nil {
		return nil, err
	}
	return b.grammar(root)
}

// schemaNode is the slice of JSON Schema this package reads. Unknown keywords are
// ignored rather than rejected, but a type or shape that cannot be constrained is
// reported by the compiler.
type schemaNode struct {
	Type                 string                `json:"type"`
	Properties           map[string]schemaNode `json:"properties"`
	Required             []string              `json:"required"`
	Items                *schemaNode           `json:"items"`
	Enum                 []json.RawMessage     `json:"enum"`
	AdditionalProperties *bool                 `json:"additionalProperties"`
	// propOrder preserves the declared order of properties, which json.Unmarshal into
	// a map loses. It is filled by a second pass over the raw object.
	propOrder []string
}

// UnmarshalJSON decodes the schema and additionally records property order, which a
// plain map decode discards but the object grammar needs to place members and
// commas deterministically.
func (s *schemaNode) UnmarshalJSON(data []byte) error {
	type raw schemaNode
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	*s = schemaNode(r)
	if s.Type == "object" || s.Properties != nil {
		order, err := objectKeyOrder(data, "properties")
		if err != nil {
			return err
		}
		s.propOrder = order
	}
	return nil
}

// objectKeyOrder returns the keys of the object named field, in source order, by
// streaming the raw JSON. json.Unmarshal into a Go map drops order, but constrained
// generation must emit properties in a fixed order, so the source order is recovered
// here.
func objectKeyOrder(data []byte, field string) ([]string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	body, ok := top[field]
	if !ok {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("gbnf: %s is not an object", field)
	}
	var order []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("gbnf: non-string key in %s", field)
		}
		order = append(order, key)
		// Skip the value, however nested, so the next token is the following key.
		if err := skipValue(dec); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// skipValue consumes one complete JSON value from the decoder, descending through
// nested objects and arrays so the caller lands on the next sibling token.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		for dec.More() {
			if d == '{' {
				if _, err := dec.Token(); err != nil { // key
					return err
				}
			}
			if err := skipValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // closing delim
			return err
		}
	}
	return nil
}

// builder accumulates named rules while compiling a schema, sharing the JSON
// primitive rules and the object-tail rules so the emitted grammar stays small.
type builder struct {
	rules map[string]node
	order []string
	seq   int
}

func newBuilder() *builder {
	return &builder{rules: map[string]node{}}
}

// add registers a rule once. A name already present is left as is, which lets
// shared primitives be requested freely.
func (b *builder) add(name string, n node) {
	if _, ok := b.rules[name]; ok {
		return
	}
	b.rules[name] = n
	b.order = append(b.order, name)
}

// fresh returns a unique rule name with the given prefix.
func (b *builder) fresh(prefix string) string {
	b.seq++
	return fmt.Sprintf("%s_%d", prefix, b.seq)
}

// grammar finalizes the builder into a Grammar rooted at root, ordering rules with
// the root first so the rendered text leads with the start rule.
func (b *builder) grammar(root string) (*Grammar, error) {
	order := make([]string, 0, len(b.order))
	order = append(order, root)
	for _, name := range b.order {
		if name != root {
			order = append(order, name)
		}
	}
	g := &Grammar{root: root, rules: b.rules, order: order}
	// Defend the invariant the recognizer and runtime both rely on: every reference
	// resolves to a defined rule.
	for _, name := range g.References() {
		if _, ok := g.rules[name]; !ok {
			return nil, fmt.Errorf("gbnf: rule %q referenced but not defined", name)
		}
	}
	return g, nil
}

// compile builds the rule for one schema node, registers it under name, and returns
// name. It dispatches on the schema's type, falling back to an enum constraint when
// type is absent but enum is present.
func (b *builder) compile(s *schemaNode, name string) (string, error) {
	if len(s.Enum) > 0 && s.Type == "" {
		n, err := enumNode(s.Enum)
		if err != nil {
			return "", err
		}
		b.add(name, n)
		return name, nil
	}
	switch s.Type {
	case "object":
		return b.compileObject(s, name)
	case "array":
		return b.compileArray(s, name)
	case "string":
		if len(s.Enum) > 0 {
			n, err := enumNode(s.Enum)
			if err != nil {
				return "", err
			}
			b.add(name, n)
			return name, nil
		}
		b.needString()
		b.add(name, ref{"json_string"})
		return name, nil
	case "integer":
		b.needInteger()
		b.add(name, ref{"json_integer"})
		return name, nil
	case "number":
		b.needNumber()
		b.add(name, ref{"json_number"})
		return name, nil
	case "boolean":
		b.needBoolean()
		b.add(name, ref{"json_boolean"})
		return name, nil
	case "":
		// An untyped schema with no enum admits any JSON value; constrain it to
		// well-formed JSON rather than refusing, so partially-specified tools still
		// gain structural safety.
		b.needValue()
		b.add(name, ref{"json_value"})
		return name, nil
	default:
		return "", fmt.Errorf("gbnf: unsupported schema type %q", s.Type)
	}
}

// compileObject builds the rule for an object schema. A closed object (the default,
// and what every tool argument schema uses) is a brace-delimited list of its
// declared members in order, each required member mandatory and each optional one
// skippable, with no other members permitted. An open object with no declared
// properties is a free-form JSON object.
//
// An open object that also declares typed properties is refused: a context-free
// grammar cannot both type-check a declared property and admit an arbitrary extra
// property under a different name without letting the declared one be re-admitted
// untyped, which would silently permit an invalid call. Refusing is sound; guessing
// is not. This case does not arise for tool argument schemas, which are closed.
func (b *builder) compileObject(s *schemaNode, name string) (string, error) {
	open := s.AdditionalProperties != nil && *s.AdditionalProperties
	if open {
		if len(s.propOrder) > 0 {
			return "", errors.New("gbnf: object with additionalProperties:true and declared properties is not supported")
		}
		b.needValue()
		b.add(name, ref{"json_object"})
		return name, nil
	}

	b.needWS()
	declared := map[string]bool{}
	for _, key := range s.propOrder {
		declared[key] = true
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		// A required property with no entry in "properties" cannot be type-constrained
		// or even located in the declared-order body, so its mandatoriness would be
		// silently lost. Refuse rather than emit a grammar that admits a call missing a
		// required argument.
		if !declared[r] {
			return "", fmt.Errorf("gbnf: required property %q is not declared in properties", r)
		}
		required[r] = true
	}
	// Compile each property's value rule first; the tail rules reference them.
	memberNodes := make([]node, len(s.propOrder))
	for i, key := range s.propOrder {
		ps := s.Properties[key]
		valName, err := b.compile(&ps, b.fresh("val"))
		if err != nil {
			return "", fmt.Errorf("gbnf: property %q: %w", key, err)
		}
		// A trailing whitespace rule lets insignificant space sit between this value
		// and the following comma or closing brace, so the grammar accepts any JSON
		// spacing while still requiring the tokens in order.
		memberNodes[i] = seq{[]node{
			lit{strconv.Quote(key)},
			ref{"json_ws"},
			lit{":"},
			ref{"json_ws"},
			ref{valName},
			ref{"json_ws"},
		}}
	}
	tail := b.objectTail(name, s.propOrder, memberNodes, required, 0, true)
	body := seq{[]node{lit{"{"}, ref{"json_ws"}, ref{tail}, lit{"}"}}}
	b.add(name, body)
	return name, nil
}

// objectTail builds (and registers) the rule matching declared members from index i
// onward. first records that no member has yet been emitted, so the next present
// member carries no leading comma. Each (i, first) tail is its own named rule, so
// optional properties do not multiply the grammar's size. Past the last declared
// property nothing more may appear: a closed object admits no extra members.
func (b *builder) objectTail(base string, keys []string, members []node, required map[string]bool, i int, first bool) string {
	name := fmt.Sprintf("%s_tail_%d_%t", base, i, first)
	if _, ok := b.rules[name]; ok {
		return name
	}
	// Reserve the name before recursing so a referenced tail always resolves.
	b.add(name, seq{})

	if i == len(keys) {
		b.rules[name] = seq{} // matches empty: no further members
		return name
	}

	lead := func(n node) node {
		if first {
			return n
		}
		return seq{[]node{lit{","}, ref{"json_ws"}, n}}
	}
	present := seq{[]node{lead(members[i]), ref{b.objectTail(base, keys, members, required, i+1, false)}}}
	if required[keys[i]] {
		b.rules[name] = present
		return name
	}
	absent := ref{b.objectTail(base, keys, members, required, i+1, first)}
	b.rules[name] = alt{[]node{present, absent}}
	return name
}

// compileArray builds the rule for an array schema: bracket-delimited, comma
// separated values matching the item schema (or any JSON value when items is
// absent).
func (b *builder) compileArray(s *schemaNode, name string) (string, error) {
	b.needWS()
	itemRef := "json_value"
	if s.Items != nil {
		b.needValue()
		var err error
		itemRef, err = b.compile(s.Items, b.fresh("item"))
		if err != nil {
			return "", err
		}
	} else {
		b.needValue()
	}
	item := seq{[]node{ref{"json_ws"}, ref{itemRef}, ref{"json_ws"}}}
	body := seq{[]node{
		lit{"["},
		ref{"json_ws"},
		opt{seq{[]node{item, star{seq{[]node{lit{","}, item}}}}}},
		ref{"json_ws"},
		lit{"]"},
	}}
	b.add(name, body)
	return name, nil
}

// enumNode constrains a value to one of a fixed set of JSON literals, rendered in
// their canonical compact form so the grammar matches exactly those values.
func enumNode(values []json.RawMessage) (node, error) {
	alts := make([]node, 0, len(values))
	for _, v := range values {
		canon, err := canonicalJSON(v)
		if err != nil {
			return nil, fmt.Errorf("gbnf: enum value: %w", err)
		}
		alts = append(alts, lit{canon})
	}
	if len(alts) == 1 {
		return alts[0], nil
	}
	return alt{alts}, nil
}

// canonicalJSON reduces a JSON value to the compact, key-sorted form the grammar
// emits, so an enum literal matches regardless of incidental whitespace in the
// schema source.
func canonicalJSON(v json.RawMessage) (string, error) {
	var decoded any
	if err := json.Unmarshal(v, &decoded); err != nil {
		return "", err
	}
	out, err := marshalCanonical(decoded)
	if err != nil {
		return "", err
	}
	return out, nil
}

// marshalCanonical serializes a decoded JSON value to compact text. encoding/json
// already emits object keys in sorted order and omits insignificant whitespace, so
// the result is a stable canonical form an enum literal can be compared against.
func marshalCanonical(v any) (string, error) {
	bs, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(bs), nil
}

// --- shared JSON primitive rules --------------------------------------------

func (b *builder) needWS() {
	b.add("json_ws", star{class{ranges: [][2]rune{{' ', ' '}, {'\t', '\t'}, {'\n', '\n'}, {'\r', '\r'}}}})
}

func (b *builder) needString() {
	b.add("json_string", seq{[]node{lit{`"`}, star{ref{"json_char"}}, lit{`"`}}})
	b.add("json_char", alt{[]node{
		class{ranges: [][2]rune{{'"', '"'}, {'\\', '\\'}}, negated: true},
		seq{[]node{lit{`\`}, ref{"json_escape"}}},
	}})
	b.add("json_escape", alt{[]node{
		class{ranges: [][2]rune{{'"', '"'}, {'\\', '\\'}, {'/', '/'}, {'b', 'b'}, {'f', 'f'}, {'n', 'n'}, {'r', 'r'}, {'t', 't'}}},
		seq{[]node{lit{"u"}, ref{"json_hex"}, ref{"json_hex"}, ref{"json_hex"}, ref{"json_hex"}}},
	}})
	b.add("json_hex", class{ranges: [][2]rune{{'0', '9'}, {'a', 'f'}, {'A', 'F'}}})
}

func (b *builder) needInteger() {
	b.add("json_integer", seq{[]node{
		opt{lit{"-"}},
		alt{[]node{lit{"0"}, seq{[]node{class{ranges: [][2]rune{{'1', '9'}}}, star{class{ranges: [][2]rune{{'0', '9'}}}}}}}},
	}})
}

func (b *builder) needBoolean() {
	b.add("json_boolean", alt{[]node{lit{"true"}, lit{"false"}}})
}

func (b *builder) needNumber() {
	b.needInteger()
	b.add("json_number", seq{[]node{
		ref{"json_integer"},
		opt{seq{[]node{lit{"."}, plus{class{ranges: [][2]rune{{'0', '9'}}}}}}},
		opt{seq{[]node{
			class{ranges: [][2]rune{{'e', 'e'}, {'E', 'E'}}},
			opt{class{ranges: [][2]rune{{'+', '+'}, {'-', '-'}}}},
			plus{class{ranges: [][2]rune{{'0', '9'}}}},
		}}},
	}})
}

// needValue defines a generic JSON value rule and everything it depends on, for
// open objects, untyped properties, and free-form array items.
func (b *builder) needValue() {
	b.needWS()
	b.needString()
	b.needNumber()
	b.needBoolean()
	member := seq{[]node{ref{"json_ws"}, ref{"json_string"}, ref{"json_ws"}, lit{":"}, ref{"json_value"}}}
	b.add("json_object", seq{[]node{
		lit{"{"},
		ref{"json_ws"},
		opt{seq{[]node{member, star{seq{[]node{lit{","}, member}}}}}},
		ref{"json_ws"},
		lit{"}"},
	}})
	elem := seq{[]node{ref{"json_ws"}, ref{"json_value"}, ref{"json_ws"}}}
	b.add("json_array", seq{[]node{
		lit{"["},
		ref{"json_ws"},
		opt{seq{[]node{elem, star{seq{[]node{lit{","}, elem}}}}}},
		ref{"json_ws"},
		lit{"]"},
	}})
	b.add("json_value", seq{[]node{
		ref{"json_ws"},
		alt{[]node{ref{"json_string"}, ref{"json_number"}, ref{"json_boolean"}, lit{"null"}, ref{"json_array"}, ref{"json_object"}}},
		ref{"json_ws"},
	}})
}
