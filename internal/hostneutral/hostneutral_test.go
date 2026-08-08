package hostneutral

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestHostNeutrality is the gate. It fails when a package outside internal/ names
// a host's record type in an exported identifier or a serialized property, or
// when a ref-shaped {Kind, ID} type closes its vocabulary with a named Kind.
// Running `go test ./...` therefore enforces the boundary with no extra wiring.
func TestHostNeutrality(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Skipf("hostneutral: %v (skipping outside a source checkout)", err)
	}
	vs, err := Check(root, DefaultPolicy())
	if err != nil {
		t.Fatalf("hostneutral check: %v", err)
	}
	for _, v := range vs {
		t.Errorf("%s:%d: %s: %s", v.Path, v.Line, v.Name, v.Reason)
	}
	if len(vs) > 0 {
		t.Logf("%d host-neutrality violation(s). A Flynn contract names only what Flynn owns: carry the host's record as an opaque {Kind, ID} ref, and rename the identifier.", len(vs))
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

// writeTree lays a synthetic source tree out under a temp root and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		if err := writeFile(root, name, body); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// writeFile writes one slash-separated path under root, creating its directories.
// It takes no testing.TB so the property can call it with a rapid.T in hand.
func writeFile(root, name, body string) error {
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

func TestCheckCatchesTheLeaks(t *testing.T) {
	root := writeTree(t, map[string]string{
		"memory/leak.go": `package memory

// TaskID is the exported name that started this.
type TaskID string

type Item struct {
	EpicName string
	Ref      string ` + "`json:\"task_id\"`" + `
	fine     string
}

func SprintPlan() {}

type Anchor struct {
	Kind AnchorKind
	ID   string
}
`,
	})

	vs, err := Check(root, DefaultPolicy())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	want := map[string]bool{
		"TaskID":          true, // exported type
		"EpicName":        true, // exported field
		"task_id":         true, // serialized property
		"SprintPlan":      true, // exported func
		"Kind AnchorKind": true, // ref-shaped type with a closed kind
	}
	got := map[string]bool{}
	for _, v := range vs {
		got[v.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("no violation reported for %q; got %v", name, keys(got))
		}
	}
	if len(got) != len(want) {
		t.Errorf("reported %v, want exactly %v", keys(got), keys(want))
	}
}

func TestCheckLeavesTheInnocentAlone(t *testing.T) {
	root := writeTree(t, map[string]string{
		// Ordinary names that share letters with the denied list, an unexported
		// leak (not part of any contract), a plain-string ref, and a big record
		// that happens to carry a Kind.
		"controlplane/ok.go": `package controlplane

func Issue() {}

type Feed struct {
	Issuer string
	Note   string
	Issued int64 ` + "`json:\"iat\"`" + `
}

type taskRunner struct{ taskID string }

type Anchor struct {
	Kind string
	ID   string
}

type Resource struct {
	Kind  ResourceKind
	ID    string
	Name  string
}
`,
		// internal/ is out of band: it is not importable by a host.
		"internal/host/leak.go": `package host

type TaskID string
`,
		// Test files name host concepts on purpose.
		"memory/anchor_test.go": `package memory

type TaskAnchor struct{}
`,
	})

	vs, err := Check(root, DefaultPolicy())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(vs) != 0 {
		t.Fatalf("clean tree reported violations: %v", vs)
	}
}

func TestCamelWords(t *testing.T) {
	cases := map[string][]string{
		"TaskID":       {"task", "id"},
		"Issuer":       {"issuer"},
		"HTTPClient":   {"http", "client"},
		"ID":           {"id"},
		"epicOfATask":  {"epic", "of", "a", "task"},
		"Sprint":       {"sprint"},
		"NotebookEdit": {"notebook", "edit"},
	}
	for in, want := range cases {
		got := camelWords(in)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("camelWords(%q) = %v, want %v", in, got, want)
		}
	}
}

// Property: an exported name is refused exactly when one of its camel components
// is a denied noun. Neither half may drift - a name built only from innocent words
// is always allowed however it is cased, and inserting a denied noun as a
// component always trips the gate, wherever in the name it lands.
func TestProp_ExportedNameRefusedIffItCarriesADeniedNoun(t *testing.T) {
	policy := DefaultPolicy()
	denied := []string{"Task", "Subtask", "Epic", "Sprint", "Backlog", "Kanban", "Milestone", "Ticket", "Entity", "Cortex"}
	innocent := []string{"Issuer", "Note", "Area", "Project", "Record", "ID", "Store", "Ref", "Kind", "Issued", "Taskless"}

	root := t.TempDir()
	rapid.Check(t, func(rt *rapid.T) {
		parts := rapid.SliceOfN(rapid.SampledFrom(innocent), 1, 4).Draw(rt, "parts")
		insert := rapid.Bool().Draw(rt, "insert")
		if insert {
			at := rapid.IntRange(0, len(parts)).Draw(rt, "at")
			noun := rapid.SampledFrom(denied).Draw(rt, "noun")
			parts = append(parts[:at:at], append([]string{noun}, parts[at:]...)...)
		}
		name := strings.Join(parts, "")

		if err := writeFile(root, "pkg/decl.go", "package pkg\n\ntype "+name+" struct{}\n"); err != nil {
			rt.Fatalf("write: %v", err)
		}
		vs, err := Check(root, policy)
		if err != nil {
			rt.Fatalf("check: %v", err)
		}
		if insert && len(vs) == 0 {
			rt.Fatalf("%q carries a denied noun but was allowed", name)
		}
		if !insert && len(vs) != 0 {
			rt.Fatalf("%q is built from innocent words but was refused: %v", name, vs)
		}
	})
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
