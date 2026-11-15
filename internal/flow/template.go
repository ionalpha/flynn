package flow

import (
	"fmt"
	"strings"
)

// template is a parsed string with {{ expression }} holes: a URL, a header value, a
// JSON body leaf. compileFlow parses each once, so rendering never re-parses, and
// the expression language is the only way a template reads the scope.
type template struct {
	parts []templatePart
	// singleExpr is set when the template is exactly one {{ expr }}; rendering then
	// returns the typed value, so a body can carry a JSON list or number.
	singleExpr node
}

type templatePart struct {
	literal string // set when expr is nil
	expr    node   // set for a {{ ... }} hole
}

// parseTemplate compiles a template string. Unbalanced braces are an error, so a
// malformed template is rejected at admission.
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

// renderValue renders to a typed value: a single expression yields its raw value,
// anything else a string built with toString.
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

// renderString renders the template as a string, for a URL, method or header.
func (t *template) renderString(s *scope) (string, error) {
	v, err := t.renderValue(s)
	if err != nil {
		return "", err
	}
	return toString(v), nil
}
