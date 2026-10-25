package flow

import (
	"fmt"
	"strings"
	"testing"
)

// TestLexTokens pins the token stream for each lexical class, including the
// positions an error message reports and the number/field-access boundary.
func TestLexTokens(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"empty", "", "EOF@0"},
		{"whitespace only", " \t\r\n ", "EOF@5"},
		{"integer", "42", "num:42@0 EOF@2"},
		{"decimal", "1.25", "num:1.25@0 EOF@4"},
		{"second dot ends a number", "1.2.3", "num:1.2@0 dot@3 num:3@4 EOF@5"},
		{"trailing dot stays in the number", "1.", "num:1.@0 EOF@2"},
		{"identifier", "_a1 b", "id:_a1@0 id:b@4 EOF@5"},
		{"path with index", "a.b[0]", "id:a@0 dot@1 id:b@2 lb@3 num:0@4 rb@5 EOF@6"},
		{"call", "f(x, 1)", "id:f@0 lp@1 id:x@2 comma@3 num:1@5 rp@6 EOF@7"},
		{"single quoted string", `'hi'`, "str:hi@0 EOF@4"},
		{"escapes", `"a\nb\t\\\"\q"`, "str:a\nb\t\\\"q@0 EOF@14"},
		{"two char operators", "== != <= >= && ||", "op:==@0 op:!=@3 op:<=@6 op:>=@9 op:&&@12 op:||@15 EOF@17"},
		{"one char operators", "< > ! + - * /", "op:<@0 op:>@2 op:!@4 op:+@6 op:-@8 op:*@10 op:/@12 EOF@13"},
		{"operator then operand", "!a<=2", "op:!@0 id:a@1 op:<=@2 num:2@4 EOF@5"},
		{"lone ampersand is not an operator", "a & b", "ERR"},
		{"unterminated string", `'abc`, "ERR"},
		{"stray rune", "a # b", "ERR"},
	}
	names := map[tokenKind]string{
		tEOF: "EOF", tNumber: "num", tString: "str", tIdent: "id", tDot: "dot",
		tLBracket: "lb", tRBracket: "rb", tLParen: "lp", tRParen: "rp", tComma: "comma", tOp: "op",
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			toks, err := lex(c.src)
			if c.want == "ERR" {
				if err == nil {
					t.Fatalf("lex(%q) = %v, want an error", c.src, toks)
				}
				return
			}
			if err != nil {
				t.Fatalf("lex(%q): %v", c.src, err)
			}
			var parts []string
			for _, tk := range toks {
				label := names[tk.kind]
				if tk.kind == tNumber || tk.kind == tString || tk.kind == tIdent || tk.kind == tOp {
					label += ":" + tk.text
				}
				parts = append(parts, fmt.Sprintf("%s@%d", label, tk.pos))
			}
			if got := strings.Join(parts, " "); got != c.want {
				t.Fatalf("lex(%q) =\n %q\nwant\n %q", c.src, got, c.want)
			}
		})
	}
}

func TestLexErrorsNamePositions(t *testing.T) {
	for src, want := range map[string]string{
		"ab 'x":  "unterminated string at 3",
		"a # b":  `unexpected character "#" at 2`,
		"1 + @":  `unexpected character "@" at 4`,
		`"ok" '`: "unterminated string at 5",
	} {
		_, err := lex(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("lex(%q) error = %v, want it to contain %q", src, err, want)
		}
	}
}
