package gbnf

import (
	"errors"
	"fmt"
	"unicode"
)

// WellFormed checks the rendered GBNF text on its own terms, independent of the AST
// it came from: every group and class and string literal is balanced, the start
// rule is defined, and every bareword reference resolves to a defined rule. It scans
// the text a runtime would actually parse, so a rendering bug is caught here rather
// than at the runtime. It returns the first problem found, or nil.
func WellFormed(text, root string) error {
	defined, referenced, err := scan(text)
	if err != nil {
		return err
	}
	if !defined[root] {
		return fmt.Errorf("gbnf: start rule %q is not defined", root)
	}
	for name := range referenced {
		if !defined[name] {
			return fmt.Errorf("gbnf: rule %q is referenced but not defined", name)
		}
	}
	return nil
}

// scan walks GBNF text and returns the set of defined rule names and the set of
// referenced rule names. String literals and character classes are consumed whole
// so their contents are never mistaken for references, and parentheses must balance.
func scan(text string) (defined, referenced map[string]bool, err error) {
	defined = map[string]bool{}
	referenced = map[string]bool{}
	r := []rune(text)
	depth := 0
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case c == '"':
			j, err := skipString(r, i)
			if err != nil {
				return nil, nil, err
			}
			i = j
		case c == '[':
			j, err := skipClass(r, i)
			if err != nil {
				return nil, nil, err
			}
			i = j
		case c == '(':
			depth++
			i++
		case c == ')':
			depth--
			if depth < 0 {
				return nil, nil, errors.New("gbnf: unbalanced ')'")
			}
			i++
		case isIdentStart(c):
			start := i
			for i < len(r) && isIdentPart(r[i]) {
				i++
			}
			name := string(r[start:i])
			if isDefinition(r, i) {
				defined[name] = true
			} else {
				referenced[name] = true
			}
		default:
			i++
		}
	}
	if depth != 0 {
		return nil, nil, errors.New("gbnf: unbalanced '('")
	}
	return defined, referenced, nil
}

// skipString returns the index just past a double-quoted literal beginning at i,
// honoring backslash escapes.
func skipString(r []rune, i int) (int, error) {
	i++ // opening quote
	for i < len(r) {
		switch r[i] {
		case '\\':
			i += 2
		case '"':
			return i + 1, nil
		default:
			i++
		}
	}
	return 0, errors.New("gbnf: unterminated string literal")
}

// skipClass returns the index just past a [..] character class beginning at i,
// honoring backslash escapes.
func skipClass(r []rune, i int) (int, error) {
	i++ // opening bracket
	for i < len(r) {
		switch r[i] {
		case '\\':
			i += 2
		case ']':
			return i + 1, nil
		default:
			i++
		}
	}
	return 0, errors.New("gbnf: unterminated character class")
}

// isDefinition reports whether the identifier ending at i is a rule being defined,
// that is, whether the next non-space tokens are the "::=" assignment.
func isDefinition(r []rune, i int) bool {
	for i < len(r) && (r[i] == ' ' || r[i] == '\t') {
		i++
	}
	return i+2 < len(r) && r[i] == ':' && r[i+1] == ':' && r[i+2] == '='
}

func isIdentStart(c rune) bool {
	return c == '_' || unicode.IsLetter(c)
}

func isIdentPart(c rune) bool {
	return c == '_' || c == '-' || unicode.IsLetter(c) || unicode.IsDigit(c)
}
