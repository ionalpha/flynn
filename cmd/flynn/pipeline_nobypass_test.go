package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// gateFunctions are the trust-gate entry points. A function that reaches model execution
// must call one of these, so classification, provenance, and the containment check are
// never skipped.
var gateFunctions = map[string]bool{
	"admitOnly":      true,
	"classifySource": true,
}

// TestNoModelRunPathSkipsTheGate is the architecture guard for the safe pipeline: no
// function may reach model execution without going through the trust gate. It parses this
// package and asserts two structural invariants by inspecting the call graph, so a future
// edit that adds a serving path which forgets to classify and gate the source fails here
// rather than shipping a hole.
//
//  1. Every caller of serveModel (the function that provisions and serves a model) also
//     calls a gate function, so a model is classified and admitted before it is served.
//  2. The serve manager's Ensure (the call that starts the runtime process in a test
//     with injected provisioning) is reached only from serveModel's helpers, and the
//     public host's Serve (the call that starts it in production) only from serveViaHost,
//     and each helper is called only by serveModel, so there is no second, ungated way to
//     launch a runtime.
func TestNoModelRunPathSkipsTheGate(t *testing.T) {
	// Parse this package's own source (production files only), so the call-graph check
	// reflects exactly what ships. Reading the directory and parsing each file avoids the
	// deprecated whole-directory parser and is enough for a single package's files.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()

	var ensureCallers, hostServeCallers []string
	helperCallers := map[string][]string{}
	var serveModelCallers []string
	gatedServeModelCallers := map[string]bool{}

	for _, entry := range entries {
		fname := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(fname, ".go") || strings.HasSuffix(fname, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, fname, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", fname, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			calls := calledNames(fn.Body)
			name := fn.Name.Name

			if calls["Ensure"] {
				ensureCallers = append(ensureCallers, name)
			}
			if callsHostMethod(fn.Body, "Serve") {
				hostServeCallers = append(hostServeCallers, name)
			}
			for _, helper := range []string{"serveStubbed", "serveViaHost"} {
				if calls[helper] {
					helperCallers[helper] = append(helperCallers[helper], name)
				}
			}
			if calls["serveModel"] && name != "serveModel" {
				serveModelCallers = append(serveModelCallers, name)
				if callsAnyGate(calls) {
					gatedServeModelCallers[name] = true
				}
			}
		}
	}

	if len(serveModelCallers) == 0 {
		t.Fatal("found no callers of serveModel; the architecture guard cannot be relied on, re-check the chokepoint")
	}
	for _, caller := range serveModelCallers {
		if !gatedServeModelCallers[caller] {
			t.Fatalf("%s serves a model without calling the trust gate (one of %v); every model-run path must classify and admit the source first", caller, keys(gateFunctions))
		}
	}

	// The runtime is started only inside serveModel's helpers, so Ensure and the host's Serve
	// must each have exactly one caller, and that caller is reachable only from serveModel. A
	// new caller would be a second path to execution that skips serveModel's gating.
	for _, caller := range ensureCallers {
		if caller != "serveStubbed" {
			t.Fatalf("the serve manager's Ensure is called by %s; it must be reached only through serveModel so no path starts a runtime ungated", caller)
		}
	}
	for _, caller := range hostServeCallers {
		if caller != "serveViaHost" {
			t.Fatalf("the host's Serve is called by %s; it must be reached only through serveModel so no path starts a runtime ungated", caller)
		}
	}
	for _, helper := range []string{"serveStubbed", "serveViaHost"} {
		if got := helperCallers[helper]; len(got) != 1 || got[0] != "serveModel" {
			t.Fatalf("%s is called by %v; only serveModel may call it", helper, got)
		}
	}
}

// calledNames collects the set of function and method names called within a block, by
// the final identifier of the call (a bare name, or the selector for a method call).
func calledNames(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			out[fn.Name] = true
		case *ast.SelectorExpr:
			out[fn.Sel.Name] = true
		}
		return true
	})
	return out
}

// callsHostMethod reports whether a block calls the named method on a field called host, the
// runner's public local-inference host. A bare method name would also match unrelated Serve
// methods elsewhere in the package.
func callsHostMethod(body *ast.BlockStmt, method string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
			if recv, ok := sel.X.(*ast.SelectorExpr); ok && recv.Sel.Name == "host" {
				found = true
			}
		}
		return true
	})
	return found
}

func callsAnyGate(calls map[string]bool) bool {
	for g := range gateFunctions {
		if calls[g] {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
