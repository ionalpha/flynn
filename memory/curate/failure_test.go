package curate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/memory/curate"
	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

// stubStore overrides one method of a real store so a failure can be injected
// without reimplementing the whole interface.
type stubStore struct {
	state.MemoryStore
	recallErr error
	deleteErr error
	writeErr  func(m state.MemoryItem) error
}

func (s stubStore) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	if s.recallErr != nil {
		return nil, s.recallErr
	}
	return s.MemoryStore.Recall(ctx, q)
}

func (s stubStore) Delete(ctx context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.MemoryStore.Delete(ctx, id)
}

func (s stubStore) Write(ctx context.Context, m state.MemoryItem) (state.MemoryItem, error) {
	if s.writeErr != nil {
		if err := s.writeErr(m); err != nil {
			return state.MemoryItem{}, err
		}
	}
	return s.MemoryStore.Write(ctx, m)
}

// newStubbed returns a curating store over a stub, and the inner store the stub
// wraps, so a test can read what actually landed.
func newStubbed(t *testing.T, stub func(*stubStore), opts ...curate.Option) (*curate.Store, state.MemoryStore) {
	t.Helper()
	p := state.NewMemory()
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Fatalf("close provider: %v", err)
		}
	})
	s := stubStore{MemoryStore: p.Memory()}
	stub(&s)
	return curate.Wrap(s, opts...), p.Memory()
}

// The policy reads the subject's series before it decides anything, so a store
// that cannot answer that read has to fail the write. Guessing append and writing
// anyway would leave a replacement that never replaced.
func TestWriteFailsWhenTheSeriesCannotBeRead(t *testing.T) {
	boom := errors.New("recall unavailable")
	st, inner := newStubbed(t, func(s *stubStore) { s.recallErr = boom })

	if _, err := st.Write(context.Background(), state.MemoryItem{
		Kind: "decision", Subject: "db-choice", Content: "Postgres",
	}); !errors.Is(err, boom) {
		t.Fatalf("write = %v, want the store's recall error", err)
	}
	items, err := inner.Recall(context.Background(), state.RecallQuery{})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("a write that could not read its series stored %d items, want none", len(items))
	}
}

// The replacement is stored before the item it replaces is retired, so a delete
// that fails leaves both live with the link recorded. The error names the item
// that is still there, and the stored replacement comes back with it: reporting a
// bare failure would invite a retry that writes the fact a second time.
func TestRetirementFailureReturnsTheStoredReplacement(t *testing.T) {
	boom := errors.New("delete unavailable")
	st, inner := newStubbed(t, func(s *stubStore) { s.deleteErr = boom })
	ctx := context.Background()

	first, err := st.Write(ctx, state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "MySQL"})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	second, err := st.Write(ctx, state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "Postgres"})
	if !errors.Is(err, boom) {
		t.Fatalf("write = %v, want the store's delete error", err)
	}
	if second.ID == "" || second.Content != "Postgres" {
		t.Fatalf("write returned %+v, want the stored replacement alongside the error", second)
	}
	if len(second.Supersedes) != 1 || second.Supersedes[0] != first.ID {
		t.Fatalf("the stored replacement supersedes %v, want [%s]", second.Supersedes, first.ID)
	}
	items, err := inner.Recall(ctx, state.RecallQuery{Subjects: []string{"db-choice"}})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("live items = %d, want both, since the retirement failed", len(items))
	}
}

// The conflict episode is a note about the write, so losing it must not lose the
// write. The conclusion is real memory and is returned; the error says the note
// did not land.
func TestConflictEpisodeFailureReturnsTheStoredWrite(t *testing.T) {
	boom := errors.New("write unavailable")
	st, inner := newStubbed(t, func(s *stubStore) {
		s.writeErr = func(m state.MemoryItem) error {
			if m.Kind == curate.KindConflict {
				return boom
			}
			return nil
		}
	})
	ctx := context.Background()

	if _, err := st.Write(ctx, state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Cloudflare",
		Sources: []string{guard.SchemeUser + "operator"},
	}); err != nil {
		t.Fatalf("the operator's fact: %v", err)
	}
	got, err := st.Write(ctx, state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Fly",
		Sources: []string{guard.SchemeAgent + "run-7"},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("write = %v, want the store's write error", err)
	}
	if got.ID == "" || got.Content != "we deploy to Fly" {
		t.Fatalf("write returned %+v, want the stored conclusion alongside the error", got)
	}
	items, err := inner.Recall(ctx, state.RecallQuery{Subjects: []string{"deploy-target"}})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("live items = %d, want the operator's fact and the conclusion", len(items))
	}
}

// A demoted write that cannot be stored at all is a plain failure: there is
// nothing to record a contradiction about.
func TestConflictWriteFailureIsReported(t *testing.T) {
	boom := errors.New("write unavailable")
	st, _ := newStubbed(t, func(s *stubStore) {
		s.writeErr = func(m state.MemoryItem) error {
			if m.Content == "we deploy to Fly" {
				return boom
			}
			return nil
		}
	})
	ctx := context.Background()

	if _, err := st.Write(ctx, state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Cloudflare",
		Sources: []string{guard.SchemeUser + "operator"},
	}); err != nil {
		t.Fatalf("the operator's fact: %v", err)
	}
	if _, err := st.Write(ctx, state.MemoryItem{
		Kind: "fact", Subject: "deploy-target", Content: "we deploy to Fly",
		Sources: []string{guard.SchemeAgent + "run-7"},
	}); !errors.Is(err, boom) {
		t.Fatalf("write = %v, want the store's write error", err)
	}
}

// A fork scan that cannot read the store loses the notice, not the write: the
// scan is an observation, and trading the memory for the warning about it would
// be the wrong way round.
func TestForkScanFailureDoesNotFailTheWrite(t *testing.T) {
	p := state.NewMemory()
	t.Cleanup(func() { _ = p.Close() })
	inner := p.Memory()
	ctx := context.Background()

	// Only the unfiltered scope read fails; the subject read the policy depends on
	// still works, which is what isolates the fork scan's failure.
	stub := recallFilterStore{MemoryStore: inner, fail: errors.New("scan unavailable")}
	var notices []curate.Notice
	st := curate.Wrap(stub, curate.WithNotify(func(_ context.Context, n curate.Notice) {
		notices = append(notices, n)
	}))

	if _, err := st.Write(ctx, state.MemoryItem{
		Kind: "decision", Subject: "db-choice", Content: "Postgres",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(notices) != 0 {
		t.Fatalf("notices = %+v, want none: the scan could not run", notices)
	}
	items, err := inner.Recall(ctx, state.RecallQuery{Subjects: []string{"db-choice"}})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("live items = %d, want the write to have landed", len(items))
	}
}

// recallFilterStore fails only the unsubjected recall, which is the one the fork
// scan takes.
type recallFilterStore struct {
	state.MemoryStore
	fail error
}

func (s recallFilterStore) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	if len(q.Subjects) == 0 {
		return nil, s.fail
	}
	return s.MemoryStore.Recall(ctx, q)
}

// Everything that is not the write path delegates untouched. The policy has an
// opinion about what a write means and none about reads, usage or promotion, and
// a decorator that quietly filtered any of them would be changing a contract it
// only meant to wrap.
func TestEverythingElseDelegates(t *testing.T) {
	p := state.NewMemory()
	t.Cleanup(func() { _ = p.Close() })
	inner := p.Memory()
	st := curate.Wrap(inner)
	ctx := context.Background()

	it, err := st.Write(ctx, state.MemoryItem{Kind: "fact", Subject: "db-choice", Content: "Postgres"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := st.RecordPush(ctx, []string{it.ID}); err != nil {
		t.Fatalf("record push: %v", err)
	}
	if err := st.RecordUse(ctx, it.ID, state.UsagePrimed); err != nil {
		t.Fatalf("record use: %v", err)
	}
	usage, err := st.Usage(ctx, []string{it.ID})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(usage) != 1 || usage[0].PushCount != 1 || usage[0].PrimedUses != 1 {
		t.Fatalf("usage = %+v, want one row counting the push and the use", usage)
	}
	if _, err := st.Promote(ctx, state.PromotionDecision{
		MemoryID: it.ID, By: "operator", Promoted: true,
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	promotions, err := st.Promotions(ctx, []string{it.ID})
	if err != nil {
		t.Fatalf("promotions: %v", err)
	}
	if len(promotions) != 1 || !promotions[0].Promoted {
		t.Fatalf("promotions = %+v, want the decision this store recorded", promotions)
	}
	if err := st.Delete(ctx, it.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	items, err := st.Recall(ctx, state.RecallQuery{})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("recall after the delete = %d items, want none", len(items))
	}
}

// A nil option is ignored rather than installed, so a host wiring options from
// configuration cannot switch the measure off by passing nothing.
func TestNilOptionsAreIgnored(t *testing.T) {
	p := state.NewMemory()
	t.Cleanup(func() { _ = p.Close() })
	st := curate.Wrap(p.Memory(),
		curate.WithNotify(nil), curate.WithSimilarity(nil), curate.WithClass("", curate.ClassReplace))
	ctx := context.Background()

	if _, err := st.Write(ctx, state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "MySQL"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := st.Write(ctx, state.MemoryItem{Kind: "decision", Subject: "db-choice", Content: "Postgres"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	items, err := st.Recall(ctx, state.RecallQuery{Subjects: []string{"db-choice"}})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(items) != 1 || items[0].Content != "Postgres" {
		t.Fatalf("live = %+v, want the replacement alone", items)
	}
	// The empty kind was not installed as a replace kind, so an unkinded write
	// still appends.
	if got := st.ClassOf(""); got != curate.ClassAppend {
		t.Fatalf("ClassOf(\"\") = %v, want append", got)
	}
}
