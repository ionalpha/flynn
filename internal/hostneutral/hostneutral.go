// Package hostneutral enforces host neutrality as a test rather than a review
// habit: a Flynn contract names only what Flynn owns and can verify on its own.
// Anything belonging to whoever embeds Flynn is carried as an opaque typed ref,
// stored, indexed and matched, never resolved and never validated for referential
// integrity.
//
// The direction is what matters. Flynn naming a host's concept in a contract is
// the leak, because it makes Flynn's correctness depend on a record only the host
// can produce: a field called TaskID cannot be exercised, or even tested, without
// something that has tasks. Flynn handing work to a host through an interface is
// the opposite and is how the whole architecture is meant to work.
//
// The gate is mechanical and has two halves, matching the two ways the boundary
// gives way in practice.
//
// A denied noun in an exported name. The leak that prompted this arrived as a
// design that specified memory anchors as "task IDs, entity IDs" surfaced by "the
// context tools". Flynn has no tasks and no entities, so the criterion could not
// be met by Flynn plus a temp SQLite file. Names are checked component by
// component after splitting on camel case and on the underscores in a struct tag,
// so a serialized property called task_id is caught along with the Go field.
//
// A ref-shaped type that closes its vocabulary. A {Kind, ID} pair works as an
// opaque ref only while Kind stays a plain string. Give it a named type and the
// package acquires an opinion about which kinds exist, which is a host's opinion:
// the enum has to enumerate somebody else's record types, and a ref whose kind is
// not on the list stops round-tripping. The gate holds both halves of every
// ref-shaped struct to string.
//
// Only the public band is scanned, because that is the surface a host depends on:
// every package outside internal/, ignoring _test.go files. Test code names host
// concepts freely, which is how a suite proves a ref to something Flynn cannot
// resolve is still a perfectly good ref.
package hostneutral

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode"
)

// Violation is one exported name, or one ref-shaped type, that crosses the
// boundary. Path is module-relative so the message reads the same everywhere.
type Violation struct {
	Path   string
	Line   int
	Name   string
	Reason string
}

// Policy parameterizes the gate so its logic can be tested against synthetic
// trees while the live gate runs DefaultPolicy over the repository.
type Policy struct {
	// DeniedNouns are the record types a host owns, lowercase. A name whose camel
	// components (or whose struct-tag words) include one of these is refused.
	DeniedNouns map[string]bool
	// SkipDirs are directory names pruned anywhere in the walk.
	SkipDirs map[string]bool
	// Exempt lists "path:name" pairs that predate the gate. It exists so the gate
	// can land over a tree it did not grow up with. It is empty, and adding to it
	// is how the boundary erodes: the entry outlives the memory of why it was
	// allowed. Prefer renaming the identifier.
	Exempt map[string]bool
}

// DefaultPolicy is the live policy.
//
// The denied list is deliberately short. Every word on it is a record type in
// somebody's tracker and is not something Flynn has, so a match is a leak rather
// than a coincidence. Words that would read as leaks but collide with ordinary
// English are left off on purpose, because a gate with false positives gets an
// exemption list and an exemption list is how the rule stops being enforced:
//
//   - "note" is a plain noun for a line of text (govbench.Case.Note is one) as
//     often as it is a host's document type.
//   - "issue" is the verb in every credential package that mints something
//     (controlplane.Issue, Issuer).
//   - "area" and "project" are ordinary partitioning words, and project is one of
//     Flynn's own scope levels, defined in state.Scope in Flynn's own terms.
//
// Those are covered by the sweep a reviewer does over prose, not by this.
func DefaultPolicy() Policy {
	return Policy{
		DeniedNouns: map[string]bool{
			"task": true, "subtask": true, "epic": true, "sprint": true,
			"backlog": true, "kanban": true, "milestone": true, "ticket": true,
			"entity": true, "cortex": true, "ionalpha": true,
		},
		SkipDirs: map[string]bool{
			".git": true, ".worktrees": true, "testdata": true,
			"vendor": true, "node_modules": true, "internal": true,
		},
		Exempt: map[string]bool{},
	}
}

// Check walks the public band under root and returns every violation, in walk
// order.
func Check(root string, policy Policy) ([]Violation, error) {
	var out []Violation
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if policy.SkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			// A file that does not parse is the compiler's problem, not the
			// boundary's; the build fails on it either way.
			return nil //nolint:nilerr // parse errors are reported by the build
		}
		out = append(out, checkFile(fset, file, rel, policy)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkFile collects the violations in one parsed file.
func checkFile(fset *token.FileSet, file *ast.File, rel string, policy Policy) []Violation {
	var out []Violation
	report := func(pos token.Pos, name, reason string) {
		if policy.Exempt[rel+":"+name] {
			return
		}
		out = append(out, Violation{Path: rel, Line: fset.Position(pos).Line, Name: name, Reason: reason})
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			checkName(node.Name, "function or method", policy, report)
		case *ast.TypeSpec:
			checkName(node.Name, "type", policy, report)
		case *ast.ValueSpec:
			for _, id := range node.Names {
				checkName(id, "constant or variable", policy, report)
			}
		case *ast.Field:
			checkField(node, policy, report)
		case *ast.BasicLit:
			checkSchemaLiteral(node, policy, report)
		case *ast.StructType:
			if name, ok := refShapedKindType(node); ok {
				report(node.Pos(), name,
					"a ref-shaped {Kind, ID} struct must keep both halves plain strings; a named Kind type closes the vocabulary to the kinds Flynn happens to know, which is the host's list to hold")
			}
		}
		return true
	})
	return out
}

// checkField reports on an exported struct field or interface method name and on
// the serialized property names in its tag, which is where a denied noun survives
// a Go rename.
func checkField(f *ast.Field, policy Policy, report func(token.Pos, string, string)) {
	for _, id := range f.Names {
		checkName(id, "field or interface method", policy, report)
	}
	if f.Tag == nil {
		return
	}
	tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
	for _, key := range []string{"json", "cbor", "yaml", "db"} {
		value, ok := tag.Lookup(key)
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(value, ",")
		for _, word := range strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
			if policy.DeniedNouns[strings.ToLower(word)] {
				report(f.Tag.Pos(), name, "serialized property names "+strings.ToLower(word)+", a record type a host owns and Flynn cannot resolve")
			}
		}
	}
}

// checkSchemaLiteral reports a denied noun among the property names of a JSON
// schema written as a Go string literal, which is how the kinds a host writes
// resources against are declared (goal, budget, extension). A schema property is
// contract surface as much as a Go field is, and it survives every rename of the
// Go type beside it.
//
// The literal has to parse as a JSON object carrying "properties" to be looked at
// at all, so an ordinary string that happens to contain a denied word is not a
// finding. Naming an external tool is not a leak either: externagent lists the
// tools a Claude or Codex process exposes, which is Flynn describing something
// outside it rather than requiring it.
func checkSchemaLiteral(lit *ast.BasicLit, policy Policy, report func(token.Pos, string, string)) {
	if lit.Kind != token.STRING || !strings.Contains(lit.Value, `"properties"`) {
		return
	}
	text := lit.Value
	if strings.HasPrefix(text, "`") {
		text = strings.Trim(text, "`")
	} else if unquoted, err := strconv.Unquote(text); err == nil {
		text = unquoted
	}
	var doc any
	if json.Unmarshal([]byte(text), &doc) != nil {
		return
	}
	for _, name := range schemaProperties(doc) {
		for _, word := range strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
			if policy.DeniedNouns[strings.ToLower(word)] {
				report(lit.Pos(), name, "schema property names "+strings.ToLower(word)+
					", a record type a host owns and Flynn cannot resolve")
			}
		}
	}
}

// schemaProperties collects the property names declared anywhere in a decoded
// JSON schema, at every nesting level.
func schemaProperties(node any) []string {
	var out []string
	switch n := node.(type) {
	case map[string]any:
		if props, ok := n["properties"].(map[string]any); ok {
			for name := range props {
				out = append(out, name)
			}
		}
		for _, v := range n {
			out = append(out, schemaProperties(v)...)
		}
	case []any:
		for _, v := range n {
			out = append(out, schemaProperties(v)...)
		}
	}
	return out
}

// checkName reports an exported identifier whose camel components include a
// denied noun. Unexported names are the package's own business: they are not part
// of any contract and renaming them changes nothing a host can see.
func checkName(id *ast.Ident, kind string, policy Policy, report func(token.Pos, string, string)) {
	if id == nil || !id.IsExported() {
		return
	}
	for _, word := range camelWords(id.Name) {
		if policy.DeniedNouns[word] {
			report(id.Pos(), id.Name, kind+" names "+word+
				", a record type a host owns and Flynn cannot resolve; carry it as an opaque typed ref instead")
			return
		}
	}
}

// refShapedKindType reports a struct that is a {Kind, ID} ref whose Kind is not a
// plain string, returning the offending field's rendered type. A struct qualifies
// as ref-shaped when it declares both a Kind and an ID field and nothing else
// beyond them, which is the shape the boundary rule is about; a large record that
// happens to have a Kind is a different thing and is left alone.
func refShapedKindType(s *ast.StructType) (string, bool) {
	if s.Fields == nil || len(s.Fields.List) != 2 {
		return "", false
	}
	var kind ast.Expr
	seen := map[string]bool{}
	for _, f := range s.Fields.List {
		if len(f.Names) != 1 {
			return "", false
		}
		name := f.Names[0].Name
		seen[name] = true
		if name == "Kind" {
			kind = f.Type
		}
	}
	if !seen["Kind"] || !seen["ID"] {
		return "", false
	}
	if ident, ok := kind.(*ast.Ident); ok && ident.Name == "string" {
		return "", false
	}
	return "Kind " + types(kind), true
}

// types renders a type expression well enough for a message. Anything it cannot
// name is reported as "a named type", which is the part that matters.
func types(e ast.Expr) string {
	if ident, ok := e.(*ast.Ident); ok {
		return ident.Name
	}
	return "a named type"
}

// camelWords splits an identifier into lowercase words on camel-case boundaries,
// so TaskID yields task, id and Issuer yields issuer rather than issue. Runs of
// capitals stay together (ID, HTTPClient), which is what keeps an initialism from
// shattering into letters that match nothing.
func camelWords(name string) []string {
	var out []string
	runes := []rune(name)
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := unicode.IsUpper(cur) && !unicode.IsUpper(prev)
		// The end of an initialism that runs into a new word: HTTPServer breaks
		// before S, not before P.
		if unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
			boundary = true
		}
		if boundary {
			out = append(out, strings.ToLower(string(runes[start:i])))
			start = i
		}
	}
	out = append(out, strings.ToLower(string(runes[start:])))
	return out
}
