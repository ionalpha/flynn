package flow

import (
	"fmt"
	"strings"
)

// template is a parsed string with embedded {{ expression }} holes. It is the form
// every templated field in a spec takes: a URL, a header value, a JSON body leaf.
// The compile pass (compile.go) parses each templated field exactly once and the
// interpreter renders the parsed form, so evaluating a template N times never
// re-parses it, interpolation stays deterministic, and the expression language is
// the single mechanism for reading the scope.
type template struct {
	parts []templatePart
	// singleExpr is set when the whole template is exactly one {{ expr }} and no
	// surrounding text. Then rendering returns the expression's typed value (a list,
	// a number, an object) instead of its string form, so a body can carry a real
	// JSON value rather than a stringified one.
	singleExpr node
}

type templatePart struct {
	literal string // set when expr is nil
	expr    node   // set for a {{ ... }} hole
}

// parseTemplate compiles a template string. Unbalanced braces are an error, so a
// malformed template is rejected at admission rather than rendering wrong.
func parseTemplate(src string) (*template, error) {
	var parts []templatePart
	i := 0
	for i < len(src) {
		open := strings.Index(src[i:], "{{")
		if open < 0 {
			parts = append(parts, templatePart{literal: src[i:]})
			break
		}
		open += i
		if open > i {
			parts = append(parts, templatePart{literal: src[i:open]})
		}
		closeRel := strings.Index(src[open+2:], "}}")
		if closeRel < 0 {
			return nil, fmt.Errorf("flow: unclosed '{{' at %d", open)
		}
		exprSrc := src[open+2 : open+2+closeRel]
		n, err := parseExpr(strings.TrimSpace(exprSrc))
		if err != nil {
			return nil, err
		}
		parts = append(parts, templatePart{expr: n})
		i = open + 2 + closeRel + 2
	}

	t := &template{parts: parts}
	if len(parts) == 1 && parts[0].expr != nil {
		t.singleExpr = parts[0].expr
	}
	return t, nil
}

// renderValue evaluates the template to a typed value. A whole-template single
// expression yields its raw value; anything with surrounding text yields a string
// (the parts concatenated with each hole rendered via toString).
func (t *template) renderValue(s *scope) (any, error) {
	if t.singleExpr != nil {
		return t.singleExpr.eval(s)
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.expr == nil {
			b.WriteString(p.literal)
			continue
		}
		v, err := p.expr.eval(s)
		if err != nil {
			return nil, err
		}
		b.WriteString(toString(v))
	}
	return b.String(), nil
}

// renderString evaluates the template and renders the result as a string, the form
// a URL, method, or header value needs.
func (t *template) renderString(s *scope) (string, error) {
	v, err := t.renderValue(s)
	if err != nil {
		return "", err
	}
	return toString(v), nil
}
