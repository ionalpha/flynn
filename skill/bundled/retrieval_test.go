package bundled

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/skill/skillrecall"
	"github.com/ionalpha/flynn/state"
)

// seedPack puts the whole pack in a store, which is the state a run recalls from.
// The table is checked against that rather than against the loaded files: what
// decides whether a skill is offered is the record, and the record is what a
// description has to survive being turned into.
func seedPack(t *testing.T) state.SkillStore {
	t.Helper()
	store := newStore(t)
	if _, err := Seed(context.Background(), store); err != nil {
		t.Fatalf("seed the pack: %v", err)
	}
	return store
}

// TestPackIsRetrievable runs the runtime's ranker over the real pack for every
// objective the table names. It is the cheap half of judging a skill: no model, no
// tokens, and it catches the way an authored skill most often fails, which is that
// it is correct, it is well written, and nothing ever offers it.
func TestPackIsRetrievable(t *testing.T) {
	table, err := RetrievalTable()
	if err != nil {
		t.Fatalf("read the retrieval table: %v", err)
	}
	failures := table.Check(context.Background(), seedPack(t), skillrecall.DefaultLimit)
	if len(failures) > 0 {
		t.Errorf("the pack does not retrieve the way it says it does:\n%s", skillrecall.Report(failures))
	}
}

// TestEveryPackSkillStatesItsTriggers refuses a skill nobody said an objective for.
// A skill with no row is one whose author never had to answer what it is reached
// for, and the answer is worth writing down before the body is: a skill whose
// trigger set cannot be stated does not yet have a scope. It also catches the row
// left behind by a rename, which would otherwise assert nothing forever.
func TestEveryPackSkillStatesItsTriggers(t *testing.T) {
	table, err := RetrievalTable()
	if err != nil {
		t.Fatalf("read the retrieval table: %v", err)
	}
	packed, err := Skills()
	if err != nil {
		t.Fatalf("load the pack: %v", err)
	}
	shipped := map[string]bool{}
	for _, sk := range packed {
		shipped[sk.Slug] = true
	}

	claimed := map[string]bool{}
	for _, slug := range table.Claims() {
		claimed[slug] = true
	}
	for _, sk := range packed {
		if !claimed[sk.Slug] {
			t.Errorf("%s ships with no objective it must be offered for; add a row to %s", sk.Slug, RetrievalPath)
		}
	}
	for _, slug := range table.Covers() {
		if !shipped[slug] {
			t.Errorf("%s names %s, which the pack does not ship; the row asserts nothing", RetrievalPath, slug)
		}
	}
}
