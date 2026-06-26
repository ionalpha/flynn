// Package gbnf compiles a tool's JSON Schema into a GBNF grammar that a local
// inference runtime applies as a token mask during decoding, so a model cannot
// emit a structurally invalid tool call no matter how small or weak it is. The
// malformed-call failure class is removed by construction, independent of model
// size, because the runtime can only sample tokens the grammar permits.
//
// The package is deliberately self-contained and has an in-process recognizer for
// the same grammar AST it renders. That recognizer is the executable meaning of a
// grammar: tests compile a schema, then check that the grammar accepts exactly the
// JSON values the schema admits, so the emitted text is proven correct here without
// needing a GPU or a running runtime in the loop. Rendering to GBNF text and
// recognizing against the AST share one representation, so what the tests verify is
// what ships to the runtime.
//
// Only the subset of JSON Schema that tool argument schemas actually use is
// supported (objects with typed properties, required lists, enums, arrays, nested
// objects, closed or open). An unsupported construct is a compile error, never a
// silently wrong grammar: refusing is safe, guessing is not.
package gbnf

import (
	"sort"
	"strings"
)

// node is one element of a grammar production. The concrete kinds form a small
// regular-plus-recursion algebra that is enough to describe JSON values: a literal
// string, a character class, a reference to another rule, and the usual
// composition (sequence, alternation, and the three repetitions).
type node interface{ isNode() }

type (
	// lit matches an exact run of runes.
	lit struct{ s string }
	// class matches a single rune that is (or, when negated, is not) in one of the
	// inclusive rune ranges.
	class struct {
		ranges  [][2]rune
		negated bool
	}
	// ref names another rule in the grammar.
	ref struct{ name string }
	// seq matches each child in order.
	seq struct{ items []node }
	// alt matches any one of its children.
	alt struct{ items []node }
	// star matches zero or more repetitions of child.
	star struct{ child node }
	// plus matches one or more repetitions of child.
	plus struct{ child node }
	// opt matches zero or one of child.
	opt struct{ child node }
)

func (lit) isNode()   {}
func (class) isNode() {}
func (ref) isNode()   {}
func (seq) isNode()   {}
func (alt) isNode()   {}
func (star) isNode()  {}
func (plus) isNode()  {}
func (opt) isNode()   {}

// Grammar is a named set of rules with a distinguished root, renderable to GBNF
// text and recognizable in process against the same rules. The zero value is not
// usable; build one through a compile entry point such as Tool or Arguments.
type Grammar struct {
	root  string
	rules map[string]node
	order []string // rule names in a stable render order, root first
}

// String renders the grammar as GBNF text, the form a runtime consumes. Rules are
// emitted root-first then in insertion order, so the output is deterministic and
// diff-stable.
func (g *Grammar) String() string {
	var b strings.Builder
	for i, name := range g.order {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(name)
		b.WriteString(" ::= ")
		b.WriteString(render(g.rules[name]))
	}
	b.WriteByte('\n')
	return b.String()
}

// Root returns the name of the grammar's start rule.
func (g *Grammar) Root() string { return g.root }

// render serializes one node to GBNF, parenthesizing only where precedence
// requires it so the output stays readable.
func render(n node) string {
	switch v := n.(type) {
	case lit:
		return quote(v.s)
	case ref:
		return v.name
	case class:
		var b strings.Builder
		b.WriteByte('[')
		if v.negated {
			b.WriteByte('^')
		}
		for _, r := range v.ranges {
			if r[0] == r[1] {
				b.WriteString(classEscape(r[0]))
			} else {
				b.WriteString(classEscape(r[0]))
				b.WriteByte('-')
				b.WriteString(classEscape(r[1]))
			}
		}
		b.WriteByte(']')
		return b.String()
	case seq:
		parts := make([]string, len(v.items))
		for i, it := range v.items {
			parts[i] = renderInSeq(it)
		}
		return strings.Join(parts, " ")
	case alt:
		parts := make([]string, len(v.items))
		for i, it := range v.items {
			parts[i] = render(it)
		}
		return strings.Join(parts, " | ")
	case star:
		return renderAtom(v.child) + "*"
	case plus:
		return renderAtom(v.child) + "+"
	case opt:
		return renderAtom(v.child) + "?"
	}
	return ""
}

// renderInSeq wraps an alternation inside a sequence, where the bare ` | ` would
// otherwise bind looser than the surrounding concatenation.
func renderInSeq(n node) string {
	if _, ok := n.(alt); ok {
		return "(" + render(n) + ")"
	}
	return render(n)
}

// renderAtom wraps a node so a postfix repetition operator binds to the whole node
// rather than only its last element.
func renderAtom(n node) string {
	switch n.(type) {
	case lit, ref, class:
		return render(n)
	default:
		return "(" + render(n) + ")"
	}
}

// quote renders s as a GBNF double-quoted literal.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// classEscape renders one rune inside a character class, escaping the characters
// that are structural there.
func classEscape(r rune) string {
	switch r {
	case ']':
		return `\]`
	case '\\':
		return `\\`
	case '-':
		return `\-`
	case '^':
		return `\^`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	default:
		return string(r)
	}
}

// References returns the set of rule names referenced anywhere in the grammar,
// sorted. It underpins the well-formedness check that every reference resolves.
func (g *Grammar) References() []string {
	seen := map[string]struct{}{}
	for _, n := range g.rules {
		collectRefs(n, seen)
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func collectRefs(n node, into map[string]struct{}) {
	switch v := n.(type) {
	case ref:
		into[v.name] = struct{}{}
	case seq:
		for _, it := range v.items {
			collectRefs(it, into)
		}
	case alt:
		for _, it := range v.items {
			collectRefs(it, into)
		}
	case star:
		collectRefs(v.child, into)
	case plus:
		collectRefs(v.child, into)
	case opt:
		collectRefs(v.child, into)
	}
}
