package flow

import "testing"

// evalSrc parses and evaluates an expression against a flat scope.
func evalSrc(t *testing.T, src string, vars map[string]any) (any, error) {
	t.Helper()
	n, err := parseExpr(src)
	if err != nil {
		return nil, err
	}
	return n.eval(newScope(vars))
}

func TestExprLiteralsAndArithmetic(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"1 + 2", float64(3)},
		{"2 * 3 + 1", float64(7)},
		{"1 + 2 * 3", float64(7)},
		{"(1 + 2) * 3", float64(9)},
		{"10 / 4", float64(2.5)},
		{"-5 + 2", float64(-3)},
		{"'a' + 'b'", "ab"},
		{"true && false", false},
		{"true || false", true},
		{"!false", true},
		{"3 > 2", true},
		{"2 >= 2", true},
		{"'a' < 'b'", true},
		{"1 == 1", true},
		{"1 != 2", true},
	}
	for _, c := range cases {
		got, err := evalSrc(t, c.src, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got != c.want {
			t.Fatalf("%s = %v (%T), want %v", c.src, got, got, c.want)
		}
	}
}

func TestExprPathAndIndex(t *testing.T) {
	vars := map[string]any{
		"config": map[string]any{"q": "hello", "n": float64(2)},
		"steps": map[string]any{
			"fetch": map[string]any{"body": map[string]any{
				"items": []any{
					map[string]any{"name": "a"},
					map[string]any{"name": "b"},
				},
			}},
		},
	}
	got, err := evalSrc(t, "steps.fetch.body.items[1].name", vars)
	if err != nil {
		t.Fatal(err)
	}
	if got != "b" {
		t.Fatalf("got %v", got)
	}
	// A missing field is null, not an error.
	got, err = evalSrc(t, "config.missing", vars)
	if err != nil || got != nil {
		t.Fatalf("missing field: got %v err %v", got, err)
	}
}

func TestExprShortCircuitAvoidsError(t *testing.T) {
	// The right side references an unknown name; && must not evaluate it once the
	// left side is false.
	got, err := evalSrc(t, "false && nope", nil)
	if err != nil {
		t.Fatalf("short circuit should avoid the error: %v", err)
	}
	if got != false {
		t.Fatalf("got %v", got)
	}
}

func TestExprBuiltins(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"urlencode('a b&c')", "a+b%26c"},
		{"len('abc')", float64(3)},
		{"lower('AbC')", "abc"},
		{"upper('AbC')", "ABC"},
		{"trim('  x  ')", "x"},
		{"contains('hello', 'ell')", true},
		{"default('', 'fallback')", "fallback"},
		{"default('set', 'fallback')", "set"},
	}
	for _, c := range cases {
		got, err := evalSrc(t, c.src, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got != c.want {
			t.Fatalf("%s = %v, want %v", c.src, got, c.want)
		}
	}
}

// TestExprCannotReachHost proves the language has no escape: an unknown function is
// an error, not a call into anything. This is the structural guarantee that an
// expression is side-effect-free.
func TestExprCannotReachHost(t *testing.T) {
	for _, src := range []string{"exec('ls')", "open('/etc/passwd')", "system('rm')", "read('x')"} {
		if _, err := evalSrc(t, src, nil); err == nil {
			t.Fatalf("expected %q to be rejected as an unknown function", src)
		}
	}
}

func TestExprErrors(t *testing.T) {
	for _, src := range []string{
		"1 +",     // dangling operator
		"(1 + 2",  // unclosed paren
		"steps[0", // unclosed bracket
		"1 2",     // trailing token
		"'unterminated",
		"unknownRef + 1", // unbound reference
		"1 / 0",          // division by zero
	} {
		if _, err := evalSrc(t, src, nil); err == nil {
			t.Fatalf("expected error for %q", src)
		}
	}
}

func TestExprIndexOutOfRange(t *testing.T) {
	vars := map[string]any{"xs": []any{float64(1)}}
	if _, err := evalSrc(t, "xs[5]", vars); err == nil {
		t.Fatal("expected out-of-range error")
	}
}
