package rigor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// procexecDir is the one package allowed to call the os/exec constructors.
const procexecDir = "internal/procexec/"

// procStartExemptions names the non-test files that start a process without going through
// internal/procexec, each with the reason. The test fails on an entry whose file no longer
// starts a process directly, so the list only shrinks.
var procStartExemptions = map[string]string{
	"internal/hardware/hardware.go":       "runs nvidia-smi, wmic and powershell for GPU and RAM probes; moves to procexec in a follow-up outside the sandbox change",
	"internal/hardware/ram_darwin.go":     "runs sysctl on macOS, where no console window opens; moves to procexec with the rest of internal/hardware",
	"internal/tui/term/editor.go":         "hands the user's own console to their editor, which is the one start that needs the window",
	"internal/conformtest/conformtest.go": "re-executes the test binary inside conformance tests; not a path any command reaches",
}

// procStart is one place a file starts an operating system process.
type procStart struct {
	where string
	what  string
}

// procStarts returns the process starts in src that bypass internal/procexec: a call to an
// os/exec constructor, a composite exec.Cmd, os.StartProcess, syscall.StartProcess or
// syscall.ForkExec, and a Windows CreateProcess call whose function never names
// CREATE_NO_WINDOW. Selectors are resolved through the file's own import names, so a renamed
// import does not hide a start.
func procStarts(t *testing.T, filename string, src any) []procStart {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	names := map[string]string{} // local name -> import path
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		local := filepath.Base(p)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		names[local] = p
	}
	// qualified resolves a pkg.Name selector to its import path and name.
	qualified := func(n ast.Node) (path, name string, ok bool) {
		sel, isSel := n.(*ast.SelectorExpr)
		if !isSel {
			return "", "", false
		}
		id, isID := sel.X.(*ast.Ident)
		if !isID {
			return "", "", false
		}
		path, ok = names[id.Name]
		return path, sel.Sel.Name, ok
	}

	var out []procStart
	record := func(n ast.Node, what string) {
		out = append(out, procStart{where: fset.Position(n.Pos()).String(), what: what})
	}
	// A declaration is the unit a CREATE_NO_WINDOW must share with its CreateProcess call,
	// and it covers a func literal held in a package-level var as well as a function.
	for _, decl := range f.Decls {
		var creates []ast.Node
		hidesWindow := false
		ast.Inspect(decl, func(n ast.Node) bool {
			if lit, ok := n.(*ast.CompositeLit); ok {
				if path, name, ok := qualified(lit.Type); ok && path == "os/exec" && name == "Cmd" {
					record(lit, "exec.Cmd literal")
				}
			}
			path, name, ok := qualified(n)
			if !ok {
				return true
			}
			switch {
			case path == "os/exec" && (name == "Command" || name == "CommandContext"):
				record(n, "exec."+name)
			case path == "os" && name == "StartProcess", path == "syscall" && (name == "StartProcess" || name == "ForkExec"):
				record(n, filepath.Base(path)+"."+name)
			case (path == "golang.org/x/sys/windows" || path == "syscall") && strings.HasPrefix(name, "CreateProcess"):
				creates = append(creates, n)
			case path == "golang.org/x/sys/windows" && name == "CREATE_NO_WINDOW":
				hidesWindow = true
			}
			return true
		})
		if !hidesWindow {
			for _, n := range creates {
				record(n, "CreateProcess without CREATE_NO_WINDOW")
			}
		}
	}
	return out
}

// TestChildProcessesAreStartedThroughProcexec fails when production code starts a process
// without internal/procexec. On Windows a console child of a parent that has no console
// (flynn run from a launcher, a service or an agent's shell) opens a console window of its
// own unless the start passes CREATE_NO_WINDOW; one forgotten start brings the window back,
// so the rule is checked for every file in the module rather than left to review. The scan
// ignores build tags, so a Windows-only file is read on every platform.
func TestChildProcessesAreStartedThroughProcexec(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	found := map[string]bool{}
	var bad []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, procexecDir) {
			return nil
		}
		starts := procStarts(t, path, nil)
		if len(starts) == 0 {
			return nil
		}
		if _, ok := procStartExemptions[rel]; ok {
			found[rel] = true
			return nil
		}
		for _, s := range starts {
			bad = append(bad, s.what+" at "+s.where)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s starts a process without internal/procexec: use procexec.Command or procexec.CommandContext, which set CREATE_NO_WINDOW on Windows (a raw CreateProcess must pass the flag itself)", b)
	}
	for file, reason := range procStartExemptions {
		if reason == "" {
			t.Errorf("exemption %q has no reason", file)
		}
		if !found[file] {
			t.Errorf("exemption %q no longer starts a process directly: remove it", file)
		}
	}
}

// TestProcessStartDetectorSeesRenamedAndRawStarts keeps the guard above honest: an aliased
// import, a composite literal, a raw start and a CreateProcess without the flag are all
// found, and a CreateProcess that names the flag is not.
func TestProcessStartDetectorSeesRenamedAndRawStarts(t *testing.T) {
	src := `package p
import (
	osexec "os/exec"
	"os"
	"golang.org/x/sys/windows"
)
func a() { _ = osexec.Command("x"); _ = osexec.CommandContext(nil, "x") }
func b() { _ = &osexec.Cmd{Path: "x"}; _, _ = os.StartProcess("x", nil, nil) }
func c() { _ = windows.CreateProcess(nil, nil, nil, nil, true, 0, nil, nil, nil, nil) }
func d() { _ = windows.CreateProcess(nil, nil, nil, nil, true, windows.CREATE_NO_WINDOW, nil, nil, nil, nil) }
func e() { _ = os.Getpid() }
`
	got := procStarts(t, "x.go", src)
	var kinds []string
	for _, s := range got {
		kinds = append(kinds, s.what)
	}
	sort.Strings(kinds)
	want := []string{"CreateProcess without CREATE_NO_WINDOW", "exec.Cmd literal", "exec.Command", "exec.CommandContext", "os.StartProcess"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("detector found %q, want %q", kinds, want)
	}
}
