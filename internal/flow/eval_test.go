package flow

import (
	"strings"
	"testing"
)

// opaque is a value type the expression language has no rule for. A flow only ever sees
// JSON-shaped values, but config is a map[string]any a Go host fills, so the coercion
// rules must have a defined answer for a foreign type rather than mis-reading it.
type opaque struct{ n int }

// TestTruthyOfForeignValue proves a value outside the JSON shapes reads as true (it is
// present), and that the JSON shapes read by emptiness.
func TestTruthyOfForeignValue(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"nil", nil, false},
		{"false", false, false},
		{"true", true, true},
		{"zero", float64(0), false},
		{"number", float64(3), true},
		{"empty string", "", false},
		{"string", "x", true},
		{"empty list", []any{}, false},
		{"list", []any{float64(1)}, true},
		{"empty object", map[string]any{}, false},
		{"object", map[string]any{"a": float64(1)}, true},
		{"foreign", opaque{n: 1}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := evalSrc(t, "!!x", map[string]any{"x": c.v})
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != c.want {
				t.Fatalf("truthy(%v) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

// TestDefaultFallsBackOnEmptyOnly proves default() falls back only for null and the empty
// string/collection, so a legitimate false or zero is kept rather than being replaced.
func TestDefaultFallsBackOnEmptyOnly(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want any
	}{
		{"null falls back", nil, "fb"},
		{"empty string falls back", "", "fb"},
		{"empty list falls back", []any{}, "fb"},
		{"empty object falls back", map[string]any{}, "fb"},
		{"false is kept", false, false},
		{"zero is kept", float64(0), float64(0)},
		{"foreign is kept", opaque{}, opaque{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := evalSrc(t, "default(x, 'fb')", map[string]any{"x": c.v})
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != c.want {
				t.Fatalf("default(%v) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

// TestEqualsAcrossShapes proves equality is numeric across numeric types, structural for
// lists and objects, and false across mismatched types rather than coercing.
func TestEqualsAcrossShapes(t *testing.T) {
	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{"int and float", 3, float64(3), true},
		{"int64 and float", int64(3), float64(3), true},
		{"different numbers", float64(3), float64(4), false},
		{"strings", "a", "a", true},
		{"string and number", "3", float64(3), false},
		{"bools", true, true, true},
		{"bool mismatch", true, false, false},
		{"bool and number", true, float64(1), false},
		{"nulls", nil, nil, true},
		{"null and empty string", nil, "", false},
		{"equal lists", []any{float64(1), "a"}, []any{float64(1), "a"}, true},
		{"lists differ in length", []any{float64(1)}, []any{float64(1), float64(2)}, false},
		{"lists differ in element", []any{float64(1)}, []any{float64(2)}, false},
		{"list and object", []any{}, map[string]any{"a": float64(1)}, false},
		{"equal objects", map[string]any{"a": float64(1)}, map[string]any{"a": float64(1)}, true},
		{"objects differ in size", map[string]any{"a": float64(1)}, map[string]any{"a": float64(1), "b": float64(2)}, false},
		{"objects differ in key", map[string]any{"a": float64(1)}, map[string]any{"b": float64(1)}, false},
		{"objects differ in value", map[string]any{"a": float64(1)}, map[string]any{"a": float64(2)}, false},
		{"object and string", map[string]any{}, "", false},
		{"foreign values", opaque{n: 1}, opaque{n: 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := evalSrc(t, "a == b", map[string]any{"a": c.a, "b": c.b})
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != c.want {
				t.Fatalf("%v == %v is %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestOrderingRefusesIncomparableTypes proves an ordering on values with no order fails
// loudly instead of producing a meaningless answer.
func TestOrderingRefusesIncomparableTypes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		vars map[string]any
	}{
		{"number and string", "n < s", map[string]any{"n": float64(1), "s": "a"}},
		{"bools", "a < b", map[string]any{"a": true, "b": false}},
		{"null and number", "x <= n", map[string]any{"x": nil, "n": float64(1)}},
		{"lists", "a > b", map[string]any{"a": []any{}, "b": []any{}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := evalSrc(t, c.src, c.vars)
			if err == nil {
				t.Fatal("expected an ordering on incomparable types to fail")
			}
			if !strings.Contains(err.Error(), "cannot order") {
				t.Fatalf("expected a cannot-order error, got %v", err)
			}
		})
	}
}

// TestOrderingOnMixedNumericTypes proves the Go-built int and int64 a host may put in
// config order against JSON's float64 rather than failing as incomparable.
func TestOrderingOnMixedNumericTypes(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"i < f", false},
		{"i <= f", true},
		{"i > f", false},
		{"i >= f", true},
		{"i64 > f", false},
		{"f < i64", false},
	}
	vars := map[string]any{"i": 3, "i64": int64(3), "f": float64(3)}
	for _, c := range cases {
		got, err := evalSrc(t, c.src, vars)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got != c.want {
			t.Fatalf("%s = %v, want %v", c.src, got, c.want)
		}
	}
}

// TestArithmeticRefusesNonNumbers proves arithmetic on a non-number fails rather than
// coercing, and that division by zero is an error rather than an infinity a later step
// would carry.
func TestArithmeticRefusesNonNumbers(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"subtract a string", "'a' - 1", "needs numbers"},
		{"multiply a bool", "true * 2", "needs numbers"},
		{"divide by a null", "1 / x", "needs numbers"},
		{"divide by zero", "1 / 0", "division by zero"},
		{"negate a string", "-s", "cannot negate"},
	}
	vars := map[string]any{"x": nil, "s": "a"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := evalSrc(t, c.src, vars)
			if err == nil {
				t.Fatalf("%s: expected an error", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: expected %q, got %v", c.src, c.want, err)
			}
		})
	}
}

// TestStringCoercion proves the "+" fallback renders each value the way a template does:
// an integral number without a decimal tail, a bool as its word, a null as nothing.
func TestStringCoercion(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"integral number", float64(42), "42"},
		{"fractional number", 1.5, "1.5"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"null", nil, ""},
		{"string", "s", "s"},
		{"foreign", opaque{n: 7}, "{7}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := evalSrc(t, "'' + v", map[string]any{"v": c.v})
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != c.want {
				t.Fatalf("'' + %v = %q, want %q", c.v, got, c.want)
			}
		})
	}
}

// TestIndexRefusals proves every way an index can be wrong is an error rather than a
// silent null: only a missing object field is null, because optional fields are testable.
func TestIndexRefusals(t *testing.T) {
	vars := map[string]any{
		"obj":  map[string]any{"a": float64(1)},
		"list": []any{"x"},
		"num":  float64(1),
		"null": nil,
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"non-string object key", "obj[1]", "object key must be a string"},
		{"non-integer list index", "list['a']", "list index must be an integer"},
		{"fractional list index", "list[0.5]", "list index must be an integer"},
		{"index past the end", "list[1]", "out of range"},
		{"negative index", "list[0 - 1]", "out of range"},
		{"index into null", "null.a", "cannot index into null"},
		{"index into a number", "num.a", "cannot index into"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := evalSrc(t, c.src, vars)
			if err == nil {
				t.Fatalf("%s: expected an error", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: expected %q, got %v", c.src, c.want, err)
			}
		})
	}
	got, err := evalSrc(t, "obj.missing", vars)
	if err != nil {
		t.Fatalf("a missing object field should be null, got %v", err)
	}
	if got != nil {
		t.Fatalf("a missing object field should be null, got %v", got)
	}
}

// TestBuiltinRefusals proves the whitelisted functions reject a wrong arity or a wrong
// argument type, and that an unknown name is rejected rather than reaching the host.
func TestBuiltinRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"unknown function", "exec('rm -rf /')", "unknown function"},
		{"urlencode arity", "urlencode()", "urlencode expects 1"},
		{"len arity", "len('a', 'b')", "len expects 1"},
		{"len of a bool", "len(true)", "len of bool"},
		{"lower arity", "lower()", "lower expects 1"},
		{"upper arity", "upper()", "upper expects 1"},
		{"trim arity", "trim()", "trim expects 1"},
		{"contains arity", "contains('a')", "contains expects 2"},
		{"join arity", "join(list)", "join expects"},
		{"join of a non-list", "join('a', ',')", "join expects a list"},
		{"default arity", "default('a')", "default expects"},
	}
	vars := map[string]any{"list": []any{"a"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := evalSrc(t, c.src, vars)
			if err == nil {
				t.Fatalf("%s: expected an error", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: expected %q, got %v", c.src, c.want, err)
			}
		})
	}
}

// TestBuiltinResults proves each whitelisted function computes what it claims, including
// len over every shape it accepts.
func TestBuiltinResults(t *testing.T) {
	vars := map[string]any{
		"list": []any{"a", float64(2)},
		"obj":  map[string]any{"a": float64(1)},
		"null": nil,
	}
	cases := []struct {
		src  string
		want any
	}{
		{"urlencode('a b&c')", "a+b%26c"},
		{"len('héllo')", float64(5)},
		{"len(list)", float64(2)},
		{"len(obj)", float64(1)},
		{"len(null)", float64(0)},
		{"lower('AbC')", "abc"},
		{"upper('AbC')", "ABC"},
		{"trim('  a  ')", "a"},
		{"contains('abc', 'bc')", true},
		{"contains('abc', 'z')", false},
		{"join(list, '-')", "a-2"},
	}
	for _, c := range cases {
		got, err := evalSrc(t, c.src, vars)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got != c.want {
			t.Fatalf("%s = %v, want %v", c.src, got, c.want)
		}
	}
}

// TestUnknownReferenceFailsLoudly proves a name the scope does not bind is an error, so a
// typo in a spec cannot silently evaluate to null.
func TestUnknownReferenceFailsLoudly(t *testing.T) {
	_, err := evalSrc(t, "nope", map[string]any{"yes": float64(1)})
	if err == nil || !strings.Contains(err.Error(), "unknown reference") {
		t.Fatalf("expected an unknown-reference error, got %v", err)
	}
}

// TestShortCircuitSkipsTheFailingSide proves && and || do not evaluate the right side once
// the left decides the result, so a guard like `x && x.field` is expressible.
func TestShortCircuitSkipsTheFailingSide(t *testing.T) {
	vars := map[string]any{"x": nil}
	got, err := evalSrc(t, "x && x.field", vars)
	if err != nil {
		t.Fatalf("&& should not evaluate the right side: %v", err)
	}
	if got != false {
		t.Fatalf("want false, got %v", got)
	}
	got, err = evalSrc(t, "!x || x.field", vars)
	if err != nil {
		t.Fatalf("|| should not evaluate the right side: %v", err)
	}
	if got != true {
		t.Fatalf("want true, got %v", got)
	}
}

// TestShortCircuitPropagatesLeftErrors proves an error on the deciding side is surfaced
// rather than swallowed into a false.
func TestShortCircuitPropagatesLeftErrors(t *testing.T) {
	for _, src := range []string{"nope && true", "nope || true", "true && nope", "false || nope"} {
		if _, err := evalSrc(t, src, nil); err == nil {
			t.Fatalf("%s: expected the unknown reference to surface", src)
		}
	}
}

// TestLexStringEscapes proves the string lexer decodes the escapes it documents and keeps
// an unknown escape's literal character, so a spec can carry a quote or a newline.
func TestLexStringEscapes(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{`'a\nb'`, "a\nb"},
		{`'a\tb'`, "a\tb"},
		{`'a\\b'`, `a\b`},
		{`'it\'s'`, "it's"},
		{`"say \"hi\""`, `say "hi"`},
		{`'a\qb'`, "aqb"},
		{`''`, ""},
		{`"double"`, "double"},
	}
	for _, c := range cases {
		got, err := evalSrc(t, c.src, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got != c.want {
			t.Fatalf("%s = %q, want %q", c.src, got, c.want)
		}
	}
}

// TestLexRefusesMalformedSource proves the lexer returns an error rather than panicking or
// silently truncating, which is what keeps a hostile spec from reaching the parser.
func TestLexRefusesMalformedSource(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"unterminated single quote", "'abc", "unterminated string"},
		{"unterminated double quote", `"abc`, "unterminated string"},
		{"dangling escape", `'abc\`, "unterminated string"},
		{"stray rune", "a @ b", "unexpected character"},
		{"stray brace", "{a}", "unexpected character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseExpr(c.src)
			if err == nil {
				t.Fatalf("%s: expected a lex error", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: expected %q, got %v", c.src, c.want, err)
			}
		})
	}
}
