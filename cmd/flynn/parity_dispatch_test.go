package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// dispatchers names the function that routes each parent command's subcommands.
var dispatchers = map[string]string{
	"auth":         "runAuth",
	"db":           "runDB",
	"deps":         "runDeps",
	"extensions":   "runExtensions",
	"integrations": "runIntegrations",
	"mcp":          "runMCP",
	"memory":       "dispatchMemory",
	"models":       "dispatchModels",
	"playbook":     "runPlaybook",
	"sandbox":      "sandboxCommand",
	"services":     "runServices",
	"skill":        "dispatchSkill",
	"spine":        "dispatchSpine",
	"version":      "runVersion",
}

// TestUsageAndDispatchAgree reads each dispatcher's source and requires the words it
// routes on to be the subcommands the help pages list, in both directions.
func TestUsageAndDispatchAgree(t *testing.T) {
	routed := map[string][]string{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil {
				routed[fn.Name.Name] = routedWords(fn)
			}
		}
	}
	for _, c := range cliCommands() {
		if len(c.subs) == 0 {
			continue
		}
		words, ok := routed[dispatchers[c.name]]
		if !ok {
			t.Errorf("%s has subcommands and no dispatcher named in this test", c.name)
			continue
		}
		var listed []string
		for _, s := range c.subs {
			listed = append(listed, s.name)
			listed = append(listed, s.aliases...)
			if !slices.Contains(words, s.name) {
				t.Errorf("help lists `%s %s` and %s does not route it", c.name, s.name, dispatchers[c.name])
			}
			for _, a := range s.aliases {
				if !slices.Contains(words, a) {
					t.Errorf("help lists `%s %s` as an alias and %s does not route it", c.name, a, dispatchers[c.name])
				}
			}
		}
		for _, w := range words {
			if !slices.Contains(listed, w) {
				t.Errorf("%s routes `%s %s` and help does not list it", dispatchers[c.name], c.name, w)
			}
		}
	}
}

// routedWords are the string literals fn compares its subcommand word (args[0] or sub)
// against, in a switch or a comparison.
func routedWords(fn *ast.FuncDecl) []string {
	var words []string
	isWord := func(e ast.Expr) bool {
		if id, ok := e.(*ast.Ident); ok {
			return id.Name == "sub"
		}
		ix, ok := e.(*ast.IndexExpr)
		if !ok {
			return false
		}
		x, xok := ix.X.(*ast.Ident)
		i, iok := ix.Index.(*ast.BasicLit)
		return xok && iok && (x.Name == "args" || x.Name == "sub") && i.Value == "0"
	}
	add := func(e ast.Expr) {
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil && s != "" {
				words = append(words, s)
			}
		}
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SwitchStmt:
			if n.Tag == nil || !isWord(n.Tag) {
				return true
			}
			for _, s := range n.Body.List {
				if cc, ok := s.(*ast.CaseClause); ok {
					for _, e := range cc.List {
						add(e)
					}
				}
			}
		case *ast.BinaryExpr:
			if (n.Op == token.EQL || n.Op == token.NEQ) && isWord(n.X) {
				add(n.Y)
			}
		}
		return true
	})
	return words
}
