package flow

import "testing"

// FuzzParseExpr asserts the expression parser never panics, whatever bytes it is
// given: a malformed expression must return an error, not crash. This matters
// because expressions arrive in runtime-authored specs.
func FuzzParseExpr(f *testing.F) {
	for _, s := range []string{
		"1 + 2", "config.a.b[0]", "urlencode(x)", "a && b || !c",
		"", "((((", "'unterminated", "1 2 3", ".", "[]", "a.", "a[",
		"\x00\xff", "1e999", "999999999999999999999999",
	} {
		f.Add(s)
	}
	f.Fuzz(func(_ *testing.T, src string) {
		// Must not panic. A parse error is fine; a successful parse must produce a
		// node that also evaluates without panicking against an empty scope.
		n, err := parseExpr(src)
		if err != nil {
			return
		}
		_, _ = n.eval(newScope(map[string]any{}))
	})
}

// FuzzParseTemplate asserts the template parser never panics on arbitrary input.
func FuzzParseTemplate(f *testing.F) {
	for _, s := range []string{
		"plain", "a {{b}} c", "{{", "}}", "{{}}", "{{ {{ }}", "a {{ urlencode(x) }}",
		"", "{{config.a}}{{config.b}}",
	} {
		f.Add(s)
	}
	f.Fuzz(func(_ *testing.T, src string) {
		tpl, err := parseTemplate(src)
		if err != nil {
			return
		}
		_, _ = tpl.renderValue(newScope(map[string]any{}))
	})
}
