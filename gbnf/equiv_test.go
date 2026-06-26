package gbnf

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// The equivalence tests are the core correctness proof. For each schema in the
// corpus they generate many JSON candidates (valid and invalid) and assert the
// compiled grammar accepts a candidate exactly when an independent reference
// validator says the schema admits it. The grammar and the validator are written
// from the schema independently, so agreement across a large corpus is strong
// evidence the emitted grammar means what the schema means. Candidates are
// serialized in declared property order, the order constrained decoding produces.

var corpus = []string{
	readToolSchema,
	`{"type":"object","required":["command"],"properties":{"command":{"type":"string"},"timeout":{"type":"integer"}},"additionalProperties":false}`,
	`{"type":"object","required":["mode"],"properties":{"mode":{"type":"string","enum":["read","write","append"]},"force":{"type":"boolean"}},"additionalProperties":false}`,
	`{"type":"object","required":["tags","meta"],"properties":{"tags":{"type":"array","items":{"type":"string"}},"meta":{"type":"object","required":["k"],"properties":{"k":{"type":"integer"},"note":{"type":"string"}},"additionalProperties":false}},"additionalProperties":false}`,
	`{"type":"object","required":["id"],"properties":{"id":{"type":"string"},"score":{"type":"number"}},"additionalProperties":false}`,
	`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"},"c":{"type":"integer"}},"additionalProperties":false}`,
	`{"type":"object","additionalProperties":true}`,
}

func TestSchemaRecognizerEquivalence(t *testing.T) {
	for si, raw := range corpus {
		var s schemaNode
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatalf("schema %d: parse: %v", si, err)
		}
		g, err := Arguments(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("schema %d: compile: %v", si, err)
		}
		if err := WellFormed(g.String(), g.Root()); err != nil {
			t.Fatalf("schema %d: not well-formed: %v", si, err)
		}
		mismatches := 0
		for seed := range 4000 {
			c := &cursor{data: seedBytes(si, seed)}
			text := genValue(&s, c, 0)
			var v any
			if json.Unmarshal([]byte(text), &v) != nil {
				continue // generator should always emit valid JSON; skip the rare miss
			}
			want := refValid(v, &s)
			got := g.Accepts(text)
			if got != want {
				mismatches++
				if mismatches <= 5 {
					t.Errorf("schema %d: Accepts=%v, refValid=%v for %s", si, got, want, text)
				}
			}
		}
		if mismatches > 0 {
			t.Errorf("schema %d: %d mismatches", si, mismatches)
		}
	}
}

// seedBytes turns two integers into a deterministic byte stream, a fuzz-free,
// repeatable source for the generator that needs no random package.
func seedBytes(a, b int) []byte {
	out := make([]byte, 0, 32)
	x := uint64(a)*0x9E3779B97F4A7C15 + uint64(b)*0xBF58476D1CE4E5B9 + 0x94D049BB133111EB
	for range 32 {
		x ^= x >> 30
		x *= 0xBF58476D1CE4E5B9
		x ^= x >> 27
		out = append(out, byte(x))
	}
	return out
}

// --- reference validator (independent of the grammar) -----------------------

// refValid reports whether the decoded JSON value v satisfies schema s, by the same
// subset rules the compiler constrains. It is written from the schema directly and
// shares no code with the grammar, so it is an independent oracle.
func refValid(v any, s *schemaNode) bool {
	if len(s.Enum) > 0 && s.Type == "" {
		return matchesEnumVal(v, s.Enum)
	}
	switch s.Type {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		// The grammar treats an explicit additionalProperties:true (with no declared
		// properties) as a free-form object, and everything else as closed. The oracle
		// mirrors that contract, not raw JSON Schema's open-by-default.
		if s.AdditionalProperties != nil && *s.AdditionalProperties {
			return true
		}
		for _, r := range s.Required {
			if _, present := m[r]; !present {
				return false
			}
		}
		for k, val := range m {
			ps, declared := s.Properties[k]
			if !declared {
				return false // closed: no undeclared properties
			}
			if !refValid(val, &ps) {
				return false
			}
		}
		return true
	case "array":
		a, ok := v.([]any)
		if !ok {
			return false
		}
		if s.Items == nil {
			return true
		}
		for _, e := range a {
			if !refValid(e, s.Items) {
				return false
			}
		}
		return true
	case "string":
		if _, ok := v.(string); !ok {
			return false
		}
		if len(s.Enum) > 0 {
			return matchesEnumVal(v, s.Enum)
		}
		return true
	case "integer":
		f, ok := v.(float64)
		return ok && !math.IsInf(f, 0) && f == math.Trunc(f)
	case "number":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "":
		return true
	default:
		return false
	}
}

func matchesEnumVal(v any, enum []json.RawMessage) bool {
	cv, err := marshalCanonical(v)
	if err != nil {
		return false
	}
	for _, e := range enum {
		ce, err := canonicalJSON(e)
		if err != nil {
			continue
		}
		if cv == ce {
			return true
		}
	}
	return false
}

// --- candidate generator -----------------------------------------------------

type cursor struct {
	data []byte
	pos  int
}

func (c *cursor) next() byte {
	if len(c.data) == 0 {
		return 0
	}
	b := c.data[c.pos%len(c.data)]
	c.pos++
	return b
}

func (c *cursor) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(c.next()) % n
}

// chance reports true roughly num out of den of the time.
func (c *cursor) chance(num, den int) bool { return c.intn(den) < num }

// genValue emits a JSON candidate guided by schema s. It usually produces a
// schema-shaped value but deliberately deviates (wrong type, dropped required field,
// extra property) so both acceptance and rejection are exercised. The output is
// always syntactically valid JSON in declared property order.
func genValue(s *schemaNode, c *cursor, depth int) string {
	if depth > 4 {
		return genScalar(c)
	}
	if c.chance(1, 6) {
		return genAny(c, depth) // type deviation
	}
	if len(s.Enum) > 0 && s.Type != "object" && s.Type != "array" {
		return genEnum(s, c)
	}
	switch s.Type {
	case "object":
		return genObject(s, c, depth)
	case "array":
		return genArray(s, c, depth)
	case "string":
		return genString(c)
	case "integer":
		return genInteger(c)
	case "number":
		return genNumber(c)
	case "boolean":
		return genBool(c)
	default:
		return genAny(c, depth)
	}
}

func genObject(s *schemaNode, c *cursor, depth int) string {
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	var parts []string
	for _, k := range s.propOrder {
		var include bool
		if required[k] {
			include = !c.chance(1, 8) // occasionally drop a required field (invalid)
		} else {
			include = c.chance(1, 2)
		}
		if !include {
			continue
		}
		ps := s.Properties[k]
		parts = append(parts, jsonKey(k)+":"+genValue(&ps, c, depth+1))
	}
	if c.chance(1, 4) { // sometimes add an undeclared property
		parts = append(parts, jsonKey("zz")+":"+genAny(c, depth+1))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func genArray(s *schemaNode, c *cursor, depth int) string {
	n := c.intn(4)
	parts := make([]string, 0, n)
	for range n {
		if s.Items != nil {
			parts = append(parts, genValue(s.Items, c, depth+1))
		} else {
			parts = append(parts, genAny(c, depth+1))
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func genEnum(s *schemaNode, c *cursor) string {
	if c.chance(1, 4) || len(s.Enum) == 0 { // sometimes an off-enum value
		return genString(c)
	}
	canon, err := canonicalJSON(s.Enum[c.intn(len(s.Enum))])
	if err != nil {
		return genString(c)
	}
	return canon
}

func genAny(c *cursor, depth int) string {
	kinds := 6
	if depth > 3 {
		kinds = 4
	}
	switch c.intn(kinds) {
	case 0:
		return genString(c)
	case 1:
		return genInteger(c)
	case 2:
		return genBool(c)
	case 3:
		return genNumber(c)
	case 4:
		n := c.intn(3)
		parts := make([]string, 0, n)
		for range n {
			parts = append(parts, genAny(c, depth+1))
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		n := c.intn(3)
		parts := make([]string, 0, n)
		for i := range n {
			parts = append(parts, jsonKey("g"+strconv.Itoa(i))+":"+genAny(c, depth+1))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
}

func genScalar(c *cursor) string {
	switch c.intn(4) {
	case 0:
		return genString(c)
	case 1:
		return genInteger(c)
	case 2:
		return genBool(c)
	default:
		return genNumber(c)
	}
}

func genString(c *cursor) string {
	pool := []string{"", "a", "ab", "path/to", "x_1", "read", "write"}
	return `"` + pool[c.intn(len(pool))] + `"`
}

func genInteger(c *cursor) string {
	return strconv.Itoa(c.intn(200) - 100)
}

// genNumber emits a non-integer number, so its value never coincides with an
// integer-valued float and the integer-vs-number distinction stays unambiguous.
func genNumber(c *cursor) string {
	v := c.intn(200) - 100
	return fmt.Sprintf("%d.5", v)
}

func genBool(c *cursor) string {
	if c.chance(1, 2) {
		return "true"
	}
	return "false"
}

func jsonKey(k string) string {
	b, _ := json.Marshal(k)
	return string(b)
}
