package resource

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
)

// SchemaCompiler compiles a JSON Schema document (raw JSON) into a reusable
// Validator. It is a port: the substrate ships a small, zero-dependency compiler
// (newBuiltinCompiler) that supports the structural subset of JSON Schema our
// specs use; a host that needs full JSON Schema 2020-12 compliance can inject an
// adapter wrapping a complete engine, without the core depending on it.
type SchemaCompiler interface {
	Compile(schema []byte) (Validator, error)
}

// Validator checks a decoded JSON instance against a compiled schema, returning an
// error describing the first violation.
type Validator interface {
	Validate(instance any) error
}

// builtinCompiler is the default, dependency-free SchemaCompiler. It understands
// the common JSON Schema keywords (type, required, properties, additionalProperties,
// enum, const, minimum/maximum and their exclusive forms, minLength/maxLength,
// pattern, items, minItems/maxItems) and ignores any keyword it does not know, so a
// richer schema still validates (under-constrained) rather than failing. That keeps
// authored schemas forward-compatible with a fuller engine swapped in later.
type builtinCompiler struct{}

func newBuiltinCompiler() SchemaCompiler { return builtinCompiler{} }

// Compile parses a schema document into a Validator.
func (builtinCompiler) Compile(doc []byte) (Validator, error) {
	v, err := decodeInstance(doc)
	if err != nil {
		return nil, fmt.Errorf("schema is not valid JSON: %w", err)
	}
	return compileNode(v)
}

// schema is a compiled schema node.
type schema struct {
	types       []string
	required    []string
	properties  map[string]*schema
	addlAllowed bool
	addlSchema  *schema // non-nil constrains extra properties; used only when addlAllowed
	items       *schema
	enum        []any
	hasConst    bool
	constVal    any
	min, max    *float64
	exclMin     *float64
	exclMax     *float64
	minLen      *int
	maxLen      *int
	pattern     *regexp.Regexp
	minItems    *int
	maxItems    *int
}

func compileNode(doc any) (*schema, error) {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema must be a JSON object")
	}
	s := &schema{addlAllowed: true}

	switch t := m["type"].(type) {
	case string:
		s.types = []string{t}
	case []any:
		for _, x := range t {
			if str, ok := x.(string); ok {
				s.types = append(s.types, str)
			}
		}
	}
	s.required = toStringSlice(m["required"])
	if props, ok := m["properties"].(map[string]any); ok {
		s.properties = make(map[string]*schema, len(props))
		for name, sub := range props {
			cs, err := compileNode(sub)
			if err != nil {
				return nil, fmt.Errorf("properties.%s: %w", name, err)
			}
			s.properties[name] = cs
		}
	}
	switch ap := m["additionalProperties"].(type) {
	case bool:
		s.addlAllowed = ap
	case map[string]any:
		cs, err := compileNode(ap)
		if err != nil {
			return nil, fmt.Errorf("additionalProperties: %w", err)
		}
		s.addlSchema = cs
	}
	if items, ok := m["items"]; ok {
		cs, err := compileNode(items)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		s.items = cs
	}
	if enum, ok := m["enum"].([]any); ok {
		s.enum = enum
	}
	if c, ok := m["const"]; ok {
		s.hasConst, s.constVal = true, c
	}
	s.min = floatPtr(m["minimum"])
	s.max = floatPtr(m["maximum"])
	s.exclMin = floatPtr(m["exclusiveMinimum"])
	s.exclMax = floatPtr(m["exclusiveMaximum"])
	s.minLen = intPtr(m["minLength"])
	s.maxLen = intPtr(m["maxLength"])
	s.minItems = intPtr(m["minItems"])
	s.maxItems = intPtr(m["maxItems"])
	if p, ok := m["pattern"].(string); ok {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		s.pattern = re
	}
	return s, nil
}

// Validate implements Validator.
func (s *schema) Validate(instance any) error { return s.validate("", instance) }

func (s *schema) validate(path string, v any) error {
	at := func(msg string) error {
		if path == "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("%s: %s", path, msg)
	}

	if len(s.types) > 0 && !matchesType(s.types, v) {
		return at(fmt.Sprintf("expected type %s, got %s", strings.Join(s.types, "|"), jsonType(v)))
	}
	if s.hasConst && !reflect.DeepEqual(v, s.constVal) {
		return at("value is not the required constant")
	}
	if len(s.enum) > 0 && !containsValue(s.enum, v) {
		return at("value is not one of the allowed enum values")
	}

	switch tv := v.(type) {
	case map[string]any:
		for _, req := range s.required {
			if _, ok := tv[req]; !ok {
				return at(fmt.Sprintf("missing required property %q", req))
			}
		}
		for name, val := range tv {
			sub, declared := s.properties[name]
			switch {
			case declared:
				if err := sub.validate(join(path, name), val); err != nil {
					return err
				}
			case s.addlSchema != nil:
				if err := s.addlSchema.validate(join(path, name), val); err != nil {
					return err
				}
			case !s.addlAllowed:
				return at(fmt.Sprintf("additional property %q is not allowed", name))
			}
		}
	case []any:
		if s.minItems != nil && len(tv) < *s.minItems {
			return at(fmt.Sprintf("array has %d items, fewer than minItems %d", len(tv), *s.minItems))
		}
		if s.maxItems != nil && len(tv) > *s.maxItems {
			return at(fmt.Sprintf("array has %d items, more than maxItems %d", len(tv), *s.maxItems))
		}
		if s.items != nil {
			for i, el := range tv {
				if err := s.items.validate(fmt.Sprintf("%s[%d]", path, i), el); err != nil {
					return err
				}
			}
		}
	case string:
		n := len([]rune(tv))
		if s.minLen != nil && n < *s.minLen {
			return at(fmt.Sprintf("string length %d is below minLength %d", n, *s.minLen))
		}
		if s.maxLen != nil && n > *s.maxLen {
			return at(fmt.Sprintf("string length %d exceeds maxLength %d", n, *s.maxLen))
		}
		if s.pattern != nil && !s.pattern.MatchString(tv) {
			return at("string does not match the required pattern")
		}
	case float64:
		if s.min != nil && tv < *s.min {
			return at(fmt.Sprintf("%v is below minimum %v", tv, *s.min))
		}
		if s.max != nil && tv > *s.max {
			return at(fmt.Sprintf("%v is above maximum %v", tv, *s.max))
		}
		if s.exclMin != nil && tv <= *s.exclMin {
			return at(fmt.Sprintf("%v is not above exclusiveMinimum %v", tv, *s.exclMin))
		}
		if s.exclMax != nil && tv >= *s.exclMax {
			return at(fmt.Sprintf("%v is not below exclusiveMaximum %v", tv, *s.exclMax))
		}
	}
	return nil
}

// --- helpers ---

func matchesType(types []string, v any) bool {
	jt := jsonType(v)
	for _, t := range types {
		if t == jt {
			return true
		}
		if t == "number" && jt == "integer" { // every integer is a number
			return true
		}
	}
	return false
}

func jsonType(v any) string {
	switch n := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func containsValue(set []any, v any) bool {
	for _, x := range set {
		if reflect.DeepEqual(x, v) {
			return true
		}
	}
	return false
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func floatPtr(v any) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}

func intPtr(v any) *int {
	if f, ok := v.(float64); ok {
		n := int(f)
		return &n
	}
	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
