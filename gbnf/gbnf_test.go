package gbnf

import (
	"encoding/json"
	"strings"
	"testing"
)

// readToolSchema mirrors the shape the default file-read tool exposes: a closed
// object with one required string and two optional integers.
const readToolSchema = `{
  "type": "object",
  "required": ["path"],
  "properties": {
    "path": {"type": "string"},
    "offset": {"type": "integer"},
    "limit": {"type": "integer"}
  },
  "additionalProperties": false
}`

func mustArguments(t *testing.T, schema string) *Grammar {
	t.Helper()
	g, err := Arguments(json.RawMessage(schema))
	if err != nil {
		t.Fatalf("Arguments: %v", err)
	}
	if err := WellFormed(g.String(), g.Root()); err != nil {
		t.Fatalf("WellFormed: %v\n%s", err, g.String())
	}
	return g
}

func TestArgumentsAcceptsAndRejects(t *testing.T) {
	g := mustArguments(t, readToolSchema)

	accept := []string{
		`{"path":"a.go"}`,
		`{"path":"a.go","offset":1}`,
		`{"path":"a.go","offset":1,"limit":20}`,
		`{ "path" : "a.go" , "offset" : 1 }`, // insignificant whitespace
		`{"path":""}`,
	}
	for _, in := range accept {
		if !g.Accepts(in) {
			t.Errorf("expected accept, rejected: %s", in)
		}
	}

	reject := []string{
		`{}`,                                 // required path missing
		`{"offset":1}`,                       // required path missing
		`{"path":1}`,                         // path must be a string
		`{"path":"a","limit":1.5}`,           // limit must be an integer
		`{"path":"a","extra":true}`,          // closed object, no extras
		`{"path":"a","offset":1,"path":"b"}`, // duplicate handled as out-of-order second path
		`{"limit":20,"path":"a"}`,            // out of declared order
		`{"path":"a"`,                        // unterminated
		`not json`,                           // not an object
		`{"path":"a","offset":1,"limit":20,"x":1}`, // trailing extra on closed object
	}
	for _, in := range reject {
		if g.Accepts(in) {
			t.Errorf("expected reject, accepted: %s", in)
		}
	}
}

func TestEnumConstraint(t *testing.T) {
	g := mustArguments(t, `{
      "type": "object",
      "required": ["mode"],
      "properties": {"mode": {"type": "string", "enum": ["read", "write"]}},
      "additionalProperties": false
    }`)
	if !g.Accepts(`{"mode":"read"}`) {
		t.Error("enum value read should be accepted")
	}
	if !g.Accepts(`{"mode":"write"}`) {
		t.Error("enum value write should be accepted")
	}
	if g.Accepts(`{"mode":"delete"}`) {
		t.Error("off-enum value delete should be rejected")
	}
}

func TestFreeFormOpenObject(t *testing.T) {
	g := mustArguments(t, `{"type": "object", "additionalProperties": true}`)
	accept := []string{`{}`, `{"id":"x"}`, `{"id":"x","extra":42}`, `{"a":true,"b":[1,2],"c":{"d":"e"}}`}
	for _, in := range accept {
		if !g.Accepts(in) {
			t.Errorf("free-form open object should accept: %s", in)
		}
	}
	if g.Accepts(`[1,2]`) {
		t.Error("an array is not an object")
	}
}

func TestOpenObjectWithDeclaredPropertiesIsRefused(t *testing.T) {
	_, err := Arguments(json.RawMessage(`{
      "type": "object",
      "properties": {"id": {"type": "string"}},
      "additionalProperties": true
    }`))
	if err == nil {
		t.Fatal("expected refusal: open object with declared properties cannot be soundly constrained")
	}
	if !strings.Contains(err.Error(), "additionalProperties:true and declared properties") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestArrayAndNestedObject(t *testing.T) {
	g := mustArguments(t, `{
      "type": "object",
      "required": ["tags", "meta"],
      "properties": {
        "tags": {"type": "array", "items": {"type": "string"}},
        "meta": {
          "type": "object",
          "required": ["k"],
          "properties": {"k": {"type": "integer"}},
          "additionalProperties": false
        }
      },
      "additionalProperties": false
    }`)
	accept := []string{
		`{"tags":[],"meta":{"k":1}}`,
		`{"tags":["a"],"meta":{"k":1}}`,
		`{"tags":["a","b","c"],"meta":{"k":-7}}`,
	}
	for _, in := range accept {
		if !g.Accepts(in) {
			t.Errorf("expected accept: %s", in)
		}
	}
	reject := []string{
		`{"tags":[1],"meta":{"k":1}}`,  // array item must be string
		`{"tags":[],"meta":{}}`,        // nested required k missing
		`{"tags":[],"meta":{"k":"x"}}`, // nested k must be integer
		`{"tags":"a","meta":{"k":1}}`,  // tags must be an array
		`{"meta":{"k":1}}`,             // tags required
		`{"tags":[],"meta":{"k":1.5}}`, // nested k must be integer
	}
	for _, in := range reject {
		if g.Accepts(in) {
			t.Errorf("expected reject: %s", in)
		}
	}
}

func TestNoArgumentTool(t *testing.T) {
	g, err := Tool(nil)
	if err != nil {
		t.Fatalf("Tool(nil): %v", err)
	}
	if !g.Accepts(`{}`) {
		t.Error("no-argument tool should accept the empty object")
	}
	if g.Accepts(`{"x":1}`) {
		t.Error("no-argument tool should reject any property")
	}
}

func TestRequiredPropertyNotDeclaredIsRefused(t *testing.T) {
	// "id" is required but absent from properties: its mandatoriness cannot be
	// encoded, so the schema must be refused rather than compiled into a grammar that
	// accepts a call omitting it.
	_, err := Arguments(json.RawMessage(`{"type":"object","required":["id"],"properties":{"name":{"type":"string"}},"additionalProperties":false}`))
	if err == nil {
		t.Fatal("expected refusal: required property not declared in properties")
	}
	if !strings.Contains(err.Error(), "required property") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolCallRefusesMalformedSchema(t *testing.T) {
	_, err := ToolCall([]ToolSchema{{Name: "bad", Schema: json.RawMessage(`{"type":}`)}})
	if err == nil {
		t.Fatal("expected refusal: a tool with an unparseable schema must not yield a permissive grammar")
	}
}

func TestUnsupportedTypeIsError(t *testing.T) {
	_, err := Arguments(json.RawMessage(`{"type":"geography"}`))
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported schema type") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolCallBindsNameToArguments(t *testing.T) {
	g, err := ToolCall([]ToolSchema{
		{Name: "read", Schema: json.RawMessage(readToolSchema)},
		{Name: "noop", Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`)},
	})
	if err != nil {
		t.Fatalf("ToolCall: %v", err)
	}
	if err := WellFormed(g.String(), g.Root()); err != nil {
		t.Fatalf("WellFormed: %v\n%s", err, g.String())
	}
	accept := []string{
		`{"name":"read","arguments":{"path":"a.go"}}`,
		`{"name":"noop","arguments":{}}`,
	}
	for _, in := range accept {
		if !g.Accepts(in) {
			t.Errorf("expected accept: %s", in)
		}
	}
	reject := []string{
		`{"name":"read","arguments":{}}`,           // read requires path
		`{"name":"delete","arguments":{}}`,         // unknown tool name
		`{"name":"noop","arguments":{"path":"a"}}`, // noop takes no arguments
		`{"name":"read","arguments":{"x":1}}`,      // read has no such property and is closed
		`{"arguments":{"path":"a"},"name":"read"}`, // fields out of order
	}
	for _, in := range reject {
		if g.Accepts(in) {
			t.Errorf("expected reject: %s", in)
		}
	}
}

func TestToolCallRejectsEmptySet(t *testing.T) {
	if _, err := ToolCall(nil); err == nil {
		t.Fatal("expected error for empty tool set")
	}
}

// TestAdversarialToolNameEscaped checks that a tool name carrying GBNF and JSON
// metacharacters cannot break out of its literal: the grammar stays well-formed and
// matches only the exact, properly-escaped name, never a crafted variant.
func TestAdversarialToolNameEscaped(t *testing.T) {
	name := "ev\"il\\\nbreak"
	g, err := ToolCall([]ToolSchema{{Name: name, Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}})
	if err != nil {
		t.Fatalf("ToolCall: %v", err)
	}
	if err := WellFormed(g.String(), g.Root()); err != nil {
		t.Fatalf("adversarial name produced malformed grammar: %v\n%s", err, g.String())
	}
	nameJSON, _ := json.Marshal(name)
	good := `{"name":` + string(nameJSON) + `,"arguments":{}}`
	if !g.Accepts(good) {
		t.Errorf("should accept the exact escaped name: %s", good)
	}
	if g.Accepts(`{"name":"evil","arguments":{}}`) {
		t.Error("must not accept a different name")
	}
}

// TestEnumWithMetacharacters checks enum values containing quotes and backslashes
// are matched by their canonical JSON form, not by a grammar that mis-escapes them.
func TestEnumWithMetacharacters(t *testing.T) {
	g := mustArguments(t, `{
      "type": "object",
      "required": ["q"],
      "properties": {"q": {"type": "string", "enum": ["a\"b", "c\\d"]}},
      "additionalProperties": false
    }`)
	if !g.Accepts(`{"q":"a\"b"}`) {
		t.Error(`should accept the enum value a"b`)
	}
	if !g.Accepts(`{"q":"c\\d"}`) {
		t.Error(`should accept the enum value c\d`)
	}
	if g.Accepts(`{"q":"ab"}`) {
		t.Error("must reject a value that is not in the enum")
	}
}

// TestDeeplyNestedSchemaRefused checks a schema nested past the depth limit is
// refused with an error rather than overflowing the stack. A semi-trusted tool
// source could otherwise supply one as a denial-of-service input.
func TestDeeplyNestedSchemaRefused(t *testing.T) {
	var sb strings.Builder
	const depth = maxSchemaDepth + 50
	for range depth {
		sb.WriteString(`{"type":"array","items":`)
	}
	sb.WriteString(`{"type":"string"}`)
	for range depth {
		sb.WriteString(`}`)
	}
	if _, err := Arguments(json.RawMessage(sb.String())); err == nil {
		t.Fatal("expected refusal of a schema nested past the depth limit")
	}
}

// TestOversizedSchemaRefused checks a schema larger than the byte limit is refused
// before it is parsed or compiled.
func TestOversizedSchemaRefused(t *testing.T) {
	big := `{"type":"string","description":"` + strings.Repeat("x", maxSchemaBytes) + `"}`
	if _, err := Arguments(json.RawMessage(big)); err == nil {
		t.Fatal("expected refusal of an oversized schema")
	}
}
