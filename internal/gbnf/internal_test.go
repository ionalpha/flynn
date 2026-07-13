package gbnf

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestQuoteEscapesControlCharacters checks the literal emitter: the characters that
// are structural in a GBNF double-quoted literal, and the control characters a raw
// byte would otherwise smuggle into the grammar text, are written as escapes.
func TestQuoteEscapesControlCharacters(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`a`, `"a"`},
		{`"`, `"\""`},
		{`\`, `"\\"`},
		{"\n", `"\n"`},
		{"\r", `"\r"`},
		{"\t", `"\t"`},
		{"a\nb\tc", `"a\nb\tc"`},
	}
	for _, tc := range cases {
		if got := quote(tc.in); got != tc.want {
			t.Errorf("quote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestClassEscapeCoversStructuralAndNonASCII checks the character-class emitter. The
// characters that would otherwise close or reinterpret the class are escaped, and a
// rune outside printable ASCII is emitted as a numeric escape so a wide range renders
// to portable grammar text instead of raw bytes.
func TestClassEscapeCoversStructuralAndNonASCII(t *testing.T) {
	cases := []struct {
		in   rune
		want string
	}{
		{']', `\]`},
		{'\\', `\\`},
		{'-', `\-`},
		{'^', `\^`},
		{'\n', `\n`},
		{'\r', `\r`},
		{'\t', `\t`},
		{'a', "a"},
		{0x00, `\x00`},
		{0x1f, `\x1F`},
		{0x7f, `\x7F`},
		{0x00e9, "\\u00E9"},
		{0xffff, "\\uFFFF"},
		{0x1f600, "\\U0001F600"},
	}
	for _, tc := range cases {
		if got := classEscape(tc.in); got != tc.want {
			t.Errorf("classEscape(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestRenderNodeKinds checks each node kind renders to the grammar dialect with only
// the parentheses precedence requires: a repetition binds to a whole group, and an
// alternation inside a sequence is grouped so the bar does not swallow its
// neighbours.
func TestRenderNodeKinds(t *testing.T) {
	cases := []struct {
		name string
		n    node
		want string
	}{
		{"literal", lit{"ab"}, `"ab"`},
		{"reference underscore becomes hyphen", ref{"json_ws"}, "json-ws"},
		{"single rune class", class{ranges: [][2]rune{{'a', 'a'}}}, "[a]"},
		{"range class", class{ranges: [][2]rune{{'a', 'z'}}}, "[a-z]"},
		{"negated class", class{ranges: [][2]rune{{0, 0}}, negated: true}, `[^\x00]`},
		{"empty sequence", seq{}, `""`},
		{"sequence", seq{[]node{lit{"a"}, lit{"b"}}}, `"a" "b"`},
		{"alternation in sequence is grouped", seq{[]node{alt{[]node{lit{"a"}, lit{"b"}}}, lit{"c"}}}, `("a" | "b") "c"`},
		{"alternation", alt{[]node{lit{"a"}, lit{"b"}}}, `"a" | "b"`},
		{"star of an atom", star{lit{"a"}}, `"a"*`},
		{"star of a group", star{seq{[]node{lit{"a"}, lit{"b"}}}}, `("a" "b")*`},
		{"plus", plus{ref{"x"}}, "x+"},
		{"optional group", opt{seq{[]node{lit{"a"}, lit{"b"}}}}, `("a" "b")?`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(tc.n); got != tc.want {
				t.Errorf("render = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestNodeKindsSatisfyNodeInterface exercises the marker method on every concrete
// kind, which is what keeps the node algebra closed: a kind that stopped satisfying
// it would not compile into this table.
func TestNodeKindsSatisfyNodeInterface(t *testing.T) {
	kinds := []node{
		lit{"a"},
		class{ranges: [][2]rune{{'a', 'z'}}},
		ref{"x"},
		seq{[]node{lit{"a"}}},
		alt{[]node{lit{"a"}}},
		star{lit{"a"}},
		plus{lit{"a"}},
		opt{lit{"a"}},
	}
	if len(kinds) != 8 {
		t.Fatalf("node kinds = %d, want 8", len(kinds))
	}
	for _, k := range kinds {
		k.isNode()
		if render(k) == "" {
			t.Errorf("%T rendered to the empty string", k)
		}
	}
}

// TestAcceptsWithMissingRootRule checks the recognizer refuses a grammar whose start
// rule has no definition rather than matching vacuously.
func TestAcceptsWithMissingRootRule(t *testing.T) {
	g := &Grammar{root: "root", rules: map[string]node{"other": lit{"x"}}, order: []string{"other"}}
	if g.Accepts("") {
		t.Error("a grammar with an undefined start rule must accept nothing")
	}
	if g.Accepts("x") {
		t.Error("a grammar with an undefined start rule must accept nothing")
	}
}

// TestMatcherBudgetExhaustionRejects checks the recognizer's work guard: once the
// budget is spent, matching reports no match instead of running on. A pathological
// grammar or input therefore terminates rather than looping.
func TestMatcherBudgetExhaustionRejects(t *testing.T) {
	g := &Grammar{root: "root", rules: map[string]node{"root": lit{"abc"}}, order: []string{"root"}}
	spent := &matcher{g: g, runes: []rune("abc"), memo: map[memoKey][]int{}, budget: 0}
	if got := spent.match(g.rules["root"], 0); got != nil {
		t.Errorf("an exhausted budget must yield no positions, got %v", got)
	}
	funded := &matcher{g: g, runes: []rune("abc"), memo: map[memoKey][]int{}, budget: 1 << 10}
	if got := funded.match(g.rules["root"], 0); len(got) != 1 || got[0] != 3 {
		t.Errorf("match positions = %v, want [3]", got)
	}
}

// TestRepeatDeduplicatesPositions checks star's closure: a child that can match the
// empty string cannot add a position, so the frontier terminates, and a child reached
// twice at the same position is not re-explored.
func TestRepeatDeduplicatesPositions(t *testing.T) {
	g := &Grammar{
		root: "root",
		rules: map[string]node{
			"root": star{opt{lit{"a"}}},
		},
		order: []string{"root"},
	}
	if !g.Accepts("aaa") {
		t.Error("star of an optional literal must accept a run of them")
	}
	if !g.Accepts("") {
		t.Error("star of an optional literal must accept the empty string")
	}
	if g.Accepts("ab") {
		t.Error("must not accept a character the child cannot match")
	}
}

// TestMergePositionsUnionsAscending checks the position-set merge the recognizer
// relies on: the result stays ascending and duplicate-free, including when both
// inputs hold the same position.
func TestMergePositionsUnionsAscending(t *testing.T) {
	cases := []struct {
		a, b, want []int
	}{
		{nil, []int{1, 2}, []int{1, 2}},
		{[]int{1, 2}, nil, []int{1, 2}},
		{[]int{1, 3}, []int{2, 4}, []int{1, 2, 3, 4}},
		{[]int{1, 2, 3}, []int{2, 3}, []int{1, 2, 3}},
		{[]int{5}, []int{1}, []int{1, 5}},
	}
	for _, tc := range cases {
		got := mergePositions(tc.a, tc.b)
		if len(got) != len(tc.want) {
			t.Fatalf("mergePositions(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("mergePositions(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		}
	}
}

// TestSchemaDecodeRejectsWrongFieldTypes checks a schema whose keywords have the
// wrong JSON types is refused at decode. A permissive grammar from a mistyped schema
// would let an invalid call through, so refusing is the only safe answer.
func TestSchemaDecodeRejectsWrongFieldTypes(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"type is not a string", `{"type": 7}`},
		{"required is not a list", `{"type":"object","required":"path"}`},
		{"properties is not an object", `{"type":"object","properties":7}`},
		{"additionalProperties is not a bool", `{"type":"object","additionalProperties":"yes"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Arguments(json.RawMessage(tc.schema)); err == nil {
				t.Fatalf("expected refusal of %s", tc.schema)
			}
		})
	}
}

// TestSchemaWithNullPropertiesIsRefused checks the property-order pass: "properties"
// present but not an object is a schema this package cannot read, and it is reported
// rather than compiled as a property-free object that admits any call.
func TestSchemaWithNullPropertiesIsRefused(t *testing.T) {
	_, err := Arguments(json.RawMessage(`{"type":"object","properties":null}`))
	if err == nil {
		t.Fatal("expected refusal of a schema whose properties is not an object")
	}
	if !strings.Contains(err.Error(), "properties is not an object") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestObjectKeyOrderSkipsUnrelatedFields checks the streaming order pass reaches
// "properties" past nested siblings of any shape, and reports declared order rather
// than the map order a plain decode would give.
func TestObjectKeyOrderSkipsUnrelatedFields(t *testing.T) {
	raw := []byte(`{
      "title": {"nested": [1, {"deep": true}]},
      "required": ["z"],
      "properties": {"z": {"type":"string"}, "a": {"type":"integer"}, "m": {"type":"boolean"}},
      "trailing": [[]]
    }`)
	order, err := objectKeyOrder(raw, "properties")
	if err != nil {
		t.Fatalf("objectKeyOrder: %v", err)
	}
	want := []string{"z", "a", "m"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestObjectKeyOrderNonObjectInput checks the order pass reports no order, rather
// than an order it invented, when the value it is handed is not an object or does not
// carry the field at all.
func TestObjectKeyOrderNonObjectInput(t *testing.T) {
	for _, raw := range []string{`[1,2]`, `"text"`, `null`, `{"type":"object"}`} {
		order, err := objectKeyOrder([]byte(raw), "properties")
		if err != nil {
			t.Fatalf("objectKeyOrder(%s): %v", raw, err)
		}
		if order != nil {
			t.Errorf("objectKeyOrder(%s) = %v, want no order", raw, order)
		}
	}
}

// TestObjectKeyOrderTruncatedInput checks a truncated document is an error from the
// streaming pass rather than a silently short order.
func TestObjectKeyOrderTruncatedInput(t *testing.T) {
	for _, raw := range []string{``, `{"properties": {"a": {"type":`, `{"other": [1, 2`} {
		if _, err := objectKeyOrder([]byte(raw), "properties"); err == nil {
			t.Errorf("objectKeyOrder(%q) = nil error, want a parse error", raw)
		}
	}
}

// TestObjectMemberOrderRejectsNonObject checks the member pass refuses a field whose
// value is not an object, naming the field.
func TestObjectMemberOrderRejectsNonObject(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader([]byte(`["a"]`)))
	_, err := objectMemberOrder(dec, "properties")
	if err == nil || !strings.Contains(err.Error(), "properties is not an object") {
		t.Fatalf("objectMemberOrder error = %v, want it to refuse a non-object", err)
	}
}

// TestObjectMemberOrderTruncated checks the member pass reports a parse error when
// the object it is reading ends early, at the key and inside a value alike.
func TestObjectMemberOrderTruncated(t *testing.T) {
	for _, raw := range []string{``, `{"a"`, `{"a": {"b"`} {
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		if _, err := objectMemberOrder(dec, "properties"); err == nil {
			t.Errorf("objectMemberOrder(%q) = nil error, want a parse error", raw)
		}
	}
}

// TestSkipValueDescendsAndReportsTruncation checks the value skipper lands on the
// next sibling for values of every shape, and reports an error rather than silently
// stopping when the value is cut short.
func TestSkipValueDescendsAndReportsTruncation(t *testing.T) {
	whole := []string{`1`, `"s"`, `true`, `null`, `[1,[2,{"a":3}]]`, `{"a":{"b":[1,2]}}`}
	for _, v := range whole {
		raw := `[` + v + `,"next"]`
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		if _, err := dec.Token(); err != nil { // opening bracket
			t.Fatalf("Token: %v", err)
		}
		if err := skipValue(dec); err != nil {
			t.Fatalf("skipValue(%s): %v", v, err)
		}
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("Token after skipValue(%s): %v", v, err)
		}
		if tok != "next" {
			t.Errorf("skipValue(%s) landed on %v, want the next sibling", v, tok)
		}
	}

	truncated := []string{``, `[1, 2`, `{"a": [1`, `{"a"`}
	for _, v := range truncated {
		dec := json.NewDecoder(bytes.NewReader([]byte(v)))
		if err := skipValue(dec); err == nil {
			t.Errorf("skipValue(%q) = nil error, want a parse error", v)
		}
	}
}

// TestEnumNodeRefusesUnparseableValue checks an enum literal that is not JSON is
// reported. Every entry point refuses a schema it cannot read rather than emitting a
// grammar that admits the wrong values.
func TestEnumNodeRefusesUnparseableValue(t *testing.T) {
	if _, err := enumNode([]json.RawMessage{json.RawMessage(`not json`)}); err == nil {
		t.Fatal("expected an error for an unparseable enum value")
	}
	// A single value needs no alternation: it compiles to the bare literal.
	n, err := enumNode([]json.RawMessage{json.RawMessage(`  "only"  `)})
	if err != nil {
		t.Fatalf("enumNode: %v", err)
	}
	l, ok := n.(lit)
	if !ok {
		t.Fatalf("single-value enum compiled to %T, want a literal", n)
	}
	if l.s != `"only"` {
		t.Errorf("enum literal = %s, want the canonical JSON form", l.s)
	}
}

// TestArrayItemSchemaErrorPropagates checks an array whose item schema cannot be
// compiled is refused, rather than falling back to any JSON value.
func TestArrayItemSchemaErrorPropagates(t *testing.T) {
	_, err := Arguments(json.RawMessage(`{"type":"array","items":{"type":"geography"}}`))
	if err == nil {
		t.Fatal("expected refusal of an array with an uncompilable item schema")
	}
	if !strings.Contains(err.Error(), "unsupported schema type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestNestedPropertyErrorNamesTheProperty checks a failure inside a property's schema
// is reported against that property, so an operator can find the offending key.
func TestNestedPropertyErrorNamesTheProperty(t *testing.T) {
	_, err := Arguments(json.RawMessage(`{
      "type":"object",
      "properties":{"where":{"type":"geography"}},
      "additionalProperties":false
    }`))
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), `property "where"`) {
		t.Fatalf("error = %v, want it to name the property", err)
	}
}

// TestUntypedEnumAndUntypedValue checks the two fallbacks: a schema with an enum but
// no type is constrained to those literals, and a schema with neither is constrained
// to well-formed JSON rather than refused.
func TestUntypedEnumAndUntypedValue(t *testing.T) {
	g := mustArguments(t, `{"enum": [1, "two", true, null]}`)
	for _, in := range []string{`1`, `"two"`, `true`, `null`} {
		if !g.Accepts(in) {
			t.Errorf("untyped enum should accept %s", in)
		}
	}
	if g.Accepts(`3`) {
		t.Error("untyped enum must reject a value outside the set")
	}

	free := mustArguments(t, `{}`)
	for _, in := range []string{`{"a":1}`, `[1,2]`, `"s"`, `null`, `-1.5e3`} {
		if !free.Accepts(in) {
			t.Errorf("untyped schema should accept the JSON value %s", in)
		}
	}
	if free.Accepts(`{not json}`) {
		t.Error("untyped schema must still reject text that is not JSON")
	}
}

// TestToolCallOrTextRefusesBadToolSets checks the free-text variant applies exactly
// the same tool-set validation as ToolCall: an empty set, an unnamed tool, a duplicate
// name, and an uncompilable schema are all refused rather than yielding a grammar
// whose text branch would quietly accept anything.
func TestToolCallOrTextRefusesBadToolSets(t *testing.T) {
	cases := []struct {
		name  string
		tools []ToolSchema
		want  string
	}{
		{"empty set", nil, "no tools"},
		{"empty name", []ToolSchema{{Name: "", Schema: json.RawMessage(`{}`)}}, "empty name"},
		{
			name: "duplicate name",
			tools: []ToolSchema{
				{Name: "read", Schema: json.RawMessage(readToolSchema)},
				{Name: "read", Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`)},
			},
			want: "duplicate tool name",
		},
		{
			name:  "uncompilable schema",
			tools: []ToolSchema{{Name: "read", Schema: json.RawMessage(`{"type":"geography"}`)}},
			want:  "unsupported schema type",
		},
		{
			name:  "unparseable schema",
			tools: []ToolSchema{{Name: "read", Schema: json.RawMessage(`{"type":}`)}},
			want:  "parse schema",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ToolCallOrText(tc.tools); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ToolCallOrText error = %v, want it to mention %q", err, tc.want)
			}
			if _, err := ToolCall(tc.tools); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ToolCall error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestToolIsArgumentsOfOneSchema checks the single-tool entry point constrains the
// argument object alone, with no name envelope.
func TestToolIsArgumentsOfOneSchema(t *testing.T) {
	g, err := Tool(json.RawMessage(readToolSchema))
	if err != nil {
		t.Fatalf("Tool: %v", err)
	}
	if !g.Accepts(`{"path":"a.go"}`) {
		t.Error("Tool must accept a valid argument object")
	}
	if g.Accepts(`{"name":"read","arguments":{"path":"a.go"}}`) {
		t.Error("Tool must not accept the call envelope")
	}
}

// TestGrammarRootAndReferences checks the two accessors the well-formedness gate is
// built on: the start rule is reported, and every rule name reachable from the rules
// is listed, sorted.
func TestGrammarRootAndReferences(t *testing.T) {
	g := mustArguments(t, readToolSchema)
	if g.Root() != "root" {
		t.Errorf("Root = %q, want root", g.Root())
	}
	refs := g.References()
	if len(refs) == 0 {
		t.Fatal("References returned nothing for a compiled object grammar")
	}
	for i := 1; i < len(refs); i++ {
		if refs[i-1] >= refs[i] {
			t.Fatalf("References is not sorted: %v", refs)
		}
	}
	for _, name := range refs {
		if _, ok := g.rules[name]; !ok {
			t.Errorf("reference %q has no rule", name)
		}
	}
}

// TestGrammarRejectsDanglingReference checks the builder's finalization guard: a rule
// referencing a name nobody defined is an error, not a grammar the recognizer and the
// runtime would disagree about.
func TestGrammarRejectsDanglingReference(t *testing.T) {
	b := newBuilder()
	b.add("root", ref{"nowhere"})
	if _, err := b.grammar("root"); err == nil {
		t.Fatal("expected an error for a reference with no rule")
	}
}
