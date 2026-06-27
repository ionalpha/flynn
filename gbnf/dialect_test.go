package gbnf

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// ruleNamePattern is the character set a rule name may use in the emitted grammar
// text: letters, digits, and hyphens only. An underscore is not accepted by the
// runtime's grammar parser, so a rendered name that contained one would be rejected.
var ruleNamePattern = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

// TestRenderedGrammarParsesInRuntimeDialect guards the two rendering rules a local
// runtime's grammar parser enforces: a rule name uses only letters, digits, and
// hyphens (no underscore), and no production is empty (an empty match is written as
// the empty-string literal, never a bare right-hand side). A closed object schema
// exercises both, because its internal rule names are built with underscores and its
// object tail terminates with an empty production.
func TestRenderedGrammarParsesInRuntimeDialect(t *testing.T) {
	g, err := ToolCall([]ToolSchema{{Name: "read", Schema: json.RawMessage(readToolSchema)}})
	if err != nil {
		t.Fatalf("ToolCall: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(g.String(), "\n"), "\n") {
		name, rhs, ok := strings.Cut(line, " ::= ")
		if !ok {
			t.Fatalf("rule line is not in `name ::= body` form: %q", line)
		}
		if !ruleNamePattern.MatchString(name) {
			t.Errorf("rule name %q is not in the runtime dialect (letters, digits, hyphens only)", name)
		}
		if strings.TrimSpace(rhs) == "" {
			t.Errorf("rule %q has an empty production; an empty match must render as \"\"", name)
		}
	}
}

func TestToolCallOrTextAcceptsCallAndAnswer(t *testing.T) {
	g, err := ToolCallOrText([]ToolSchema{{Name: "read", Schema: json.RawMessage(readToolSchema)}})
	if err != nil {
		t.Fatalf("ToolCallOrText: %v", err)
	}
	if !g.Accepts(`{"name":"read","arguments":{"path":"a.go"}}`) {
		t.Error("must accept a well-formed tool call")
	}
	if g.Accepts(`{"name":"read","arguments":{}}`) {
		t.Error("must reject a call missing a required argument")
	}
	if !g.Accepts("All three files are present, total 9 lines.") {
		t.Error("must accept a free-text final answer")
	}
	if !g.Accepts("done") {
		t.Error("must accept a short free-text answer")
	}
	// A free-text answer cannot start with "{": that prefix is reserved for the
	// tool-call branch, which keeps the two unambiguous.
	if g.Accepts("{not a real call") {
		t.Error("a free-text answer must not start with \"{\"")
	}
}
