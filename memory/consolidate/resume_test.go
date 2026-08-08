package consolidate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/memory/consolidate"
	"github.com/ionalpha/flynn/state"
)

// Running the pass again does nothing. The episodes are gone, so the second run
// finds no series where the first one found one, which is the base case of
// idempotence and the one a nightly job depends on.
func TestSecondRunDoesNothing(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.episode("flaky-deploy", fmt.Sprint("failure ", i))
	}
	first := f.run()
	before := f.live("flaky-deploy")

	second := f.run()
	if len(second.Results) != 0 {
		t.Fatalf("the second run did work: %+v", second.Results)
	}
	if len(f.seen) != 1 {
		t.Fatalf("distiller calls across two runs = %d, want 1", len(f.seen))
	}
	if got := f.live("flaky-deploy"); !equalStrings(got, before) {
		t.Fatalf("the second run changed the store: %v, was %v", got, before)
	}
	if first.Distilled() != 1 {
		t.Fatalf("first run = %+v, want one distilled subject", first.Results)
	}
}

// The case the resume path exists for: a run killed between writing the lesson
// and retiring the episodes. The store is then holding a lesson that names live
// episodes, which is exactly the state a naive second run would distil all over
// again, producing two lessons about one series and a supersession chain that
// disagrees with itself.
func TestInterruptedRunIsFinishedNotRepeated(t *testing.T) {
	f := newFixture(t)
	var ids []string
	for i := range 3 {
		ids = append(ids, f.episode("flaky-deploy", fmt.Sprint("failure ", i)).ID)
	}
	// The half-done state, written by hand: the lesson landed, the deletes did not.
	lesson, err := f.store.Write(context.Background(), state.MemoryItem{
		Kind: "lesson", Subject: "flaky-deploy", Content: "the deploy fails when the cache is cold",
		Supersedes: ids,
	})
	if err != nil {
		t.Fatalf("write the lesson: %v", err)
	}

	rep := f.run()
	if len(rep.Results) != 1 {
		t.Fatalf("results = %+v, want one", rep.Results)
	}
	res := rep.Results[0]
	if res.Outcome != consolidate.OutcomeResumed {
		t.Fatalf("outcome = %q, want resumed", res.Outcome)
	}
	if res.Lesson.ID != lesson.ID {
		t.Fatalf("resumed against lesson %s, want the one already written (%s)", res.Lesson.ID, lesson.ID)
	}
	if len(res.Retired) != 3 {
		t.Fatalf("retired %v, want all three episodes", res.Retired)
	}
	if rep.Resumed() != 1 || rep.Distilled() != 0 {
		t.Fatalf("counts = %d resumed / %d distilled, want 1 / 0", rep.Resumed(), rep.Distilled())
	}
	if len(f.seen) != 0 {
		t.Fatalf("the distiller was called for a series a lesson already covered: %+v", f.seen)
	}
	if got := f.live("flaky-deploy"); len(got) != 1 || got[0] != "the deploy fails when the cache is cold" {
		t.Fatalf("live = %v, want the one lesson the interrupted run wrote", got)
	}
}

// An episode written after the interrupted run is not covered by its lesson, so
// it is not swept up by the resume: the pass finishes exactly the work that was
// accounted for and leaves the rest for the next run, which then has a series of
// its own to judge.
func TestResumeLeavesUncoveredEpisodes(t *testing.T) {
	f := newFixture(t)
	var covered []string
	for i := range 3 {
		covered = append(covered, f.episode("flaky-deploy", fmt.Sprint("failure ", i)).ID)
	}
	if _, err := f.store.Write(context.Background(), state.MemoryItem{
		Kind: "lesson", Subject: "flaky-deploy", Content: "an earlier lesson", Supersedes: covered,
	}); err != nil {
		t.Fatalf("write the lesson: %v", err)
	}
	f.episode("flaky-deploy", "a failure nobody has read yet")

	if res := f.run().Results[0]; res.Outcome != consolidate.OutcomeResumed || len(res.Retired) != 3 {
		t.Fatalf("result = %+v, want the covered three retired", res)
	}
	got := f.live("flaky-deploy")
	if len(got) != 2 || !containsID(got, "a failure nobody has read yet") {
		t.Fatalf("live = %v, want the lesson and the uncovered episode", got)
	}
}

// A lesson on the subject that covers none of the episodes in hand is an older
// consolidation, not an interrupted one. The new series is distilled on its own
// terms, and the two lessons stand side by side until a host decides otherwise.
func TestAnOlderLessonDoesNotBlockANewSeries(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Write(context.Background(), state.MemoryItem{
		Kind: "lesson", Subject: "flaky-deploy", Content: "last month's lesson",
		Supersedes: []string{"mem-long-gone"},
	}); err != nil {
		t.Fatalf("write the old lesson: %v", err)
	}
	for i := range 3 {
		f.episode("flaky-deploy", fmt.Sprint("a new failure ", i))
	}

	if res := f.run().Results[0]; res.Outcome != consolidate.OutcomeDistilled {
		t.Fatalf("outcome = %q, want the new series distilled", res.Outcome)
	}
	got := f.live("flaky-deploy")
	if len(got) != 2 || !containsID(got, "last month's lesson") {
		t.Fatalf("live = %v, want both lessons", got)
	}
}

// A lesson that supersedes nothing was written by hand rather than by a pass, so
// it says nothing about whether this series has been consolidated. Reading it as
// an interrupted run would strand a series that nobody has distilled.
func TestALessonThatSupersedesNothingIsNotAResume(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Write(context.Background(), state.MemoryItem{
		Kind: "lesson", Subject: "flaky-deploy", Content: "somebody wrote this by hand",
	}); err != nil {
		t.Fatalf("write the lesson: %v", err)
	}
	for i := range 3 {
		f.episode("flaky-deploy", fmt.Sprint("failure ", i))
	}

	if res := f.run().Results[0]; res.Outcome != consolidate.OutcomeDistilled {
		t.Fatalf("outcome = %q, want the series distilled", res.Outcome)
	}
}

// A subject with no episodes is not an error and not work. It is what a host gets
// when it consolidates on a signal that turned out to be stale, and the honest
// answer is an empty result rather than a failure.
func TestSubjectWithNoEpisodes(t *testing.T) {
	f := newFixture(t)
	res, err := f.pass.Subject(context.Background(), "flaky-deploy", state.Scope{})
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	if res.Outcome != consolidate.OutcomeTooFew || res.Episodes != 0 {
		t.Fatalf("result = %+v, want an empty series reported as too few", res)
	}
	if len(f.seen) != 0 {
		t.Fatalf("the distiller was called for an empty series: %+v", f.seen)
	}
}

// A lesson with the same subject in another scope is a different series' lesson,
// and must not be read as this one's interrupted run.
func TestResumeStaysInScope(t *testing.T) {
	f := newFixture(t)
	scope := state.Scope{Project: "p"}
	var ids []string
	for i := range 3 {
		ids = append(ids, f.episode("flaky-deploy", fmt.Sprint("failure ", i),
			func(it *state.MemoryItem) { it.Scope = scope }).ID)
	}
	// A lesson in the global scope naming this scope's episodes: possible, because
	// nothing resolves a superseded id, and not this series' business.
	if _, err := f.store.Write(context.Background(), state.MemoryItem{
		Kind: "lesson", Subject: "flaky-deploy", Content: "somebody else's lesson", Supersedes: ids,
	}); err != nil {
		t.Fatalf("write the lesson: %v", err)
	}

	res, err := f.pass.Subject(context.Background(), "flaky-deploy", scope)
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	if res.Outcome != consolidate.OutcomeDistilled {
		t.Fatalf("outcome = %q, want the scoped series distilled on its own terms", res.Outcome)
	}
}

// Retiring an episode that is already gone is not a failure. Another run, or a
// host clearing up by hand, has done the same work, and the pass is defined by
// the state it leaves rather than by who got there first.
func TestRetiringAnAlreadyGoneEpisodeIsNotAFailure(t *testing.T) {
	p := state.NewMemory()
	t.Cleanup(func() { _ = p.Close() })
	inner := p.Memory()
	ctx := context.Background()

	var ids []string
	for i := range 3 {
		it, err := inner.Write(ctx, state.MemoryItem{
			Kind: "episode", Subject: "flaky-deploy", Content: fmt.Sprint("failure ", i),
		})
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		ids = append(ids, it.ID)
	}
	// One episode disappears between the read and the retirement, which is what a
	// concurrent run or a curator looks like from here.
	stub := stubStore{MemoryStore: inner, deleteFn: func(id string) error {
		if id == ids[1] {
			return state.ErrNotFound
		}
		return inner.Delete(ctx, id)
	}}
	pass, err := consolidate.New(stub, consolidate.DistillerFunc(
		func(context.Context, consolidate.Series) (consolidate.Lesson, error) {
			return consolidate.Lesson{Content: "the lesson"}, nil
		}))
	if err != nil {
		t.Fatalf("new pass: %v", err)
	}

	rep, err := pass.Run(ctx, state.RecallQuery{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Failures) != 0 {
		t.Fatalf("failures = %+v, want none", rep.Failures)
	}
	res := rep.Results[0]
	if res.Outcome != consolidate.OutcomeDistilled {
		t.Fatalf("outcome = %q, want distilled", res.Outcome)
	}
	// The report says what this run actually retired, not what it intended to.
	if len(res.Retired) != 2 || containsID(res.Retired, ids[1]) {
		t.Fatalf("retired = %v, want the two this run really tombstoned", res.Retired)
	}
}

// Consolidating one subject a hundred times in a row leaves the store where one
// consolidation would have. This is idempotence stated over the operation rather
// than over a single scenario, which is where the resume logic would otherwise be
// only as good as the cases somebody wrote down.
func TestRepeatedRunsConverge(t *testing.T) {
	f := newFixture(t)
	for i := range 5 {
		f.episode("flaky-deploy", fmt.Sprint("failure ", i))
	}
	first := f.run()
	settled := f.live("flaky-deploy")

	for range 10 {
		rep := f.run()
		for _, res := range rep.Results {
			if res.Outcome == consolidate.OutcomeDistilled {
				t.Fatalf("a repeat run distilled again: %+v", res)
			}
		}
		if got := f.live("flaky-deploy"); !equalStrings(got, settled) {
			t.Fatalf("a repeat run changed the store: %v, was %v", got, settled)
		}
	}
	if len(f.seen) != 1 || first.Distilled() != 1 {
		t.Fatalf("distiller calls = %d across eleven runs, want 1", len(f.seen))
	}
}

// stubStore overrides one method of a real store so a failure can be injected
// without reimplementing the whole interface.
type stubStore struct {
	state.MemoryStore
	recallFn func(q state.RecallQuery) ([]state.MemoryItem, error)
	writeErr error
	deleteFn func(id string) error
}

func (s stubStore) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	if s.recallFn != nil {
		return s.recallFn(q)
	}
	return s.MemoryStore.Recall(ctx, q)
}

func (s stubStore) Write(ctx context.Context, m state.MemoryItem) (state.MemoryItem, error) {
	if s.writeErr != nil {
		return state.MemoryItem{}, s.writeErr
	}
	return s.MemoryStore.Write(ctx, m)
}

func (s stubStore) Delete(ctx context.Context, id string) error {
	if s.deleteFn != nil {
		return s.deleteFn(id)
	}
	return s.MemoryStore.Delete(ctx, id)
}

// newStubbedPass builds a pass over a stubbed store, with a distiller a test can
// steer.
func newStubbedPass(t *testing.T, stub func(*stubStore, state.MemoryStore),
	distil func(consolidate.Series) (consolidate.Lesson, error),
) (*consolidate.Pass, state.MemoryStore) {
	t.Helper()
	p := state.NewMemory()
	t.Cleanup(func() { _ = p.Close() })
	inner := p.Memory()
	s := stubStore{MemoryStore: inner}
	stub(&s, inner)
	pass, err := consolidate.New(s, consolidate.DistillerFunc(
		func(_ context.Context, in consolidate.Series) (consolidate.Lesson, error) {
			if distil != nil {
				return distil(in)
			}
			return consolidate.Lesson{Content: "the lesson"}, nil
		}))
	if err != nil {
		t.Fatalf("new pass: %v", err)
	}
	return pass, inner
}

// seedEpisodes writes n episodes on a subject straight into the store.
func seedEpisodes(t *testing.T, store state.MemoryStore, subject string, n int) {
	t.Helper()
	for i := range n {
		if _, err := store.Write(context.Background(), state.MemoryItem{
			Kind: "episode", Subject: subject, Content: fmt.Sprint(subject, " ", i),
		}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// One subject that cannot be finished does not cost the rest of the sweep. An
// offline pass over a whole store that returned on the first model timeout would
// leave two hundred healthy subjects unconsolidated and no record of which.
func TestOneBadSubjectDoesNotStopTheSweep(t *testing.T) {
	boom := errors.New("the model timed out")
	pass, inner := newStubbedPass(t, func(*stubStore, state.MemoryStore) {},
		func(in consolidate.Series) (consolidate.Lesson, error) {
			if in.Subject == "flaky-deploy" {
				return consolidate.Lesson{}, boom
			}
			return consolidate.Lesson{Content: "the lesson"}, nil
		})
	seedEpisodes(t, inner, "flaky-deploy", 3)
	seedEpisodes(t, inner, "slow-tests", 3)

	rep, err := pass.Run(context.Background(), state.RecallQuery{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Failures) != 1 || rep.Failures[0].Subject != "flaky-deploy" || !errors.Is(rep.Failures[0].Err, boom) {
		t.Fatalf("failures = %+v, want the one subject that could not be distilled", rep.Failures)
	}
	if rep.Distilled() != 1 || rep.Results[0].Subject != "slow-tests" {
		t.Fatalf("results = %+v, want the healthy subject consolidated", rep.Results)
	}
}

// A lesson that cannot be written retires nothing. The write comes first for
// exactly this reason: a pass that deleted the evidence and then failed to store
// what it had learned from it would have destroyed the series.
func TestALessonThatCannotBeWrittenRetiresNothing(t *testing.T) {
	boom := errors.New("write unavailable")
	pass, inner := newStubbedPass(t, func(s *stubStore, _ state.MemoryStore) { s.writeErr = boom }, nil)
	seedEpisodes(t, inner, "flaky-deploy", 3)

	rep, err := pass.Run(context.Background(), state.RecallQuery{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Failures) != 1 || !errors.Is(rep.Failures[0].Err, boom) {
		t.Fatalf("failures = %+v, want the write failure", rep.Failures)
	}
	items, err := inner.Recall(context.Background(), state.RecallQuery{Subjects: []string{"flaky-deploy"}})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("live = %d items, want all three episodes untouched", len(items))
	}
}

// A retirement that fails part way reports the subject as failed and says which
// ids did come out, so a host reading the report knows the store is half-way and
// a re-run finishes it through the resume path.
func TestAFailedRetirementIsReported(t *testing.T) {
	boom := errors.New("delete unavailable")
	var seen int
	pass, inner := newStubbedPass(t, func(s *stubStore, in state.MemoryStore) {
		s.deleteFn = func(id string) error {
			if seen++; seen > 1 {
				return boom
			}
			return in.Delete(context.Background(), id)
		}
	}, nil)
	seedEpisodes(t, inner, "flaky-deploy", 3)

	rep, err := pass.Run(context.Background(), state.RecallQuery{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Failures) != 1 || !errors.Is(rep.Failures[0].Err, boom) {
		t.Fatalf("failures = %+v, want the delete failure", rep.Failures)
	}
	// The lesson is stored and names every episode, so the next run resumes rather
	// than distilling a second one.
	rep2, err := pass.Run(context.Background(), state.RecallQuery{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(rep2.Failures) == 0 && rep2.Resumed() != 1 {
		t.Fatalf("the second run = %+v, want it resuming the interrupted retirement", rep2)
	}
}

// The reads the pass depends on are its own, and a store that cannot answer them
// fails the work rather than guessing at it.
func TestReadFailuresAreReported(t *testing.T) {
	boom := errors.New("recall unavailable")

	// The sweep's own read: nothing has happened yet, so this is a plain error.
	sweep, _ := newStubbedPass(t, func(s *stubStore, _ state.MemoryStore) {
		s.recallFn = func(state.RecallQuery) ([]state.MemoryItem, error) { return nil, boom }
	}, nil)
	if _, err := sweep.Run(context.Background(), state.RecallQuery{}); !errors.Is(err, boom) {
		t.Fatalf("run = %v, want the store's error", err)
	}

	// The single-subject read.
	one, _ := newStubbedPass(t, func(s *stubStore, _ state.MemoryStore) {
		s.recallFn = func(state.RecallQuery) ([]state.MemoryItem, error) { return nil, boom }
	}, nil)
	if _, err := one.Subject(context.Background(), "flaky-deploy", state.Scope{}); !errors.Is(err, boom) {
		t.Fatalf("subject = %v, want the store's error", err)
	}

	// The resume check, which runs before anything is distilled: a pass that
	// shrugged this off would distil a second lesson over an interrupted run.
	resume, inner := newStubbedPass(t, func(s *stubStore, in state.MemoryStore) {
		s.recallFn = func(q state.RecallQuery) ([]state.MemoryItem, error) {
			if len(q.Kinds) == 1 && q.Kinds[0] == consolidate.KindLesson {
				return nil, boom
			}
			return in.Recall(context.Background(), q)
		}
	}, nil)
	seedEpisodes(t, inner, "flaky-deploy", 3)
	rep, err := resume.Run(context.Background(), state.RecallQuery{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Failures) != 1 || !errors.Is(rep.Failures[0].Err, boom) {
		t.Fatalf("failures = %+v, want the lesson read's failure", rep.Failures)
	}
}
