package resource

import (
	"errors"
	"fmt"
	"strings"
)

// Op is a label-selector match operator.
type Op int

const (
	// OpExists matches when the key is present (any value).
	OpExists Op = iota
	// OpNotExists matches when the key is absent.
	OpNotExists
	// OpEquals matches when the key's value equals the requirement value.
	OpEquals
	// OpNotEquals matches when the key is absent or its value differs.
	OpNotEquals
	// OpIn matches when the key's value is one of the requirement values.
	OpIn
	// OpNotIn matches when the key is absent or its value is none of the values.
	OpNotIn
)

// Requirement is one label constraint. Values is empty for existence operators,
// a single element for (in)equality, and a set for In/NotIn.
type Requirement struct {
	Key    string
	Op     Op
	Values []string
}

// matches reports whether labels satisfy this requirement.
func (req Requirement) matches(labels map[string]string) bool {
	v, ok := labels[req.Key]
	switch req.Op {
	case OpExists:
		return ok
	case OpNotExists:
		return !ok
	case OpEquals:
		return ok && len(req.Values) == 1 && v == req.Values[0]
	case OpNotEquals:
		return !ok || len(req.Values) != 1 || v != req.Values[0]
	case OpIn:
		return ok && contains(req.Values, v)
	case OpNotIn:
		return !ok || !contains(req.Values, v)
	default:
		return false
	}
}

// Selector is a conjunction of requirements: a resource matches when its labels
// satisfy every requirement. The empty Selector matches everything. This is the
// universal declarative query plane over all kinds (the Kubernetes label-selector
// model).
type Selector []Requirement

// Matches reports whether the labels satisfy all requirements.
func (s Selector) Matches(labels map[string]string) bool {
	for _, req := range s {
		if !req.matches(labels) {
			return false
		}
	}
	return true
}

// Everything is the selector that matches every resource.
func Everything() Selector { return nil }

// ParseSelector parses a Kubernetes-style label selector: comma-separated
// requirements, each one of `key`, `!key`, `key=value`, `key==value`,
// `key!=value`, `key in (a, b)`, or `key notin (a, b)`. An empty string is
// Everything.
func ParseSelector(s string) (Selector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts, err := splitTopLevel(s)
	if err != nil {
		return nil, err
	}
	sel := make(Selector, 0, len(parts))
	for _, p := range parts {
		req, err := parseRequirement(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		sel = append(sel, req)
	}
	return sel, nil
}

func parseRequirement(p string) (Requirement, error) {
	if p == "" {
		return Requirement{}, errors.New("resource: empty selector requirement")
	}
	// Set operators: "key in (a, b)" / "key notin (a, b)".
	if i := strings.Index(p, "("); i >= 0 {
		if !strings.HasSuffix(p, ")") {
			return Requirement{}, fmt.Errorf("resource: malformed set requirement %q", p)
		}
		head := strings.Fields(p[:i])
		if len(head) != 2 {
			return Requirement{}, fmt.Errorf("resource: malformed set requirement %q", p)
		}
		key, op := head[0], head[1]
		values := parseValues(p[i+1 : len(p)-1])
		switch op {
		case "in":
			return Requirement{Key: key, Op: OpIn, Values: values}, nil
		case "notin":
			return Requirement{Key: key, Op: OpNotIn, Values: values}, nil
		default:
			return Requirement{}, fmt.Errorf("resource: unknown set operator %q", op)
		}
	}
	switch {
	case strings.HasPrefix(p, "!"):
		return Requirement{Key: strings.TrimSpace(p[1:]), Op: OpNotExists}, nil
	case strings.Contains(p, "!="):
		k, v, _ := strings.Cut(p, "!=")
		return Requirement{Key: strings.TrimSpace(k), Op: OpNotEquals, Values: []string{strings.TrimSpace(v)}}, nil
	case strings.Contains(p, "=="):
		k, v, _ := strings.Cut(p, "==")
		return Requirement{Key: strings.TrimSpace(k), Op: OpEquals, Values: []string{strings.TrimSpace(v)}}, nil
	case strings.Contains(p, "="):
		k, v, _ := strings.Cut(p, "=")
		return Requirement{Key: strings.TrimSpace(k), Op: OpEquals, Values: []string{strings.TrimSpace(v)}}, nil
	default:
		return Requirement{Key: p, Op: OpExists}, nil
	}
}

func parseValues(s string) []string {
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// splitTopLevel splits on commas that are not inside parentheses, so the commas
// within a set requirement's value list stay together.
func splitTopLevel(s string) ([]string, error) {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("resource: unbalanced parentheses in selector %q", s)
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("resource: unbalanced parentheses in selector %q", s)
	}
	return append(out, s[start:]), nil
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
