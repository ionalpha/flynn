// Package hostneutral tests that a Flynn contract names only what Flynn owns and can
// verify alone. Anything belonging to the embedding host is an opaque typed ref,
// stored, indexed and matched, never resolved or checked for referential integrity.
//
// Direction decides it. A host concept named in a Flynn contract makes Flynn's
// correctness depend on a record only the host produces: a field called TaskID
// cannot be tested without something that has tasks. Flynn handing work to a host
// through an interface is the intended design.
//
// The gate catches the two ways the boundary breaks.
//
// A denied noun in an exported name. One leak specified memory anchors as "task IDs,
// entity IDs"; Flynn has neither, so the criterion could not be met by Flynn and a
// temp SQLite file. Names are split on camel case and on struct-tag underscores, so
// a serialized task_id is caught with the Go field.
//
// A ref-shaped type that closes its vocabulary. A {Kind, ID} pair is an opaque ref
// only while Kind is a plain string. A named type makes the package enumerate a
// host's record types, and a ref whose kind is not listed stops round-tripping, so
// the gate holds both fields of a ref-shaped struct to string.
//
// Only the public band is scanned, every package outside internal/ without _test.go
// files, because a host depends on it. Tests name host concepts freely to prove a
// ref Flynn cannot resolve is still a valid ref.
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

// Violation is one exported name or ref-shaped type that crosses the boundary. Path
// is module-relative so the message reads the same everywhere.
type Violation struct {
	Path   string
	Line   int
	Name   string
	Reason string
}

// Policy parameterizes the gate so tests run it on synthetic trees; the live gate
// runs DefaultPolicy over the repository.
type Policy struct {
	// DeniedNouns are host-owned record types, lowercase. A name whose camel words or
	// struct-tag words include one is refused.
	DeniedNouns map[string]bool
	// SkipDirs are directory names pruned anywhere in the walk.
	SkipDirs map[string]bool
	// Exempt lists "path:name" pairs that predate the gate. It is empty. Each entry
	// erodes the boundary and outlives the reason it was allowed, so rename the
	// identifier instead.
	Exempt map[string]bool
}

// DefaultPolicy is the live policy.
//
// Every denied word is a host tracker record type Flynn lacks, so a match is a leak.
// Words that collide with ordinary English are left off, because false positives
// force an exemption list and that ends enforcement:
//
//   - "note" is a line of text (govbench.Case.Note) as often as a host document.
//   - "issue" is the verb in credential packages (controlplane.Issue, Issuer).
//   - "area" and "project" are partitioning words; project is a Flynn scope level.
//
// A reviewer's sweep over prose covers those.
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

// Check walks the public band under root and returns every violation in walk order.
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
			// The build reports a file that does not parse.
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

// checkField checks an exported field or method name and the property names in its
// tag, where a denied noun survives a Go rename.
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
// schema in a Go string literal, where host-facing kinds (goal, budget, extension)
// are declared. A schema property is contract like a Go field, and survives every
// rename of the type beside it.
//
// Only a literal parsing as a JSON object with "properties" is checked, so a plain
// string containing a denied word is no finding. Naming an external tool is no leak:
// externagent lists the tools a Claude or Codex process exposes, describing
// something outside Flynn without requiring it.
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

// schemaProperties collects property names at every nesting level of a decoded JSON
// schema.
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

// checkName reports an exported identifier whose camel words include a denied noun.
// Unexported names belong to no contract, so a host never sees them.
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

// refShapedKindType reports a {Kind, ID} ref struct whose Kind is not a plain
// string, returning that field's rendered type. Only a struct with exactly the Kind
// and ID fields qualifies; a larger record with a Kind is left alone.
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

// types renders a type expression for a message; anything else is "a named type".
func types(e ast.Expr) string {
	if ident, ok := e.(*ast.Ident); ok {
		return ident.Name
	}
	return "a named type"
}

// camelWords splits an identifier into lowercase camel-case words, so TaskID yields
// task, id and Issuer yields issuer, never issue. Capital runs stay together (ID,
// HTTPClient), so an initialism does not split into letters.
func camelWords(name string) []string {
	var out []string
	runes := []rune(name)
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := unicode.IsUpper(cur) && !unicode.IsUpper(prev)
		// An initialism running into a new word breaks before its last capital:
		// HTTPServer breaks before S.
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
