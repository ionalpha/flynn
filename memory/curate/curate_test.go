package curate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/memory/curate"
	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

// epoch is the start of every test's manual clock, so CreatedAt is whatever the
// test advanced it to and never the wall clock.
var epoch = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// fixture is a curating store over an in-memory provider, with the notices it
// reported collected for assertion.
type fixture struct {
	t       *testing.T
	store   *curate.Store
	inner   state.MemoryStore
	clk     *clock.Manual
	notices []curate.Notice
}

func newFixture(t *testing.T, opts ...curate.Option) *fixture {
	t.Helper()
	clk := clock.NewManual(epoch)
	p := state.NewMemory(state.WithClock(clk))
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Fatalf("close provider: %v", err)
		}
	})
	f := &fixture{t: t, inner: p.Memory(), clk: clk}
	opts = append([]curate.Option{curate.WithNotify(func(_ context.Context, n curate.Notice) {
		f.notices = append(f.notices, n)
	})}, opts...)
	f.store = curate.Wrap(f.inner, opts...)
	return f
}

// write persists through the policy and advances the clock, so every item has a
// distinct CreatedAt and recency order is the write order reversed.
func (f *fixture) write(it state.MemoryItem) state.MemoryItem {
	f.t.Helper()
	out, err := f.store.Write(context.Background(), it)
	if err != nil {
		f.t.Fatalf("write %q: %v", it.Content, err)
	}
	f.clk.Advance(time.Minute)
	return out
}

// live returns the live contents under a subject in a scope, most recent first.
func (f *fixture) live(subject string, scope state.Scope) []string {
	f.t.Helper()
	items, err := f.store.Recall(context.Background(), state.RecallQuery{
		Subjects: []string{subject}, Scope: scope,
	})
	if err != nil {
		f.t.Fatalf("recall %q: %v", subject, err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.Scope == scope {
			out = append(out, it.Content)
		}
	}
	return out
}

func (f *fixture) wantLive(what, subject string, scope state.Scope, want ...string) {
	f.t.Helper()
	got := f.live(subject, scope)
	if len(got) != len(want) {
		f.t.Fatalf("%s: live on %q = %v, want %v", what, subject, got, want)
	}
	for _, w := range want {
		if !contains(got, w) {
			f.t.Fatalf("%s: live on %q = %v, want it to include %q", what, subject, got, w)
		}
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// A state kind asserts one current answer, so the second write of it retires the
// first and says so. Both halves matter: a store that superseded without retiring
// would hand a reader two live answers, and one that retired without recording
// the link would lose what the correction corrected.
func TestReplaceKindSupersedesAndRetires(t *testing.T) {
	f := newFixture(t)
	first := f.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "we are going with MySQL"})
	second := f.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "we are going with Postgres"})

	f.wantLive("after the replacement", "db-choice", state.Scope{}, "we are going with Postgres")
	if len(second.Supersedes) != 1 || second.Supersedes[0] != first.ID {
		t.Fatalf("replacement supersedes %v, want [%s]", second.Supersedes, first.ID)
	}
	// The retired item is tombstoned, not erased: it is gone from recall and its id
	// still resolves through the chain the replacement recorded.
	if err := f.store.Delete(context.Background(), first.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("deleting the already-retired item = %v, want ErrNotFound", err)
	}
	if len(f.notices) != 0 {
		t.Fatalf("an ordinary replacement reported %+v, want nothing", f.notices)
	}
}

// The subject is the key, so the same kind under a different subject is a
// different answer and retires nothing. A store that keyed on kind alone would
// let a decision about the queue delete the decision about the database.
func TestReplaceIsScopedToTheSubject(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "Postgres"})
	f.write(state.MemoryItem{Kind: "decision", Subject: "queue-choice", Content: "NATS"})

	f.wantLive("the first subject", "db-choice", state.Scope{}, "Postgres")
	f.wantLive("the second subject", "queue-choice", state.Scope{}, "NATS")
}

// Two kinds on one subject are two different assertions about it. A decision
// about the database does not retire the preference stated about it, or a reader
// asking for the standing preference finds it gone with nothing saying why.
func TestReplaceIsScopedToTheKind(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Kind: "preference", Subject: "db-choice", Content: "the operator likes boring databases"})
	f.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "we are going with Postgres"})

	f.wantLive("both kinds on one subject", "db-choice", state.Scope{},
		"the operator likes boring databases", "we are going with Postgres")
}

// Scope is matched exactly, never widened. A workspace's own decision overrides
// the project-wide one for its own reads, through recall's most-specific-first
// order, and must not retire it: the outer decision is still in force for every
// other workspace under the project.
func TestReplaceDoesNotCrossScopes(t *testing.T) {
	f := newFixture(t)
	project := state.Scope{Project: "p"}
	workspace := state.Scope{Project: "p", Workspace: "w"}

	f.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Scope: project, Content: "the project uses Postgres"})
	f.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Scope: workspace, Content: "this workspace uses SQLite"})

	f.wantLive("the project-wide decision", "db-choice", project, "the project uses Postgres")
	f.wantLive("the workspace decision", "db-choice", workspace, "this workspace uses SQLite")
}

// The episode kinds are the whole point of the split. The fifth failure matters
// because there were four before it, so appending has to keep every one of them,
// and none of them may record a supersession that a consolidation pass would then
// read as a distillation somebody already did.
func TestAppendKindKeepsTheSeries(t *testing.T) {
	f := newFixture(t)
	for _, content := range []string{"failure one", "failure two", "failure three"} {
		got := f.write(state.MemoryItem{Kind: "episode", Subject: "flaky-deploy", Content: content})
		if len(got.Supersedes) != 0 {
			t.Fatalf("appended %q supersedes %v, want nothing", content, got.Supersedes)
		}
	}
	f.wantLive("the series", "flaky-deploy", state.Scope{}, "failure one", "failure two", "failure three")
}

// An unknown kind appends. Guessing replace on a vocabulary this package does not
// know would delete the record to find out the guess was wrong, and appending is
// the answer that cannot lose anything.
func TestUnknownKindAppends(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Kind: "runbook", Subject: "deploy-steps", Content: "first version"})
	f.write(state.MemoryItem{Kind: "runbook", Subject: "deploy-steps", Content: "second version"})
	f.wantLive("an unknown kind", "deploy-steps", state.Scope{}, "first version", "second version")

	// Which the host overrides when the kind is its own and it knows better.
	g := newFixture(t, curate.WithClass("runbook", curate.ClassReplace))
	g.write(state.MemoryItem{Kind: "runbook", Subject: "deploy-steps", Content: "first version"})
	g.write(state.MemoryItem{Kind: "runbook", Subject: "deploy-steps", Content: "second version"})
	g.wantLive("a kind the host declared a replace kind", "deploy-steps", state.Scope{}, "second version")

	if got := g.store.ClassOf("runbook"); got != curate.ClassReplace {
		t.Fatalf("ClassOf(runbook) = %v, want replace", got)
	}
	if got := g.store.ClassOf("episode"); got != curate.ClassAppend {
		t.Fatalf("ClassOf(episode) = %v, want append", got)
	}
}

// An item with no subject has no series to reason about, so the policy has no
// question to answer and the write passes through untouched.
func TestUnsubjectedWritePassesThrough(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Kind: "fact", Content: "the build was slow today"})
	f.write(state.MemoryItem{Kind: "fact", Content: "the build was slow again"})

	items, err := f.store.Recall(context.Background(), state.RecallQuery{Subjects: []string{""}})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("unsubjected writes = %d items, want both", len(items))
	}
}

// The subject a caller passes is normalized before anything keys on it, so a
// second write spelled differently lands on the same chain rather than starting
// its own. A store that keyed on the raw string would fork silently and read back
// as two live answers.
func TestSubjectIsNormalizedBeforeKeying(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Kind: "decision", Subject: "DB Choice", Content: "MySQL"})
	second := f.write(state.MemoryItem{Kind: "decision", Subject: "db_choice", Content: "Postgres"})

	if second.Subject != "db-choice" {
		t.Fatalf("stored subject = %q, want db-choice", second.Subject)
	}
	f.wantLive("after a differently spelled rewrite", "db-choice", state.Scope{}, "Postgres")

	// A subject with nothing to key on is refused here, before any read of the
	// store, the same answer the record itself gives.
	if _, err := f.store.Write(context.Background(), state.MemoryItem{
		Kind: "decision", Subject: "!!!", Content: "unkeyable",
	}); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("write with an unkeyable subject = %v, want ErrInvalid", err)
	}
}

// The protection that matters most: an agent's own conclusion may not quietly
// retire what the operator said. The conclusion is still stored, because refusing
// it would lose a real observation, and the contradiction is recorded on the same
// subject so the curator's read of it finds both sides.
func TestAgentWriteDoesNotReplaceAMoreTrustedFact(t *testing.T) {
	f := newFixture(t)
	stated := f.write(state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Cloudflare",
		Sources: []string{guard.SchemeUser + "operator"},
	})
	concluded := f.write(state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Fly",
		Sources: []string{guard.SchemeAgent + "run-7"},
	})

	if len(concluded.Supersedes) != 0 {
		t.Fatalf("the demoted write supersedes %v, want nothing", concluded.Supersedes)
	}
	live := f.live("deploy-target", state.Scope{})
	if !contains(live, "we deploy to Cloudflare") {
		t.Fatalf("the operator's fact was retired: %v", live)
	}
	if !contains(live, "we deploy to Fly") {
		t.Fatalf("the agent's conclusion was lost: %v", live)
	}

	// The conflict is a record in the store, not only a callback: a host with no
	// callback registered still has to be able to find it later.
	var conflict state.MemoryItem
	items, err := f.store.Recall(context.Background(), state.RecallQuery{
		Subjects: []string{"deploy-target"}, Kinds: []string{curate.KindConflict},
	})
	if err != nil {
		t.Fatalf("recall the conflict: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("conflicts recorded = %d, want 1", len(items))
	}
	conflict = items[0]
	if !strings.Contains(conflict.Content, stated.ID) {
		t.Fatalf("the conflict episode does not name the fact it contradicts: %q", conflict.Content)
	}
	if !strings.Contains(conflict.Content, "we deploy to Fly") {
		t.Fatalf("the conflict episode does not carry the conclusion: %q", conflict.Content)
	}

	if len(f.notices) != 1 {
		t.Fatalf("notices = %+v, want one cross-trust notice", f.notices)
	}
	n := f.notices[0]
	if n.Kind != curate.NoticeCrossTrust || n.Subject != "deploy-target" ||
		n.Conflicting.ID != stated.ID || n.Incoming.ID != concluded.ID {
		t.Fatalf("cross-trust notice = %+v", n)
	}
}

// Equal trust replaces. The operator revising their own preference is the
// ordinary case, and a rule demanding strictly greater trust would make the first
// answer on a subject permanent.
func TestEqualTrustReplaces(t *testing.T) {
	f := newFixture(t)
	user := []string{guard.SchemeUser + "operator"}
	f.write(state.MemoryItem{Kind: "preference", Subject: "reply-length", Content: "short answers", Sources: user})
	f.write(state.MemoryItem{Kind: "preference", Subject: "reply-length", Content: "long answers now", Sources: user})

	f.wantLive("the operator revising their own preference", "reply-length", state.Scope{}, "long answers now")
	if len(f.notices) != 0 {
		t.Fatalf("an equal-trust replacement reported %+v, want nothing", f.notices)
	}
}

// Trust runs the other way too: an operator's correction retires the agent's
// earlier conclusion without any ceremony, which is the case the protection must
// not get in the way of.
func TestMoreTrustedWriteReplacesFreely(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Fly",
		Sources: []string{guard.SchemeAgent + "run-7"},
	})
	f.write(state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Cloudflare",
		Sources: []string{guard.SchemeUser + "operator"},
	})

	f.wantLive("the operator correcting the agent", "deploy-target", state.Scope{}, "we deploy to Cloudflare")
	if len(f.notices) != 0 {
		t.Fatalf("a correction by a more trusted writer reported %+v, want nothing", f.notices)
	}
}

// A subject that looks like a spelling of one already in use is reported at the
// moment it first appears, which is the only moment the fork can be caught: once
// both exist, every later read sees two well-formed chains.
func TestForkedSubjectIsFlaggedOnce(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "Postgres"})
	f.write(state.MemoryItem{Kind: "decision", Subject: "database-choice", Content: "Postgres, again"})
	// A second write to the forked subject is no longer new, so it does not report
	// the same fork a second time.
	f.write(state.MemoryItem{Kind: "decision", Subject: "database-choice", Content: "Postgres, still"})
	// An unrelated subject sharing a word is not a fork.
	f.write(state.MemoryItem{Kind: "decision", Subject: "queue-choice", Content: "NATS"})

	if len(f.notices) != 1 {
		t.Fatalf("notices = %+v, want exactly one fork notice", f.notices)
	}
	n := f.notices[0]
	if n.Kind != curate.NoticeForkedSubject || n.Subject != "database-choice" || n.Similar != "db-choice" {
		t.Fatalf("fork notice = %+v", n)
	}
}

// Fork detection is a heuristic, so it reports and never refuses: the write lands
// whatever the measure thinks, and a host that has a better measure supplies it.
func TestForkDetectionIsPluggableAndNeverRefuses(t *testing.T) {
	// A measure that calls everything a fork must not stop anything being written.
	always := newFixture(t, curate.WithSimilarity(func(_, _ string) float64 { return 1 }))
	always.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "Postgres"})
	always.write(state.MemoryItem{Kind: "decision", Subject: "queue-choice", Content: "NATS"})
	always.wantLive("a write the measure called a fork", "queue-choice", state.Scope{}, "NATS")
	if len(always.notices) != 1 {
		t.Fatalf("notices = %+v, want one", always.notices)
	}

	// A threshold above 1 is how a host with no useful measure opts out.
	off := newFixture(t, curate.WithSimilarityThreshold(1.5))
	off.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "Postgres"})
	off.write(state.MemoryItem{Kind: "decision", Subject: "database-choice", Content: "Postgres, again"})
	if len(off.notices) != 0 {
		t.Fatalf("fork detection was switched off and still reported %+v", off.notices)
	}
}

// The scan for a fork only reads subjects in the write's own scope. Two projects
// naming their own topics the same way is not a fork, and reporting it would put
// a notice in front of a person for a coincidence.
func TestForkDetectionStaysInScope(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Scope: state.Scope{Project: "a"}, Content: "Postgres"})
	f.write(state.MemoryItem{Kind: "decision", Subject: "database-choice", Scope: state.Scope{Project: "b"}, Content: "MySQL"})

	if len(f.notices) != 0 {
		t.Fatalf("a same-named subject in another scope reported %+v, want nothing", f.notices)
	}
}

// Unsubjected items are not candidates for a fork. They are the ones a store
// holds most of, and the empty subject would otherwise be the nearest thing to
// every short new subject.
func TestForkDetectionSkipsUnsubjectedItems(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Kind: "observation", Content: "the build was slow today"})
	f.write(state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "Postgres"})

	if len(f.notices) != 0 {
		t.Fatalf("an unsubjected item was treated as a fork candidate: %+v", f.notices)
	}
}

// A replacement that cannot be stored fails the write, and nothing is retired:
// the retirement runs after the write for exactly this reason, so a store that
// could not take the new answer still has the old one.
func TestReplacementWriteFailureRetiresNothing(t *testing.T) {
	boom := errors.New("write unavailable")
	st, inner := newStubbed(t, func(s *stubStore) {
		s.writeErr = func(m state.MemoryItem) error {
			if m.Content == "Postgres" {
				return boom
			}
			return nil
		}
	})
	ctx := context.Background()

	if _, err := st.Write(ctx, state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "MySQL"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := st.Write(ctx, state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "Postgres"}); !errors.Is(err, boom) {
		t.Fatalf("write = %v, want the store's write error", err)
	}
	items, err := inner.Recall(ctx, state.RecallQuery{Subjects: []string{"db-choice"}})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(items) != 1 || items[0].Content != "MySQL" {
		t.Fatalf("live = %+v, want the standing answer untouched", items)
	}
}

// The conflict episode quotes the conclusion, and one long memory must not turn
// the note about it into a copy of it.
func TestConflictEpisodeBoundsTheQuote(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("we deploy to somewhere very specific ", 20)
	f.write(state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Cloudflare",
		Sources: []string{guard.SchemeUser + "operator"},
	})
	f.write(state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: long,
		Sources: []string{guard.SchemeTool + "web-fetch"},
	})

	items, err := f.store.Recall(context.Background(), state.RecallQuery{Kinds: []string{curate.KindConflict}})
	if err != nil {
		t.Fatalf("recall the conflict: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("conflicts recorded = %d, want 1", len(items))
	}
	if got := items[0].Content; len(got) >= len(long) || !strings.Contains(got, "...") {
		t.Fatalf("the conflict episode quoted %d characters of a %d character memory", len(got), len(long))
	}
	if got := f.notices[0].Detail; !strings.Contains(got, "untrusted") {
		t.Fatalf("the notice does not name the incoming trust: %q", got)
	}
}

// The quote is bounded in characters, not bytes. A memory written in a script
// where every character is three bytes is not a long memory, and cutting it by
// byte count would truncate an ordinary sentence and could split a character in
// half on the way.
func TestConflictEpisodeBoundsTheQuoteByCharacters(t *testing.T) {
	f := newFixture(t)
	// 60 characters, 180 bytes: over any byte bound, under the character one.
	multibyte := strings.Repeat("データベース", 10)
	f.write(state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Cloudflare",
		Sources: []string{guard.SchemeUser + "operator"},
	})
	f.write(state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: multibyte,
		Sources: []string{guard.SchemeAgent + "run-7"},
	})

	items, err := f.store.Recall(context.Background(), state.RecallQuery{Kinds: []string{curate.KindConflict}})
	if err != nil {
		t.Fatalf("recall the conflict: %v", err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Content, multibyte) {
		t.Fatalf("the conflict episode did not quote the memory whole: %+v", items)
	}
}

// The class names are read by people, in a notice or a review queue.
func TestClassString(t *testing.T) {
	if got := curate.ClassReplace.String(); got != "replace" {
		t.Fatalf("ClassReplace = %q", got)
	}
	if got := curate.ClassAppend.String(); got != "append" {
		t.Fatalf("ClassAppend = %q", got)
	}
}
