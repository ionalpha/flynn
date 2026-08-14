package skillrecall_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/skill/skillrecall"
	"github.com/ionalpha/flynn/state"
)

// library is a small pack: two skills whose subjects do not overlap, seeded the way
// the runtime holds them.
func library(t *testing.T, skills ...state.Skill) state.SkillStore {
	t.Helper()
	store := state.NewMemory().Skills()
	for _, sk := range skills {
		if _, err := store.Upsert(context.Background(), sk); err != nil {
			t.Fatalf("seed %s: %v", sk.Slug, err)
		}
	}
	return store
}

func debugging() state.Skill {
	return state.Skill{
		Slug:        "systematic-debugging",
		Name:        "systematic-debugging",
		Description: "Find a failing test's cause by bisecting the change rather than guessing at it.",
	}
}

func migration() state.Skill {
	return state.Skill{
		Slug:        "schema-migration",
		Name:        "schema-migration",
		Description: "Add or alter a database column without taking the table offline.",
	}
}

func TestParseTable(t *testing.T) {
	table, err := skillrecall.ParseTable([]byte(`
# a comment, and a blank line above

the tests are failing after my change | systematic-debugging |
add a column to the users table | schema-migration | systematic-debugging  # trailing comment
anything about tables at all |  | schema-migration
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(table.Cases) != 3 {
		t.Fatalf("parsed %d rows, want 3: %+v", len(table.Cases), table.Cases)
	}
	first := table.Cases[0]
	if first.Objective != "the tests are failing after my change" {
		t.Errorf("objective = %q", first.Objective)
	}
	if len(first.Offered) != 1 || first.Offered[0] != "systematic-debugging" || len(first.Absent) != 0 {
		t.Errorf("row 1 = %+v, want one offered and nothing absent", first)
	}
	// The line number is the file's, not the row's, so a failure names somewhere to
	// edit even though blank lines and comments are skipped.
	if first.Line != 4 {
		t.Errorf("first row reported line %d, want 4", first.Line)
	}
	if got := table.Cases[2]; len(got.Offered) != 0 || len(got.Absent) != 1 {
		t.Errorf("row 3 = %+v, want an absent-only row", got)
	}
	if got, want := strings.Join(table.Claims(), ","), "schema-migration,systematic-debugging"; got != want {
		t.Errorf("Claims() = %q, want %q", got, want)
	}
	if got, want := strings.Join(table.Covers(), ","), "schema-migration,systematic-debugging"; got != want {
		t.Errorf("Covers() = %q, want %q", got, want)
	}
}

// A malformed row is refused by line and by reason. The table is edited by hand
// while a skill is being written, so the parser's job on a bad row is to say where
// it is, not to guess what was meant.
func TestParseTableRefusesRowsThatAssertNothing(t *testing.T) {
	for name, in := range map[string]string{
		"no objective":       "| systematic-debugging |",
		"nothing expected":   "the tests are failing |  |",
		"too many columns":   "objective | a | b | c",
		"expectation only":   "   | | ",
		"objective and hash": "# systematic-debugging is for tests",
	} {
		t.Run(name, func(t *testing.T) {
			table, err := skillrecall.ParseTable([]byte(in))
			if name == "objective and hash" {
				// A commented line is not a row at all, which is the one case here that is
				// not an error.
				if err != nil || len(table.Cases) != 0 {
					t.Fatalf("a comment produced %d rows, err %v", len(table.Cases), err)
				}
				return
			}
			if err == nil {
				t.Fatalf("parsed %q into %+v, want a refusal", in, table.Cases)
			}
		})
	}
}

// TestCheckCatchesADescriptionThatMissesItsOwnSubject is the first of the two faults
// the table exists for: a skill that is correct and well written and that the
// objective it was written for never reaches.
func TestCheckCatchesADescriptionThatMissesItsOwnSubject(t *testing.T) {
	narrow := debugging()
	narrow.Description = "Bisect a regression by halving the interval of suspect commits."
	store := library(t, narrow, migration())

	table, err := skillrecall.ParseTable([]byte("the tests are failing after my change | systematic-debugging |"))
	if err != nil {
		t.Fatal(err)
	}
	failures := table.Check(context.Background(), store, skillrecall.DefaultLimit)
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want the missed offer", failures)
	}
	if failures[0].Slug != "systematic-debugging" || !strings.Contains(failures[0].Reason, "not offered") {
		t.Errorf("failure = %q, want it to name the skill that was not offered", failures[0])
	}

	// The same objective against the description that does mention tests and failing
	// passes, which is what makes the check a statement about the writing rather than
	// about the ranker.
	if got := table.Check(context.Background(), library(t, debugging(), migration()), skillrecall.DefaultLimit); len(got) != 0 {
		t.Errorf("a description that reaches its subject failed: %s", skillrecall.Report(got))
	}
}

// TestCheckCatchesADescriptionThatReachesIntoAnother is the fault nobody writing the
// skill would test for, and the one that degrades every other skill in the pack: a
// description broad enough to be offered for objectives that belong to a neighbour.
func TestCheckCatchesADescriptionThatReachesIntoAnother(t *testing.T) {
	greedy := debugging()
	greedy.Description = "Work through any change to a database table, a failing test, or a slow page, step by step."
	store := library(t, greedy, migration())

	table, err := skillrecall.ParseTable([]byte("add a column to the users table | schema-migration | systematic-debugging"))
	if err != nil {
		t.Fatal(err)
	}
	failures := table.Check(context.Background(), store, skillrecall.DefaultLimit)
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want the skill that crowded in", failures)
	}
	if failures[0].Slug != "systematic-debugging" || !strings.Contains(failures[0].Reason, "not its own") {
		t.Errorf("failure = %q, want it to name the skill that reached too far", failures[0])
	}
	// The report says what was offered, because that is what tells the author whether
	// to narrow this description or widen the other one.
	if !strings.Contains(failures[0].String(), "schema-migration") {
		t.Errorf("the report does not say what the objective was actually offered: %q", failures[0])
	}
}

// pack is a skill tree with rows in it: two skills that state their objectives and
// one that does not, which is the state LoadTable has to read without complaining.
func pack() fstest.MapFS {
	return fstest.MapFS{
		"skills/systematic-debugging/SKILL.md":      {Data: []byte("---\nname: systematic-debugging\n---\n")},
		"skills/systematic-debugging/retrieval.txt": {Data: []byte("the tests are failing after my change | systematic-debugging |\n")},
		"skills/schema-migration/SKILL.md":          {Data: []byte("---\nname: schema-migration\n---\n")},
		"skills/schema-migration/retrieval.txt":     {Data: []byte("# rows for this skill\n\nadd a column to the users table | schema-migration | systematic-debugging\n")},
		"skills/undocumented/SKILL.md":              {Data: []byte("---\nname: undocumented\n---\n")},
		"skills/README.md":                          {Data: []byte("not a skill\n")},
	}
}

// TestLoadTableReadsEverySkillsRows is the whole point of the per-skill files: the
// pack's table is assembled from them, and a skill that has not written any yet is
// silently absent rather than an error, because what refuses that is the coverage test
// with a message about the skill instead of one about a missing file.
func TestLoadTableReadsEverySkillsRows(t *testing.T) {
	table, err := skillrecall.LoadTable(pack(), "skills")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(table.Cases) != 2 {
		t.Fatalf("loaded %d rows, want 2: %+v", len(table.Cases), table.Cases)
	}
	if got, want := strings.Join(table.Claims(), ","), "schema-migration,systematic-debugging"; got != want {
		t.Errorf("Claims() = %q, want %q", got, want)
	}
	// A row carries the file it was written in, because the line number alone would
	// name one of a dozen files by accident.
	migrationRow := table.Cases[0]
	if got, want := migrationRow.Where(), "skills/schema-migration/retrieval.txt:3"; got != want {
		t.Errorf("Where() = %q, want %q", got, want)
	}
}

// TestLoadTableRefusesARowThatWanderedIntoAnotherSkillsFile keeps a skill's file the
// whole of what the pack claims about it. Without the rule the split would lose what
// the single table gave for free: somewhere to read every claim about one skill.
func TestLoadTableRefusesARowThatWanderedIntoAnotherSkillsFile(t *testing.T) {
	fsys := pack()
	fsys["skills/undocumented/retrieval.txt"] = &fstest.MapFile{
		Data: []byte("the tests are failing after my change | systematic-debugging |\n"),
	}
	_, err := skillrecall.LoadTable(fsys, "skills")
	if err == nil {
		t.Fatal("a row naming nothing in its own directory was accepted")
	}
	if !strings.Contains(err.Error(), "skills/undocumented/retrieval.txt:1") || !strings.Contains(err.Error(), "undocumented") {
		t.Errorf("error = %q, want it to name the file, the line and the skill", err)
	}
}

// A row that breaks the format is refused by file and line rather than by content,
// so the message is an address to open.
func TestLoadTableRefusesAMalformedRowByAddress(t *testing.T) {
	fsys := pack()
	fsys["skills/undocumented/retrieval.txt"] = &fstest.MapFile{Data: []byte("| undocumented |\n")}
	_, err := skillrecall.LoadTable(fsys, "skills")
	if err == nil {
		t.Fatal("a row with no objective was accepted")
	}
	if !strings.Contains(err.Error(), "skills/undocumented/retrieval.txt") {
		t.Errorf("error = %q, want it to name the file", err)
	}
}

// unreadable is a pack whose one file cannot be read, which is the difference between
// a skill that has not written its rows and a skill whose rows are there and lost.
type unreadable struct {
	fstest.MapFS
	path string
}

func (u unreadable) ReadFile(name string) ([]byte, error) {
	if name == u.path {
		return nil, fmt.Errorf("read %s: device is on fire", name)
	}
	return u.MapFS.ReadFile(name)
}

// A file that exists and will not open is an error, where a file that is absent is
// not. Skipping both would turn a broken checkout into a pack that quietly asserts
// less than it used to.
func TestLoadTableRefusesRowsItCannotRead(t *testing.T) {
	fsys := unreadable{MapFS: pack(), path: "skills/schema-migration/retrieval.txt"}
	if _, err := skillrecall.LoadTable(fsys, "skills"); err == nil {
		t.Fatal("an unreadable file was treated as a skill with no rows")
	}
	if _, err := skillrecall.LoadTable(pack(), "no-such-pack"); err == nil {
		t.Fatal("a root that does not exist was loaded as an empty table")
	}
}
