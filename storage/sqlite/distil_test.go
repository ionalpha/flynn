package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/memory/consolidate"
	"github.com/ionalpha/flynn/memory/distil"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// TestConsolidateOverSQLite is the standalone acceptance for the consolidation
// pass: Flynn alone, a temp database file and a configured model, with no host
// supplying anything. Until the bundled distiller existed this could not run at
// all, because consolidate.New refused without one.
//
// All four outcomes come back from the shipped path in one sweep, because they
// are only meaningful against each other: a pass that distils everything is as
// wrong as one that distils nothing, and the two that leave a series alone
// (too-few, declined) have to be distinguishable from the two that act.
func TestConsolidateOverSQLite(t *testing.T) {
	ctx := context.Background()
	p, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "consolidate.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	st := p.Memory()

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	episode := func(subject, content string, age int) state.MemoryItem {
		t.Helper()
		it, werr := st.Write(ctx, state.MemoryItem{
			Kind:      consolidate.KindEpisode,
			Subject:   subject,
			Content:   content,
			Sources:   []string{"agent:run-1"},
			CreatedAt: base.Add(time.Duration(age) * time.Hour),
		})
		if werr != nil {
			t.Fatalf("write episode %q: %v", content, werr)
		}
		return it
	}

	// Subjects are consolidated in a stable order, so naming them alphabetically
	// by outcome fixes which reply the scripted model gives to which series.
	for i, c := range []string{"the deploy failed", "it failed again", "it failed once the migration moved"} {
		episode("aaa-distilled", c, i)
	}
	for i, c := range []string{"a note", "another note", "a third note"} {
		episode("bbb-declined", c, i)
	}
	for i, c := range []string{"one failure", "a second failure"} {
		episode("ccc-too-few", c, i)
	}
	var interrupted []string
	for i, c := range []string{"the job stalled", "it stalled again", "it stalled on the same lock"} {
		interrupted = append(interrupted, episode("ddd-resumed", c, i).ID)
	}
	// An earlier run that was killed between writing the lesson and retiring the
	// episodes it was drawn from: the lesson is there, the episodes are still live.
	if _, err = st.Write(ctx, state.MemoryItem{
		Kind:       consolidate.KindLesson,
		Subject:    "ddd-resumed",
		Content:    "the lock is held by the previous job; wait for it",
		Sources:    []string{"agent:run-1"},
		Supersedes: interrupted,
	}); err != nil {
		t.Fatalf("write the interrupted run's lesson: %v", err)
	}

	// Only two subjects reach the model on the first sweep: the resumed one is
	// finished before anything is distilled, and the too-few one never gets that
	// far. The third turn is for the second sweep, where the declined series is
	// the only one still asking.
	model := llmtest.NewScripted(
		llmtest.SayText("The deploy fails when the migration runs after it. Run the migration first."),
		llmtest.SayText("NONE"),
		llmtest.SayText("NONE"),
	)
	pass, err := consolidate.New(st, distil.NewGoverned(distil.New(model)))
	if err != nil {
		t.Fatalf("build the pass: %v", err)
	}

	rep, err := pass.Run(ctx, state.RecallQuery{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Failures) != 0 {
		t.Fatalf("failures = %+v, want none", rep.Failures)
	}
	got := map[string]consolidate.Outcome{}
	for _, r := range rep.Results {
		got[r.Subject] = r.Outcome
	}
	want := map[string]consolidate.Outcome{
		"aaa-distilled": consolidate.OutcomeDistilled,
		"bbb-declined":  consolidate.OutcomeDeclined,
		"ccc-too-few":   consolidate.OutcomeTooFew,
		"ddd-resumed":   consolidate.OutcomeResumed,
	}
	for subject, w := range want {
		if got[subject] != w {
			t.Errorf("%s = %q, want %q", subject, got[subject], w)
		}
	}
	if model.Calls() != 2 {
		t.Errorf("model calls = %d, want 2: only a ready, unresumed series costs one", model.Calls())
	}

	// The distilled subject is now one lesson and no episodes, which is the whole
	// point: a wake digest can afford the lesson and could never afford the five
	// near-identical failures behind it.
	lessons := recallKind(t, ctx, st, "aaa-distilled", consolidate.KindLesson)
	if len(lessons) != 1 {
		t.Fatalf("lessons on the distilled subject = %d, want 1", len(lessons))
	}
	if lessons[0].Content == "" {
		t.Error("the lesson has no content")
	}
	if len(lessons[0].Supersedes) != 3 {
		t.Errorf("the lesson supersedes %d episodes, want 3", len(lessons[0].Supersedes))
	}
	if left := recallKind(t, ctx, st, "aaa-distilled", consolidate.KindEpisode); len(left) != 0 {
		t.Errorf("episodes left on the distilled subject = %d, want 0", len(left))
	}

	// The declined series is untouched, so the next run can try again. Declining
	// costs a re-read; deleting the evidence would cost the series.
	if left := recallKind(t, ctx, st, "bbb-declined", consolidate.KindEpisode); len(left) != 3 {
		t.Errorf("episodes left on the declined subject = %d, want all 3", len(left))
	}
	if lessons = recallKind(t, ctx, st, "bbb-declined", consolidate.KindLesson); len(lessons) != 0 {
		t.Errorf("lessons on the declined subject = %d, want 0", len(lessons))
	}

	// The interrupted run's retirement is finished without a second lesson being
	// drawn from the same series.
	if left := recallKind(t, ctx, st, "ddd-resumed", consolidate.KindEpisode); len(left) != 0 {
		t.Errorf("episodes left on the resumed subject = %d, want 0", len(left))
	}
	if lessons = recallKind(t, ctx, st, "ddd-resumed", consolidate.KindLesson); len(lessons) != 1 {
		t.Errorf("lessons on the resumed subject = %d, want the one the killed run wrote", len(lessons))
	}

	// Running the pass again does what running it once did: every subject is now
	// either consolidated or short of a series, so nothing reaches the model and
	// the exhausted script is never asked for a third turn.
	rep, err = pass.Run(ctx, state.RecallQuery{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(rep.Failures) != 0 {
		t.Fatalf("second run failures = %+v, want none", rep.Failures)
	}
	if rep.Distilled() != 0 || rep.Resumed() != 0 {
		t.Errorf("second run distilled %d and resumed %d, want 0 and 0", rep.Distilled(), rep.Resumed())
	}
	if model.Calls() != 3 {
		t.Errorf("model calls after the second run = %d, want 3: only the still-declining series asks again", model.Calls())
	}
}

// recallKind reads one subject's items of one kind.
func recallKind(t *testing.T, ctx context.Context, st state.MemoryStore, subject, kind string) []state.MemoryItem {
	t.Helper()
	items, err := st.Recall(ctx, state.RecallQuery{Subjects: []string{subject}, Kinds: []string{kind}})
	if err != nil {
		t.Fatalf("recall %s/%s: %v", subject, kind, err)
	}
	return items
}
