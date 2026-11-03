package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// stubRAM makes [commandLimits] compute its default against a machine with ram bytes of
// physical memory, for the life of the test.
func stubRAM(t *testing.T, ram int64) {
	t.Helper()
	prev := hostRAMBytes
	hostRAMBytes = func(context.Context) int64 { return ram }
	t.Cleanup(func() { hostRAMBytes = prev })
}

// TestCommandLimitsDefaultsToHalfOfRAM pins the default memory cap: half the machine's
// physical memory when no cap was asked for, the asked-for cap when one was, and no cap
// only where the RAM cannot be read.
func TestCommandLimitsDefaultsToHalfOfRAM(t *testing.T) {
	for _, tc := range []struct {
		name               string
		ram                int64
		memoryMiB, maxProc int
		want               sandbox.ResourceLimits
	}{
		{"default is half of RAM", 64 << 30, 0, 0, sandbox.ResourceLimits{MemoryMiB: 32 << 10}},
		{"odd RAM rounds down", 3<<30 + 1, 0, 0, sandbox.ResourceLimits{MemoryMiB: 1536}},
		{"explicit cap wins", 64 << 30, 1024, 0, sandbox.ResourceLimits{MemoryMiB: 1024}},
		{"explicit cap above the default wins", 8 << 30, 6144, 0, sandbox.ResourceLimits{MemoryMiB: 6144}},
		{"negative is treated as unset", 8 << 30, -1, 0, sandbox.ResourceLimits{MemoryMiB: 4096}},
		{"process cap passes through", 8 << 30, 0, 64, sandbox.ResourceLimits{MemoryMiB: 4096, MaxProcesses: 64}},
		{"unknown RAM leaves memory uncapped", 0, 0, 0, sandbox.ResourceLimits{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubRAM(t, tc.ram)
			if got := commandLimits(tc.memoryMiB, tc.maxProc); got != tc.want {
				t.Fatalf("commandLimits(%d, %d) on %d bytes = %+v, want %+v", tc.memoryMiB, tc.maxProc, tc.ram, got, tc.want)
			}
		})
	}
}

// TestCommandLimitsReadsThisMachine checks the real probe feeds the default, so a build
// where hardware.RAMBytes stopped working is caught rather than shipping uncapped
// commands. Every platform the binary ships for can read its RAM.
func TestCommandLimitsReadsThisMachine(t *testing.T) {
	if got := commandLimits(0, 0); got.MemoryMiB <= 0 {
		t.Fatalf("commandLimits(0, 0) = %+v on this machine, want a positive default memory cap", got)
	}
}

// TestEveryShellPolicySetsLimits holds the claim on [commandLimits] that every command the
// agent runs is capped: each governed.ShellPolicy this package builds outside its tests
// must set Limits, so a new entry point cannot start a shell with no memory cap.
func TestEveryShellPolicySetsLimits(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, entry := range entries {
		fname := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(fname, ".go") || strings.HasSuffix(fname, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, fname, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", fname, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ShellPolicy" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "governed" {
				return true
			}
			found++
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Limits" {
						return true
					}
				}
			}
			t.Errorf("%s: governed.ShellPolicy without Limits; set it with commandLimits so the commands it runs have a memory cap", fset.Position(lit.Pos()))
			return true
		})
	}
	if found == 0 {
		t.Fatal("found no governed.ShellPolicy literal; the check is no longer looking at the code that builds the policy")
	}
}
