// Package flow is the declarative action interpreter: it lets an extension express
// multi-step behaviour as data instead of compiled code. A Flow is an ordered list
// of steps (http, transform, condition, loop, call, return) the interpreter runs
// against a scope, where steps reference prior outputs and configuration by path.
// Values flow as ordinary decoded JSON (nil, bool, float64, string, []any,
// map[string]any), so a spec the resource store admits is also exactly what runs.
//
// The expression sub-language is deliberately small and side-effect-free: it reads
// the scope and calls a fixed whitelist of pure functions, and nothing else. It
// cannot reach the filesystem, spawn a process, or open a connection. The only
// effects a flow has are its declared http and call steps, both of which go through
// injected ports the host governs at the dispatch boundary. This file is the
// expression lexer and parser; eval.go evaluates the resulting tree.
package flow

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// node is one expression AST node. Evaluation lives in eval.go.
type node interface {
	eval(s *scope) (any, error)
}

// tokenKind enumerates the lexical tokens of the expression language.
type tokenKind int

const (
	tEOF tokenKind = iota
	tNumber
	tString
	tIdent
	tDot
	tLBracket
	tRBracket
	tLParen
	tRParen
	tComma
	tOp // any operator: == != < <= > >= && || ! + - * /
)

type token struct {
	kind tokenKind
	text string
	pos  int
}

// lex turns source into tokens. It never panics: an unterminated string or a stray
// rune is a returned error, so fuzzing the parser cannot crash the process.
func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c >= '0' && c <= '9':
			// A number is digits with at most one decimal point, so a second '.' ends
			// the token and starts a field access (e.g. so "1.2" is a number but
			// "a.1.2" parses as field steps rather than one malformed number).
			j := i
			seenDot := false
			for j < len(src) {
				if isDigit(src[j]) {
					j++
					continue
				}
				if src[j] == '.' && !seenDot {
					seenDot = true
					j++
					continue
				}
				break
			}
			toks = append(toks, token{tNumber, src[i:j], i})
			i = j
		case c == '\'' || c == '"':
			s, n, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tString, s, i})
			i = n
		case c == '_' || isLetter(c):
			j := i
			for j < len(src) && (src[j] == '_' || isLetter(src[j]) || isDigit(src[j])) {
				j++
			}
			toks = append(toks, token{tIdent, src[i:j], i})
			i = j
		case c == '.':
			toks = append(toks, token{tDot, ".", i})
			i++
		case c == '[':
			toks = append(toks, token{tLBracket, "[", i})
			i++
		case c == ']':
			toks = append(toks, token{tRBracket, "]", i})
			i++
		case c == '(':
			toks = append(toks, token{tLParen, "(", i})
			i++
		case c == ')':
			toks = append(toks, token{tRParen, ")", i})
			i++
		case c == ',':
			toks = append(toks, token{tComma, ",", i})
			i++
		default:
			op, n := lexOperator(src, i)
			if n == 0 {
				return nil, fmt.Errorf("flow: unexpected character %q at %d", string(c), i)
			}
			toks = append(toks, token{tOp, op, i})
			i = n
		}
	}
	toks = append(toks, token{tEOF, "", len(src)})
	return toks, nil
}

func lexString(src string, start int) (string, int, error) {
	quote := src[start]
	var b strings.Builder
	i := start + 1
	for i < len(src) {
		c := src[i]
		if c == '\\' && i+1 < len(src) {
			next := src[i+1]
			switch next {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '\\', '\'', '"':
				b.WriteByte(next)
			default:
				b.WriteByte(next)
			}
			i += 2
			continue
		}
		if c == quote {
			return b.String(), i + 1, nil
		}
		b.WriteByte(c)
		i++
	}
	return "", 0, fmt.Errorf("flow: unterminated string at %d", start)
}

// lexOperator matches the longest operator at i, returning the operator text and
// the new index, or n==0 if none matches.
func lexOperator(src string, i int) (string, int) {
	two := ""
	if i+1 < len(src) {
		two = src[i : i+2]
	}
	switch two {
	case "==", "!=", "<=", ">=", "&&", "||":
		return two, i + 2
	}
	switch src[i] {
	case '<', '>', '!', '+', '-', '*', '/':
		return string(src[i]), i + 1
	}
	return "", 0
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return unicode.IsLetter(rune(c)) }

// parser is a precedence-climbing (Pratt) parser over the token stream.
type parser struct {
	toks []token
	pos  int
}

// parseExpr parses a complete expression from source, erroring on trailing tokens
// so a malformed tail is rejected rather than silently ignored.
func parseExpr(src string) (node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.parseBinary(0)
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tEOF {
		return nil, fmt.Errorf("flow: unexpected trailing token %q at %d", p.cur().text, p.cur().pos)
	}
	return n, nil
}

func (p *parser) cur() token     { return p.toks[p.pos] }
func (p *parser) advance() token { t := p.toks[p.pos]; p.pos++; return t }

// binding powers for binary operators, lowest binds loosest.
func bindingPower(op string) int {
	switch op {
	case "||":
		return 1
	case "&&":
		return 2
	case "==", "!=":
		return 3
	case "<", "<=", ">", ">=":
		return 4
	case "+", "-":
		return 5
	case "*", "/":
		return 6
	}
	return 0
}

func (p *parser) parseBinary(minBP int) (node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.cur()
		if t.kind != tOp {
			break
		}
		bp := bindingPower(t.text)
		if bp == 0 || bp < minBP {
			break
		}
		p.advance()
		// Left-associative: the right operand binds tighter than this operator.
		right, err := p.parseBinary(bp + 1)
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: t.text, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (node, error) {
	t := p.cur()
	if t.kind == tOp && (t.text == "!" || t.text == "-") {
		p.advance()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: t.text, operand: operand}, nil
	}
	return p.parsePostfix()
}

// parsePostfix parses a primary then any chain of .field, [index], or a call.
func (p *parser) parsePostfix() (node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch p.cur().kind {
		case tDot:
			p.advance()
			id := p.cur()
			if id.kind != tIdent {
				return nil, fmt.Errorf("flow: expected field name after '.' at %d", id.pos)
			}
			p.advance()
			n = &indexNode{base: n, key: &literalNode{val: id.text}}
		case tLBracket:
			p.advance()
			idx, err := p.parseBinary(0)
			if err != nil {
				return nil, err
			}
			if p.cur().kind != tRBracket {
				return nil, fmt.Errorf("flow: expected ']' at %d", p.cur().pos)
			}
			p.advance()
			n = &indexNode{base: n, key: idx}
		default:
			return n, nil
		}
	}
}

func (p *parser) parsePrimary() (node, error) {
	t := p.cur()
	switch t.kind {
	case tNumber:
		p.advance()
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("flow: bad number %q at %d", t.text, t.pos)
		}
		return &literalNode{val: f}, nil
	case tString:
		p.advance()
		return &literalNode{val: t.text}, nil
	case tIdent:
		p.advance()
		switch t.text {
		case "true":
			return &literalNode{val: true}, nil
		case "false":
			return &literalNode{val: false}, nil
		case "null":
			return &literalNode{val: nil}, nil
		}
		// A call is an identifier immediately followed by '('.
		if p.cur().kind == tLParen {
			return p.parseCall(t.text)
		}
		return &refNode{name: t.text}, nil
	case tLParen:
		p.advance()
		inner, err := p.parseBinary(0)
		if err != nil {
			return nil, err
		}
		if p.cur().kind != tRParen {
			return nil, fmt.Errorf("flow: expected ')' at %d", p.cur().pos)
		}
		p.advance()
		return inner, nil
	default:
		return nil, fmt.Errorf("flow: unexpected token %q at %d", t.text, t.pos)
	}
}

func (p *parser) parseCall(name string) (node, error) {
	p.advance() // consume '('
	var args []node
	if p.cur().kind != tRParen {
		for {
			arg, err := p.parseBinary(0)
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
			if p.cur().kind == tComma {
				p.advance()
				continue
			}
			break
		}
	}
	if p.cur().kind != tRParen {
		return nil, fmt.Errorf("flow: expected ')' to close call %q at %d", name, p.cur().pos)
	}
	p.advance()
	return &callNode{name: name, args: args}, nil
}
