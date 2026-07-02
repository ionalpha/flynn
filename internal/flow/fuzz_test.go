package flow

import (
	"strings"
	"testing"
)

// FuzzExecFailedFault asserts the failure built for a command that exited nonzero never
// panics, stays bounded however large the command output is, and always names the command
// so the failure is diagnosable. The output a real command produces is unbounded and
// attacker-influenced, so the bound and the no-panic guarantee matter.
func FuzzExecFailedFault(f *testing.F) {
	f.Add("flyctl deploy", "Error: boom", 1)
	f.Add("", "", 0)
	f.Add("x", strings.Repeat("A", 9000), 137)
	f.Fuzz(func(t *testing.T, cmd, output string, exit int) {
		msg := execFailedFault(cmd, ExecResult{ExitCode: exit, Output: output}).Error()
		// Bounded: the command, plus a capped tail of output, plus fixed framing.
		if len(msg) > len(cmd)+maxFaultOutputBytes+128 {
			t.Fatalf("fault message is not bounded: len=%d cmd=%d", len(msg), len(cmd))
		}
		// Diagnosable: a non-empty command is always named in the failure.
		if cmd != "" && !strings.Contains(msg, cmd) {
			t.Fatalf("fault message does not name the command %q: %q", cmd, msg)
		}
	})
}

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
