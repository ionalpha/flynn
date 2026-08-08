package consolidate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/memory/consolidate"
	"github.com/ionalpha/flynn/state"
)

// epoch is the start of every test's manual clock, so CreatedAt is whatever the
// test advanced it to and never the wall clock.
var epoch = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// fixture is a memory store on a manual clock with a pass over it, and the
// series each Distil call was handed.
type fixture struct {
	t      *testing.T
	store  state.MemoryStore
	pass   *consolidate.Pass
	clk    *clock.Manual
	seen   []consolidate.Series
	distil func(consolidate.Series) (consolidate.Lesson, error)
}

// newFixture builds a pass whose distiller records what it was asked and answers
// with a one-line summary, which is enough for every assertion here: what the
// lesson says is the host's, and what the pass does around it is what is tested.
func newFixture(t *testing.T, opts ...consolidate.Option) *fixture {
	t.Helper()
	clk := clock.NewManual(epoch)
	p := state.NewMemory(state.WithClock(clk))
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Fatalf("close provider: %v", err)
		}
	})
	f := &fixture{t: t, store: p.Memory(), clk: clk}
	pass, err := consolidate.New(f.store, consolidate.DistillerFunc(
		func(_ context.Context, in consolidate.Series) (consolidate.Lesson, error) {
			f.seen = append(f.seen, in)
			if f.distil != nil {
				return f.distil(in)
			}
			return consolidate.Lesson{Content: fmt.Sprintf("%s: %d episodes", in.Subject, len(in.Episodes))}, nil
		}), opts...)
	if err != nil {
		t.Fatalf("new pass: %v", err)
	}
	f.pass = pass
	return f
}

// episode writes one episode and advances the clock, so a series has a stable
// narrative order.
func (f *fixture) episode(subject, content string, mutate ...func(*state.MemoryItem)) state.MemoryItem {
	f.t.Helper()
	it := state.MemoryItem{Kind: "episode", Subject: subject, Content: content}
	for _, m := range mutate {
		m(&it)
	}
	out, err := f.store.Write(context.Background(), it)
	if err != nil {
		f.t.Fatalf("write %q: %v", content, err)
	}
	f.clk.Advance(time.Minute)
	return out
}

// live returns the live contents on a subject, for the assertion that says what
// is left after a pass.
func (f *fixture) live(subject string) []string {
	f.t.Helper()
	items, err := f.store.Recall(context.Background(), state.RecallQuery{Subjects: []string{subject}})
	if err != nil {
		f.t.Fatalf("recall %q: %v", subject, err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Content)
	}
	return out
}

func (f *fixture) run() consolidate.Report {
	f.t.Helper()
	rep, err := f.pass.Run(context.Background(), state.RecallQuery{})
	if err != nil {
		f.t.Fatalf("run: %v", err)
	}
	if len(rep.Failures) != 0 {
		f.t.Fatalf("run reported failures: %+v", rep.Failures)
	}
	return rep
}

// The pass in one shape: a ready series becomes one lesson that says what it
// replaced, and the episodes it was drawn from stop being recalled.
func TestConsolidateDistilsAndRetires(t *testing.T) {
	f := newFixture(t)
	var ids []string
	for _, c := range []string{"failure one", "failure two", "failure three"} {
		ids = append(ids, f.episode("flaky-deploy", c).ID)
	}

	rep := f.run()
	if len(rep.Results) != 1 || rep.Results[0].Outcome != consolidate.OutcomeDistilled {
		t.Fatalf("results = %+v, want one distilled subject", rep.Results)
	}
	if rep.Distilled() != 1 || rep.Resumed() != 0 {
		t.Fatalf("counts = %d distilled / %d resumed, want 1 / 0", rep.Distilled(), rep.Resumed())
	}
	res := rep.Results[0]
	if res.Episodes != 3 || len(res.Retired) != 3 {
		t.Fatalf("result = %+v, want 3 episodes considered and 3 retired", res)
	}
	if got := f.live("flaky-deploy"); len(got) != 1 || got[0] != "flaky-deploy: 3 episodes" {
		t.Fatalf("live on the subject = %v, want the lesson alone", got)
	}
	for _, id := range ids {
		if !containsID(res.Lesson.Supersedes, id) {
			t.Fatalf("the lesson supersedes %v, want it to name %s", res.Lesson.Supersedes, id)
		}
	}
	// The series arrives oldest first: what changed between the first failure and
	// the last is most of what a lesson has to say.
	if len(f.seen) != 1 {
		t.Fatalf("distiller calls = %d, want 1", len(f.seen))
	}
	var order []string
	for _, ep := range f.seen[0].Episodes {
		order = append(order, ep.Content)
	}
	if strings.Join(order, ",") != "failure one,failure two,failure three" {
		t.Fatalf("series order = %v, want oldest first", order)
	}
}

// A series shorter than the threshold is left alone, and nothing is handed to the
// distiller: two episodes are a coincidence, and compressing them would spend a
// model call to shorten something a reader could have read.
func TestShortSeriesIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.episode("flaky-deploy", "failure one")
	f.episode("flaky-deploy", "failure two")

	rep := f.run()
	if len(rep.Results) != 1 || rep.Results[0].Outcome != consolidate.OutcomeTooFew {
		t.Fatalf("results = %+v, want the series reported as too few", rep.Results)
	}
	if len(f.seen) != 0 {
		t.Fatalf("the distiller was called for a short series: %+v", f.seen)
	}
	if got := f.live("flaky-deploy"); len(got) != 2 {
		t.Fatalf("live = %v, want both episodes untouched", got)
	}

	// The threshold is the host's, with a floor: distilling one episode is not
	// consolidation, it is rewriting a memory and losing the original.
	g := newFixture(t, consolidate.WithMinEpisodes(1))
	g.episode("flaky-deploy", "the only failure")
	if rep := g.run(); rep.Results[0].Outcome != consolidate.OutcomeTooFew {
		t.Fatalf("a single episode was consolidated under a floor of two: %+v", rep.Results)
	}
	g.episode("flaky-deploy", "a second failure")
	if rep := g.run(); rep.Results[0].Outcome != consolidate.OutcomeDistilled {
		t.Fatalf("two episodes under a threshold of two were not consolidated: %+v", rep.Results)
	}
}

// A distiller that has nothing to say declines, and declining costs the series
// nothing: no lesson is written and no episode is retired, so the next run sees
// exactly what this one saw.
func TestDeclinedSeriesKeepsItsEpisodes(t *testing.T) {
	f := newFixture(t)
	f.distil = func(consolidate.Series) (consolidate.Lesson, error) { return consolidate.Lesson{}, nil }
	for _, c := range []string{"one", "two", "three"} {
		f.episode("flaky-deploy", c)
	}

	rep := f.run()
	if len(rep.Results) != 1 || rep.Results[0].Outcome != consolidate.OutcomeDeclined {
		t.Fatalf("results = %+v, want the series declined", rep.Results)
	}
	if got := f.live("flaky-deploy"); len(got) != 3 {
		t.Fatalf("live = %v, want all three episodes still there", got)
	}
}

// The lesson inherits everything a later reader has to be able to trust: every
// source that fed it, every ref its episodes were about, and any taint among
// them. A pass that dropped the sources would break a purge, and one that dropped
// the taint would launder attacker-influenced content into a clean-looking lesson
// that the wake digest would then push at every session.
func TestLessonInheritsProvenanceAnchorsAndTaint(t *testing.T) {
	f := newFixture(t)
	widget := state.Anchor{Kind: "widget", ID: "w-1"}
	gadget := state.Anchor{Kind: "gadget", ID: "g-9"}

	f.episode("flaky-deploy", "one", func(it *state.MemoryItem) {
		it.Sources = []string{"agent:run-1", "tool:ci"}
		it.Anchors = []state.Anchor{widget}
	})
	f.episode("flaky-deploy", "two", func(it *state.MemoryItem) {
		it.Sources = []string{"tool:ci"}
		it.Anchors = []state.Anchor{widget, gadget}
		it.Tainted = true
	})
	f.episode("flaky-deploy", "three", func(it *state.MemoryItem) {
		it.Sources = []string{"user:operator"}
	})

	lesson := f.run().Results[0].Lesson
	if want := []string{"agent:run-1", "tool:ci", "user:operator"}; !equalStrings(lesson.Sources, want) {
		t.Fatalf("lesson sources = %v, want the union in first-credited order %v", lesson.Sources, want)
	}
	if want := []state.Anchor{gadget, widget}; !equalAnchors(lesson.Anchors, want) {
		t.Fatalf("lesson anchors = %v, want the canonical union %v", lesson.Anchors, want)
	}
	if !lesson.Tainted {
		t.Fatal("a lesson distilled from a tainted episode is not tainted")
	}
}

// A lesson must not outlive the material it was drawn from, and must not die
// before it either.
func TestLessonExpiryFollowsTheSeries(t *testing.T) {
	early, late := epoch.Add(time.Hour), epoch.Add(2*time.Hour)

	// Every episode expires, so the lesson goes when the last of them would have.
	all := newFixture(t)
	for i, at := range []time.Time{early, late, early} {
		all.episode("sprint-plan", fmt.Sprint("note ", i), func(it *state.MemoryItem) { it.ExpiresAt = at })
	}
	if got := all.run().Results[0].Lesson.ExpiresAt; !got.Equal(late) {
		t.Fatalf("lesson expiry = %v, want the latest of the series %v", got, late)
	}

	// One durable episode among them, and the lesson is about something durable.
	some := newFixture(t)
	some.episode("sprint-plan", "expiring", func(it *state.MemoryItem) { it.ExpiresAt = early })
	some.episode("sprint-plan", "durable")
	some.episode("sprint-plan", "expiring too", func(it *state.MemoryItem) { it.ExpiresAt = late })
	if got := some.run().Results[0].Lesson.ExpiresAt; !got.IsZero() {
		t.Fatalf("lesson expiry = %v, want none: one episode never expires", got)
	}
}

// The pass is defined per subject and per scope. Two projects failing at their
// own deploys are two series, and distilling them together would invent a lesson
// neither of them supports.
func TestSeriesAreGroupedBySubjectAndScope(t *testing.T) {
	f := newFixture(t)
	a := state.Scope{Project: "a"}
	b := state.Scope{Project: "b"}
	for i := range 3 {
		f.episode("flaky-deploy", fmt.Sprint("a failure ", i), func(it *state.MemoryItem) { it.Scope = a })
		f.episode("flaky-deploy", fmt.Sprint("b failure ", i), func(it *state.MemoryItem) { it.Scope = b })
		f.episode("slow-tests", fmt.Sprint("slow ", i))
	}

	rep := f.run()
	if len(rep.Results) != 3 || rep.Distilled() != 3 {
		t.Fatalf("results = %+v, want three distilled series", rep.Results)
	}
	// A stable order across runs, so a partial run's report is worth reading.
	if got := rep.Results[0].Subject; got != "flaky-deploy" {
		t.Fatalf("first result = %q, want the subjects in order", got)
	}
	if rep.Results[2].Subject != "slow-tests" {
		t.Fatalf("results out of order: %+v", rep.Results)
	}
	for _, res := range rep.Results[:2] {
		if res.Episodes != 3 {
			t.Fatalf("series %+v mixed scopes: %d episodes", res.Scope, res.Episodes)
		}
	}
	if len(f.seen) != 3 {
		t.Fatalf("distiller calls = %d, want one per series", len(f.seen))
	}
}

// Unsubjected items are not a series. They are every observation nobody filed
// under anything, and a lesson drawn from them would be about no topic at all.
func TestUnsubjectedItemsAreNotASeries(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		if _, err := f.store.Write(context.Background(), state.MemoryItem{
			Kind: "episode", Content: fmt.Sprint("loose note ", i),
		}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if rep := f.run(); len(rep.Results) != 0 {
		t.Fatalf("results = %+v, want none", rep.Results)
	}

	if _, err := f.pass.Subject(context.Background(), "", state.Scope{}); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("Subject(\"\") = %v, want ErrInvalid", err)
	}
	if _, err := f.pass.Subject(context.Background(), "!!!", state.Scope{}); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("Subject with an unkeyable subject = %v, want ErrInvalid", err)
	}
}

// The single-subject call is what a host uses when it knows which series just
// grew, and it takes whatever spelling the caller has in hand.
func TestSubjectConsolidatesOneSeries(t *testing.T) {
	f := newFixture(t)
	scope := state.Scope{Project: "p"}
	for i := range 3 {
		f.episode("flaky-deploy", fmt.Sprint("failure ", i), func(it *state.MemoryItem) { it.Scope = scope })
	}
	// A series in another scope, and another subject, both of which must be left
	// alone by a call that named neither.
	f.episode("flaky-deploy", "elsewhere")
	f.episode("slow-tests", "unrelated")

	res, err := f.pass.Subject(context.Background(), "Flaky Deploy", scope)
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	if res.Outcome != consolidate.OutcomeDistilled || res.Episodes != 3 || len(res.Retired) != 3 {
		t.Fatalf("result = %+v, want the scoped series distilled", res)
	}
	if got := f.live("slow-tests"); len(got) != 1 {
		t.Fatalf("another subject was touched: %v", got)
	}
	if len(f.seen) != 1 || f.seen[0].Subject != "flaky-deploy" {
		t.Fatalf("distiller saw %+v", f.seen)
	}
}

// The host's own vocabulary: which kinds are a series, and what a lesson is
// called.
func TestKindsAreTheHostsToName(t *testing.T) {
	f := newFixture(t,
		consolidate.WithEpisodeKinds("incident", "attempt"),
		consolidate.WithLessonKind("runbook"),
		// Ignored: a pass that read no kinds would sweep the store and do nothing,
		// and a lesson with no kind would be unreadable by kind.
		consolidate.WithEpisodeKinds(), consolidate.WithLessonKind(""))
	for _, kind := range []string{"incident", "attempt", "incident"} {
		f.episode("flaky-deploy", kind+" happened", func(it *state.MemoryItem) { it.Kind = kind })
	}
	// An episode of the built-in kind, which this pass was told not to read.
	f.episode("flaky-deploy", "an ordinary episode")

	rep := f.run()
	if len(rep.Results) != 1 || rep.Results[0].Outcome != consolidate.OutcomeDistilled {
		t.Fatalf("results = %+v, want the host's kinds consolidated", rep.Results)
	}
	if rep.Results[0].Lesson.Kind != "runbook" {
		t.Fatalf("lesson kind = %q, want the host's", rep.Results[0].Lesson.Kind)
	}
	if got := f.live("flaky-deploy"); len(got) != 2 || !containsID(got, "an ordinary episode") {
		t.Fatalf("live = %v, want the lesson and the unread episode", got)
	}
}

// A pass needs a store and a distiller, and says so at construction rather than
// on the first nightly run.
func TestNewRequiresAStoreAndADistiller(t *testing.T) {
	d := consolidate.DistillerFunc(func(context.Context, consolidate.Series) (consolidate.Lesson, error) {
		return consolidate.Lesson{}, nil
	})
	if _, err := consolidate.New(nil, d); !errors.Is(err, consolidate.ErrNoDistiller) {
		t.Fatalf("New with no store = %v, want ErrNoDistiller", err)
	}
	p := state.NewMemory()
	t.Cleanup(func() { _ = p.Close() })
	if _, err := consolidate.New(p.Memory(), nil); !errors.Is(err, consolidate.ErrNoDistiller) {
		t.Fatalf("New with no distiller = %v, want ErrNoDistiller", err)
	}
}

func containsID(hay []string, want string) bool {
	for _, h := range hay {
		if h == want {
			return true
		}
	}
	return false
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalAnchors(got, want []state.Anchor) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
