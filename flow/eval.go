package flow

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// scope is a lexical frame: named values plus an optional parent. The root frame
// holds "config" (the flow inputs) and "steps" (outputs so far); a loop pushes a
// child frame binding its element and index. Lookups walk outward, so an inner
// binding shadows an outer one without mutating it, keeping evaluation free of
// shared state across iterations.
type scope struct {
	vars   map[string]any
	parent *scope
}

func newScope(vars map[string]any) *scope { return &scope{vars: vars} }

func (s *scope) child(vars map[string]any) *scope { return &scope{vars: vars, parent: s} }

func (s *scope) get(name string) (any, bool) {
	for f := s; f != nil; f = f.parent {
		if v, ok := f.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

// literalNode is a constant value.
type literalNode struct{ val any }

func (n *literalNode) eval(*scope) (any, error) { return n.val, nil }

// refNode reads a named value from the scope. An unbound name is an error rather
// than a silent null, so a typo in a spec fails loudly at run time.
type refNode struct{ name string }

func (n *refNode) eval(s *scope) (any, error) {
	v, ok := s.get(n.name)
	if !ok {
		return nil, fmt.Errorf("flow: unknown reference %q", n.name)
	}
	return v, nil
}

// indexNode reads base.key, where key is a field name (string) or a list index
// (number). A missing map field evaluates to null so optional fields are easy to
// test; an out-of-range or non-integer list index is an error.
type indexNode struct {
	base node
	key  node
}

func (n *indexNode) eval(s *scope) (any, error) {
	base, err := n.base.eval(s)
	if err != nil {
		return nil, err
	}
	key, err := n.key.eval(s)
	if err != nil {
		return nil, err
	}
	switch b := base.(type) {
	case map[string]any:
		ks, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("flow: object key must be a string, got %T", key)
		}
		return b[ks], nil // missing field -> nil
	case []any:
		idx, ok := toInt(key)
		if !ok {
			return nil, fmt.Errorf("flow: list index must be an integer, got %T", key)
		}
		if idx < 0 || idx >= len(b) {
			return nil, fmt.Errorf("flow: list index %d out of range (len %d)", idx, len(b))
		}
		return b[idx], nil
	case nil:
		return nil, errors.New("flow: cannot index into null")
	default:
		return nil, fmt.Errorf("flow: cannot index into %T", base)
	}
}

// unaryNode is "!" (logical not) or "-" (numeric negation).
type unaryNode struct {
	op      string
	operand node
}

func (n *unaryNode) eval(s *scope) (any, error) {
	v, err := n.operand.eval(s)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "!":
		return !truthy(v), nil
	case "-":
		f, ok := toNumber(v)
		if !ok {
			return nil, fmt.Errorf("flow: cannot negate %T", v)
		}
		return -f, nil
	}
	return nil, fmt.Errorf("flow: unknown unary operator %q", n.op)
}

// binaryNode is a comparison, boolean, or arithmetic operation. && and || short
// circuit so the right side is not evaluated (or its errors raised) when the left
// already decides the result.
type binaryNode struct {
	op          string
	left, right node
}

func (n *binaryNode) eval(s *scope) (any, error) {
	switch n.op {
	case "&&":
		l, err := n.left.eval(s)
		if err != nil {
			return nil, err
		}
		if !truthy(l) {
			return false, nil
		}
		r, err := n.right.eval(s)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	case "||":
		l, err := n.left.eval(s)
		if err != nil {
			return nil, err
		}
		if truthy(l) {
			return true, nil
		}
		r, err := n.right.eval(s)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	}

	l, err := n.left.eval(s)
	if err != nil {
		return nil, err
	}
	r, err := n.right.eval(s)
	if err != nil {
		return nil, err
	}

	switch n.op {
	case "==":
		return equals(l, r), nil
	case "!=":
		return !equals(l, r), nil
	case "<", "<=", ">", ">=":
		return compare(n.op, l, r)
	case "+":
		return add(l, r)
	case "-", "*", "/":
		return arithmetic(n.op, l, r)
	}
	return nil, fmt.Errorf("flow: unknown operator %q", n.op)
}

// callNode invokes a whitelisted pure function. The whitelist is the entire surface
// the expression language can reach beyond the scope: there is no function that
// touches the host, so an expression is structurally side-effect-free.
type callNode struct {
	name string
	args []node
}

func (n *callNode) eval(s *scope) (any, error) {
	fn, ok := builtins[n.name]
	if !ok {
		return nil, fmt.Errorf("flow: unknown function %q", n.name)
	}
	args := make([]any, len(n.args))
	for i, a := range n.args {
		v, err := a.eval(s)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return fn(args)
}

// builtins is the fixed set of pure functions an expression may call. Every entry
// is deterministic and free of side effects, which is what keeps a runtime-authored
// flow from reaching outside its declared http and call steps.
var builtins = map[string]func(args []any) (any, error){
	"urlencode": func(a []any) (any, error) {
		if len(a) != 1 {
			return nil, errors.New("flow: urlencode expects 1 argument")
		}
		return url.QueryEscape(toString(a[0])), nil
	},
	"len": func(a []any) (any, error) {
		if len(a) != 1 {
			return nil, errors.New("flow: len expects 1 argument")
		}
		switch v := a[0].(type) {
		case string:
			return float64(len([]rune(v))), nil
		case []any:
			return float64(len(v)), nil
		case map[string]any:
			return float64(len(v)), nil
		case nil:
			return float64(0), nil
		default:
			return nil, fmt.Errorf("flow: len of %T", v)
		}
	},
	"lower": func(a []any) (any, error) {
		if len(a) != 1 {
			return nil, errors.New("flow: lower expects 1 argument")
		}
		return strings.ToLower(toString(a[0])), nil
	},
	"upper": func(a []any) (any, error) {
		if len(a) != 1 {
			return nil, errors.New("flow: upper expects 1 argument")
		}
		return strings.ToUpper(toString(a[0])), nil
	},
	"trim": func(a []any) (any, error) {
		if len(a) != 1 {
			return nil, errors.New("flow: trim expects 1 argument")
		}
		return strings.TrimSpace(toString(a[0])), nil
	},
	"contains": func(a []any) (any, error) {
		if len(a) != 2 {
			return nil, errors.New("flow: contains expects 2 arguments")
		}
		return strings.Contains(toString(a[0]), toString(a[1])), nil
	},
	"join": func(a []any) (any, error) {
		if len(a) != 2 {
			return nil, errors.New("flow: join expects (list, sep)")
		}
		list, ok := a[0].([]any)
		if !ok {
			return nil, fmt.Errorf("flow: join expects a list, got %T", a[0])
		}
		parts := make([]string, len(list))
		for i, e := range list {
			parts[i] = toString(e)
		}
		return strings.Join(parts, toString(a[1])), nil
	},
	"default": func(a []any) (any, error) {
		if len(a) != 2 {
			return nil, errors.New("flow: default expects (value, fallback)")
		}
		if isEmpty(a[0]) {
			return a[1], nil
		}
		return a[0], nil
	},
}

// truthy is the boolean reading of a value: false/null/0/""/empty-collection are
// false, everything else is true. It is the rule conditions and && / || use.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		return true
	}
}

// isEmpty reports whether a value is null or an empty string/collection, the test
// the default() function uses to fall back.
func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}

// equals compares two values for equality. Numbers compare numerically; otherwise
// values are equal only when the same type and value. Collections compare by deep
// structural equality so a condition can test a projected object.
func equals(a, b any) bool {
	an, aok := toNumber(a)
	bn, bok := toNumber(b)
	if aok && bok {
		return an == bn
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !equals(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			ov, ok := bv[k]
			if !ok || !equals(v, ov) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// compare orders two values for <, <=, >, >=. Numbers compare numerically, strings
// lexically; a mismatch is an error so an ordering on incomparable types fails
// loudly rather than producing a meaningless result.
func compare(op string, a, b any) (any, error) {
	if an, aok := toNumber(a); aok {
		if bn, bok := toNumber(b); bok {
			return orderResult(op, cmpFloat(an, bn)), nil
		}
	}
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return orderResult(op, strings.Compare(as, bs)), nil
		}
	}
	return nil, fmt.Errorf("flow: cannot order %T and %T", a, b)
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func orderResult(op string, c int) bool {
	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}
	return false
}

// add is "+": numeric addition when both sides are numbers, string concatenation
// otherwise (so a template-style "a" + "b" works). This is the only overloaded
// operator.
func add(a, b any) (any, error) {
	an, aok := toNumber(a)
	bn, bok := toNumber(b)
	if aok && bok {
		return an + bn, nil
	}
	return toString(a) + toString(b), nil
}

// arithmetic is "-", "*", "/" on numbers. Division by zero is an error rather than
// an infinity, so a flow cannot smuggle a NaN downstream.
func arithmetic(op string, a, b any) (any, error) {
	an, aok := toNumber(a)
	bn, bok := toNumber(b)
	if !aok || !bok {
		return nil, fmt.Errorf("flow: %q needs numbers, got %T and %T", op, a, b)
	}
	switch op {
	case "-":
		return an - bn, nil
	case "*":
		return an * bn, nil
	case "/":
		if bn == 0 {
			return nil, errors.New("flow: division by zero")
		}
		return an / bn, nil
	}
	return nil, fmt.Errorf("flow: unknown operator %q", op)
}

// toNumber coerces a value to a float64 if it is numeric. JSON numbers decode as
// float64; the int cases cover values built in Go. A bool is deliberately not a
// number, so arithmetic or an ordering on a bool fails rather than silently
// coercing true to 1.
func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	default:
		return 0, false
	}
}

// toInt coerces a value to an int index, requiring a whole number.
func toInt(v any) (int, bool) {
	f, ok := toNumber(v)
	if !ok || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// toString renders a value for templating and string functions. Numbers print
// without a trailing ".0" when integral, so {{.id}} on 42 yields "42" not "42.0".
func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return strconv.FormatInt(int64(x), 10)
		}
		return fmt.Sprintf("%g", x)
	default:
		return fmt.Sprintf("%v", x)
	}
}
