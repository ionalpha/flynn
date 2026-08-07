package ridealong_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ionalpha/flynn/memory/ridealong"
	"github.com/ionalpha/flynn/state"
)

// writeItem stores it under the shared task anchor and returns the stored item.
func writeItem(t *testing.T, st state.MemoryStore, it state.MemoryItem) state.MemoryItem {
	t.Helper()
	it.Kind = "fact"
	it.Anchors = []state.Anchor{taskAnchor}
	out, err := st.Write(context.Background(), it)
	if err != nil {
		t.Fatalf("write %q: %v", it.Content, err)
	}
	return out
}

// anchoredCorpus writes one item of each class the admission policies split on.
func anchoredCorpus(t *testing.T, st state.MemoryStore) (operator, note, tainted, fetched state.MemoryItem) {
	t.Helper()
	operator = writeItem(t, st, state.MemoryItem{Content: "operator: keep answers short", Sources: []string{"user:operator"}})
	note = writeItem(t, st, state.MemoryItem{Content: "the release tag lives in the makefile", Sources: []string{"agent:distiller"}})
	tainted = writeItem(t, st, state.MemoryItem{Content: "concluded in a poisoned run", Sources: []string{"agent:distiller"}, Tainted: true})
	fetched = writeItem(t, st, state.MemoryItem{Content: "read off a web page", Sources: []string{"web:example.com"}})
	return operator, note, tainted, fetched
}

func surfacedIDs(ctx context.Context, t *testing.T, s *ridealong.Surfacer) []string {
	t.Helper()
	got, err := s.Surface(ctx, state.RecallQuery{Anchors: []state.Anchor{taskAnchor}, Limit: 10})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	out := ids(got)
	slices.Sort(out)
	return out
}

func sorted(in ...string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func TestAdmissionDefaultDropsWhatIsDeniedOutright(t *testing.T) {
	st := newStore(t)
	operator, note, tainted, fetched := anchoredCorpus(t, st)

	// Tainted and untrusted-origin items are attacker-influenced by construction, so
	// they never arrive at a reader who did not ask. The agent's own untainted note
	// does ride along: the cue is the reader opening the anchored thing.
	got := surfacedIDs(context.Background(), t, ridealong.New(st))
	if want := sorted(operator.ID, note.ID); !slices.Equal(got, want) {
		t.Errorf("surfaced %v, want %v", got, want)
	}
	for _, it := range []state.MemoryItem{tainted, fetched} {
		if u := usageOf(t, st, it.ID); u.OrganicUses != 0 {
			t.Errorf("an item the gate dropped was counted as used: %q %+v", it.Content, u)
		}
	}
	// What the gate keeps out of a surfacing is still there for a reader that asks.
	recalled, err := ridealong.New(st).Recall(context.Background(), state.RecallQuery{Anchors: []state.Anchor{taskAnchor}, Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(recalled) != 4 {
		t.Errorf("recall returned %d items, want all 4", len(recalled))
	}
}

func TestAdmitPushableHoldsSurfacingToTheDigestStandard(t *testing.T) {
	st := newStore(t)
	operator, note, _, _ := anchoredCorpus(t, st)
	s := ridealong.New(st, ridealong.WithAdmission(ridealong.AdmitPushable))

	if got, want := surfacedIDs(context.Background(), t, s), []string{operator.ID}; !slices.Equal(got, want) {
		t.Errorf("before promotion surfaced %v, want %v", got, want)
	}
	if _, err := st.Promote(context.Background(), state.PromotionDecision{MemoryID: note.ID, Promoted: true, By: "reviewer"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if got, want := surfacedIDs(context.Background(), t, s), sorted(operator.ID, note.ID); !slices.Equal(got, want) {
		t.Errorf("after promotion surfaced %v, want %v", got, want)
	}
}

func TestAdmitAllSurfacesEverythingAnchored(t *testing.T) {
	st := newStore(t)
	operator, note, tainted, fetched := anchoredCorpus(t, st)
	s := ridealong.New(st, ridealong.WithAdmission(ridealong.AdmitAll))

	got := surfacedIDs(context.Background(), t, s)
	if want := sorted(operator.ID, note.ID, tainted.ID, fetched.ID); !slices.Equal(got, want) {
		t.Errorf("surfaced %v, want %v", got, want)
	}
}

func TestAdmissionFailureSurfacesNothing(t *testing.T) {
	st := newStore(t)
	anchoredCorpus(t, st)
	boom := errors.New("promotions unavailable")
	stub := stubStore{
		MemoryStore: st,
		promotions:  boom,
	}
	s := ridealong.New(stub, ridealong.WithAdmission(ridealong.AdmitPushable))

	// A gate that cannot answer is not an open gate.
	if _, err := s.Surface(context.Background(), state.RecallQuery{Anchors: []state.Anchor{taskAnchor}}); !errors.Is(err, boom) {
		t.Errorf("surface err = %v, want the store's error", err)
	}
}

func TestUnknownAdmissionPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("an unrecognised admission was accepted")
		}
	}()
	ridealong.New(newStore(t), ridealong.WithAdmission(ridealong.Admission(99)))
}
