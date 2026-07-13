package gbnf

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWellFormedRejectsMalformedText checks the text-level guard on its own terms:
// each case is grammar text a runtime's parser would choke on, and WellFormed must
// name the problem rather than pass it through.
func TestWellFormedRejectsMalformedText(t *testing.T) {
	cases := []struct {
		name string
		text string
		root string
		want string
	}{
		{
			name: "start rule undefined",
			text: "other ::= \"x\"\n",
			root: "root",
			want: "start rule",
		},
		{
			name: "reference without definition",
			text: "root ::= missing\n",
			root: "root",
			want: "referenced but not defined",
		},
		{
			name: "unbalanced close paren",
			text: "root ::= \"a\" )\n",
			root: "root",
			want: "unbalanced ')'",
		},
		{
			name: "unbalanced open paren",
			text: "root ::= ( \"a\"\n",
			root: "root",
			want: "unbalanced '('",
		},
		{
			name: "unterminated string literal",
			text: "root ::= \"abc\n",
			root: "root",
			want: "unterminated string literal",
		},
		{
			name: "unterminated character class",
			text: "root ::= [a-z\n",
			root: "root",
			want: "unterminated character class",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := WellFormed(tc.text, tc.root)
			if err == nil {
				t.Fatalf("WellFormed(%q) = nil, want an error", tc.text)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("WellFormed error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestWellFormedAcceptsQuotedMetacharacters proves the scanner consumes literals and
// classes whole: a bracket or paren inside a string, and an escaped bracket inside a
// class, are content and not structure, so they neither unbalance the text nor
// register as rule references.
func TestWellFormedAcceptsQuotedMetacharacters(t *testing.T) {
	text := strings.Join([]string{
		`root ::= body`,
		`body ::= "(" "[" "\"" bracket`,
		`bracket ::= [\]()a-z\\]`,
		"",
	}, "\n")
	if err := WellFormed(text, "root"); err != nil {
		t.Fatalf("WellFormed: %v\n%s", err, text)
	}
	defined, referenced, err := scan(text)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, name := range []string{"root", "body", "bracket"} {
		if !defined[name] {
			t.Errorf("rule %q should be recorded as defined", name)
		}
	}
	// Only barewords outside literals and classes are references, and every one of
	// them is also defined here.
	for name := range referenced {
		if !defined[name] {
			t.Errorf("unexpected reference %q from inside a literal or class", name)
		}
	}
}

// TestWellFormedOnCompiledGrammar ties the two halves together: the text a real
// compiled grammar renders passes its own text-level check, and referenced rules all
// resolve.
func TestWellFormedOnCompiledGrammar(t *testing.T) {
	g, err := ToolCallOrText([]ToolSchema{{Name: "read", Schema: json.RawMessage(readToolSchema)}})
	if err != nil {
		t.Fatalf("ToolCallOrText: %v", err)
	}
	if err := WellFormed(g.String(), g.Root()); err != nil {
		t.Fatalf("WellFormed: %v\n%s", err, g.String())
	}
}

// TestIsDefinitionNeedsAssignment checks the definition/reference split directly: a
// name is a definition only when "::=" follows it, whitespace aside.
func TestIsDefinitionNeedsAssignment(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"name ::= x", true},
		{"name\t::= x", true},
		{"name", false},
		{"name x", false},
		{"name :: x", false},
		{"name ::", false},
	}
	for _, tc := range cases {
		r := []rune(tc.text)
		i := strings.Index(tc.text, "name") + len("name")
		if got := isDefinition(r, i); got != tc.want {
			t.Errorf("isDefinition(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
