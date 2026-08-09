package portregister_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/portregister"
)

// The live gate. Every exported interface in the public band is accounted for in
// docs/HOST_BOUNDARY.md, and the register names no package the tree has lost.
//
// This is the check the register was written to survive. Without it the document is
// a snapshot of one afternoon's audit, and the ports that land afterwards are exactly
// the ones nobody asked the standalone question about.
func TestTheRegisterAndTheTreeAgree(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	findings, err := portregister.Check(root, filepath.Join(root, "docs", "HOST_BOUNDARY.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

// The vocabulary is closed at three verdicts plus gap. A fourth word is not a new
// category, it is a row that obliges nothing: the value of the three is that a reader
// knows what each one commits somebody to.
func TestTheRegisterUsesNoVerdictNobodyDefined(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	reg, err := portregister.ParseRegister(filepath.Join(root, "docs", "HOST_BOUNDARY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Rows) == 0 {
		t.Fatal("parsed no verdict rows at all; the table format has moved and this check is now watching nothing")
	}
	if reg.Verdicts["shipped"] == 0 {
		t.Error("no shipped rows, which cannot be right and means the parse is wrong")
	}
}

// writeTree materializes a synthetic module so the checker's logic can be tested
// against trees the repository does not have.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// check runs the checker over a synthetic tree whose register is the given markdown.
func check(t *testing.T, register string, files map[string]string) []portregister.Finding {
	t.Helper()
	files["docs/REGISTER.md"] = register
	root := writeTree(t, files)
	findings, err := portregister.Check(root, filepath.Join(root, "docs", "REGISTER.md"))
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

const oneRow = "| Deferral | Shipped producer | Wired at | Verdict |\n|---|---|---|---|\n| `widget.Spinner` | `widget.Local` | `cmd/w` | shipped |\n"

// A new interface with no row fails, and the message tells whoever tripped it what
// to write. A checker that said only "missing" would send them to read this package.
func TestANewInterfaceWithNoRowFails(t *testing.T) {
	findings := check(t, oneRow, map[string]string{
		"widget/widget.go": "package widget\ntype Spinner interface{ Spin() }\ntype Wobbler interface{ Wobble() }\n",
	})
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly the one for widget.Wobbler", findings)
	}
	got := findings[0]
	if got.Subject != "widget.Wobbler" {
		t.Errorf("subject = %q, want widget.Wobbler", got.Subject)
	}
	if !strings.Contains(got.Where, "widget/widget.go:3") {
		t.Errorf("where = %q, want the declaration's own position", got.Where)
	}
	for _, want := range []string{"shipped", "justified", "staged", "optional capability", "not-a-deferral"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the message does not offer %q, so it does not say what to do:\n%s", want, got.Detail)
		}
	}
}

// Named anywhere in the document is enough, not only in a row's first cell. Four
// interfaces can be one deferral (the approval stack is), and demanding a row each
// would push the register into a shape that hides the seam it is about.
func TestBeingNamedInTheProseIsEnough(t *testing.T) {
	register := oneRow + "\nThe spinner's own `widget.Brake` is part of the same deferral and is wired with it.\n"
	if findings := check(t, register, map[string]string{
		"widget/widget.go": "package widget\ntype Spinner interface{ Spin() }\ntype Brake interface{ Stop() }\n",
	}); len(findings) != 0 {
		t.Fatalf("findings = %v, want none: the register accounts for both", findings)
	}
}

// An unexported interface, an interface under internal/, and a non-interface type are
// none of them seams a host implements, so none of them need a row.
func TestOnlyThePublicBandsExportedInterfacesCount(t *testing.T) {
	if findings := check(t, oneRow, map[string]string{
		"widget/widget.go":        "package widget\ntype Spinner interface{ Spin() }\ntype wobbler interface{ Wobble() }\ntype Config struct{ N int }\n",
		"internal/plumb/plumb.go": "package plumb\ntype Valve interface{ Open() }\n",
		"widget/widget_test.go":   "package widget\ntype Faker interface{ Fake() }\n",
		"testdata/x/generated.go": "package x\ntype Generated interface{ G() }\n",
	}); len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
}

// The other direction: a row naming a package the tree has lost. This is the drift
// that actually happens, because a rename leaves the prose reading perfectly well.
func TestARowNamingAVanishedPackageFails(t *testing.T) {
	register := oneRow + "| `gizmo.Cranker` | `gizmo.Local` | `cmd/g` | shipped |\n"
	findings := check(t, register, map[string]string{
		"widget/widget.go": "package widget\ntype Spinner interface{ Spin() }\n",
	})
	if len(findings) != 1 || findings[0].Subject != "gizmo.Cranker" {
		t.Fatalf("findings = %v, want the one for the vanished gizmo package", findings)
	}
	if !strings.Contains(findings[0].Detail, "no such package") {
		t.Errorf("the message does not name the problem: %s", findings[0].Detail)
	}
}

// A package whose directory name differs from its package name still resolves, so
// the check reads the package clause rather than guessing from the path.
func TestAPackageIsFoundByItsClauseNotItsDirectory(t *testing.T) {
	register := "| Deferral | Shipped producer | Wired at | Verdict |\n|---|---|---|---|\n| `distil.Maker` | `distil.Model` | `cmd/d` | shipped |\n"
	if findings := check(t, register, map[string]string{
		"memory/distillation/d.go": "package distil\ntype Maker interface{ Make() }\n",
	}); len(findings) != 0 {
		t.Fatalf("findings = %v, want none: distil lives in a directory of another name", findings)
	}
}

// A gap row that says nothing about what to build fails. That is how a gap becomes
// permanent: the row exists, the document looks complete, and nobody is on the hook.
func TestAGapRowHasToSayWhatToBuild(t *testing.T) {
	bare := "| Deferral | Shipped producer | Wired at | Verdict |\n|---|---|---|---|\n| `widget.Spinner` | none | n/a | gap |\n"
	findings := check(t, bare, map[string]string{
		"widget/widget.go": "package widget\ntype Spinner interface{ Spin() }\n",
	})
	if len(findings) != 1 || !strings.Contains(findings[0].Detail, "what to build") {
		t.Fatalf("findings = %v, want the bare gap row refused", findings)
	}

	filled := "| Deferral | Shipped producer | Wired at | Verdict |\n|---|---|---|---|\n" +
		"| `widget.Spinner` | none yet; build one over the local model serving path already in the binary | n/a | gap |\n"
	if findings := check(t, filled, map[string]string{
		"widget/widget.go": "package widget\ntype Spinner interface{ Spin() }\n",
	}); len(findings) != 0 {
		t.Fatalf("findings = %v, want none: the gap says what to build", findings)
	}
}

// moduleRoot walks up from the working directory to the go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// Property: an exported interface in the public band is refused exactly when the
// register does not name it, in any of the three spellings the document uses. Both
// halves have to hold. A checker that only ever refused would be satisfied by a
// register nobody could write, and one that accepted a name it had not seen would
// pass the interface nobody thought about, which is the only case it exists for.
func TestProp_AnInterfaceIsRefusedIffTheRegisterIsSilentAboutIt(t *testing.T) {
	names := []string{"Spinner", "Wobbler", "Brake", "Cranker", "Valve", "Latch"}
	spellings := []string{"canonical", "bare", "import path"}

	rapid.Check(t, func(rt *rapid.T) {
		name := rapid.SampledFrom(names).Draw(rt, "name")
		mention := rapid.Bool().Draw(rt, "mention")
		spelling := rapid.SampledFrom(spellings).Draw(rt, "spelling")

		register := "| Deferral | Shipped producer | Wired at | Verdict |\n|---|---|---|---|\n"
		if mention {
			span := "widget." + name
			switch spelling {
			case "bare":
				span = name
			case "import path":
				span = "sub/widget." + name
			}
			register += "| `" + span + "` | `widget.Local` | `cmd/w` | shipped |\n"
		}
		findings := check(t, register, map[string]string{
			"sub/widget/widget.go": "package widget\ntype " + name + " interface{ Do() }\n",
		})
		// A row naming a package the tree lacks is a finding of the other kind, and the
		// bare spelling names no package at all, so only count the coverage findings.
		var uncovered int
		for _, f := range findings {
			if strings.Contains(f.Detail, "nothing about it in") {
				uncovered++
			}
		}
		if mention && uncovered != 0 {
			t.Fatalf("the register names widget.%s as %q and it was still refused: %v", name, spelling, findings)
		}
		if !mention && uncovered != 1 {
			t.Fatalf("the register is silent about widget.%s and it was not refused: %v", name, findings)
		}
	})
}

// A finding prints as one line naming where, what and why, because that is all
// whoever tripped it sees.
func TestAFindingReadsAsOneLine(t *testing.T) {
	f := portregister.Finding{Subject: "widget.Wobbler", Where: "widget/widget.go:3", Detail: "no row"}
	if got, want := f.String(), "widget/widget.go:3: widget.Wobbler: no row"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// A register that is not there is an error, not an empty register. Silently reading
// no rows would turn a moved or deleted document into a green build, which is the
// one outcome this check must never produce.
func TestAMissingRegisterIsAnError(t *testing.T) {
	root := writeTree(t, map[string]string{"widget/widget.go": "package widget\ntype Spinner interface{ Spin() }\n"})
	if _, err := portregister.ParseRegister(filepath.Join(root, "docs", "nope.md")); err == nil {
		t.Fatal("ParseRegister of a missing file succeeded")
	}
	if _, err := portregister.Check(root, filepath.Join(root, "docs", "nope.md")); err == nil {
		t.Fatal("Check with a missing register succeeded")
	}
}

// A root that is not there is an error, not an empty seam list. Reporting no
// interfaces because the tree could not be walked would pass the check by finding
// nothing to check, which is the failure mode a coverage gate exists to notice.
func TestAMissingTreeIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-tree")
	if _, err := portregister.Seams(missing); err == nil {
		t.Fatal("Seams of a missing tree succeeded")
	}
	if _, err := portregister.Check(missing, filepath.Join(missing, "docs", "R.md")); err == nil {
		t.Fatal("Check over a missing tree succeeded")
	}
}

// A file that does not parse is skipped rather than failing the check. The compiler
// reports it, the build fails on it, and saying so here would only say it twice.
// A parseable file beside it is still read, so one broken file does not blind the gate.
func TestAnUnparseableFileIsSkippedAndItsNeighbourIsStillRead(t *testing.T) {
	findings := check(t, oneRow, map[string]string{
		"widget/widget.go": "package widget\ntype Spinner interface{ Spin() }\n",
		"widget/broken.go": "package widget\nthis is not go at all {{{\n",
	})
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none: the broken file is the compiler's problem", findings)
	}
}

// A pipe-bearing line that is not a table row is prose, not a verdict. The register
// is a document before it is a data structure, and a parser that read every pipe as
// a row would invent rows out of sentences.
func TestAPipeInProseIsNotARow(t *testing.T) {
	register := oneRow + "\nThe spinner is chosen by `widget.Local` | never by the host.\n"
	root := writeTree(t, map[string]string{
		"docs/REGISTER.md": register,
		"widget/widget.go": "package widget\ntype Spinner interface{ Spin() }\n",
	})
	reg, err := portregister.ParseRegister(filepath.Join(root, "docs", "REGISTER.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Rows) != 1 {
		t.Fatalf("rows = %v, want only the real one", reg.Rows)
	}
}

// Findings come back in a stable order, seams by position and register entries by
// subject. A gate whose output reshuffled between runs would make a diff of two
// failures unreadable, which is most of what somebody does with them.
func TestFindingsAreOrdered(t *testing.T) {
	register := oneRow +
		"| `gizmo.Cranker` | `gizmo.Local` | `cmd/g` | shipped |\n" +
		"| `doohickey.Twister` | `doohickey.Local` | `cmd/d` | shipped |\n"
	findings := check(t, register, map[string]string{
		"widget/a_widget.go": "package widget\ntype Spinner interface{ Spin() }\ntype Wobbler interface{ Wobble() }\n",
		"widget/z_widget.go": "package widget\ntype Latch interface{ Shut() }\n",
	})
	if len(findings) < 4 {
		t.Fatalf("findings = %v, want the two unaccounted seams and the two vanished packages", findings)
	}
	for i := 1; i < len(findings); i++ {
		if findings[i-1].Where > findings[i].Where {
			t.Fatalf("findings are not ordered by position: %q then %q", findings[i-1].Where, findings[i].Where)
		}
	}
	// The register's own findings share one Where, so their order is by subject.
	var subjects []string
	for _, f := range findings {
		if !strings.Contains(f.Where, ".go:") {
			subjects = append(subjects, f.Subject)
		}
	}
	if len(subjects) != 2 || subjects[0] > subjects[1] {
		t.Fatalf("register findings = %v, want both, sorted by subject", subjects)
	}
}
