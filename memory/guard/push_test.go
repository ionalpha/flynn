package guard_test

import (
	"slices"
	"testing"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

func TestPushEligibility(t *testing.T) {
	cases := []struct {
		name string
		item state.MemoryItem
		want guard.Eligibility
	}{
		{"operator's own", state.MemoryItem{Sources: []string{"user:operator"}}, guard.PushAllowed},
		{"agent's own note", state.MemoryItem{Sources: []string{"agent:distiller"}}, guard.PushOnPromotion},
		{"bare run id", state.MemoryItem{Sources: []string{"run-42"}}, guard.PushOnPromotion},
		{"no provenance at all", state.MemoryItem{}, guard.PushOnPromotion},
		{"tool output", state.MemoryItem{Sources: []string{"tool:shell"}}, guard.PushDenied},
		{"fetched page", state.MemoryItem{Sources: []string{"web:example.com"}}, guard.PushDenied},
		{"inbound message", state.MemoryItem{Sources: []string{"inbound:signal"}}, guard.PushDenied},
		// The weakest source decides, so one trusted co-author launders nothing.
		{"operator mixed with a page", state.MemoryItem{Sources: []string{"user:operator", "web:example.com"}}, guard.PushDenied},
		// Taint outranks every provenance answer, including the operator's own.
		{"tainted operator note", state.MemoryItem{Sources: []string{"user:operator"}, Tainted: true}, guard.PushDenied},
		{"tainted agent note", state.MemoryItem{Sources: []string{"agent:distiller"}, Tainted: true}, guard.PushDenied},
	}
	for _, c := range cases {
		if got := guard.PushEligibility(c.item); got != c.want {
			t.Errorf("%s: PushEligibility = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPushEligibleAgainstPromotion(t *testing.T) {
	operator := state.MemoryItem{Sources: []string{"user:operator"}}
	agent := state.MemoryItem{Sources: []string{"agent:distiller"}}
	fetched := state.MemoryItem{Sources: []string{"web:example.com"}}
	tainted := state.MemoryItem{Sources: []string{"user:operator"}, Tainted: true}

	cases := []struct {
		name     string
		item     state.MemoryItem
		promoted bool
		want     bool
	}{
		{"operator, unpromoted", operator, false, true},
		{"operator, promoted", operator, true, true},
		{"agent, unpromoted", agent, false, false},
		{"agent, promoted", agent, true, true},
		// Denial is terminal: promotion is a reviewer reading the finished sentence,
		// which is precisely what an attacker got to write.
		{"fetched, promoted", fetched, true, false},
		{"tainted, promoted", tainted, true, false},
	}
	for _, c := range cases {
		if got := guard.PushEligible(c.item, c.promoted); got != c.want {
			t.Errorf("%s: PushEligible = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEligibilityString(t *testing.T) {
	cases := map[guard.Eligibility]string{
		guard.PushAllowed:     "allowed",
		guard.PushDenied:      "denied",
		guard.PushOnPromotion: "on-promotion",
	}
	for e, want := range cases {
		if got := e.String(); got != want {
			t.Errorf("Eligibility(%d).String() = %q, want %q", int(e), got, want)
		}
	}
}

func TestFilterPushable(t *testing.T) {
	items := []state.MemoryItem{
		{ID: "a", Content: "operator preference", Sources: []string{"user:operator"}},
		{ID: "b", Content: "agent note, promoted", Sources: []string{"agent:distiller"}},
		{ID: "c", Content: "agent note, unreviewed", Sources: []string{"agent:distiller"}},
		{ID: "d", Content: "fetched, promoted anyway", Sources: []string{"web:example.com"}},
		{ID: "e", Content: "laundered", Sources: []string{"agent:distiller"}, Tainted: true},
	}
	promoted := func(id string) bool { return id == "b" || id == "d" || id == "e" }

	got := ids(guard.FilterPushable(items, promoted))
	if want := []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("FilterPushable = %v, want %v", got, want)
	}
	// A nil promotion lookup is the host with no promotion flow: the operator's own
	// untainted memories and nothing else.
	got = ids(guard.FilterPushable(items, nil))
	if want := []string{"a"}; !slices.Equal(got, want) {
		t.Errorf("FilterPushable with no promotions = %v, want %v", got, want)
	}
	if got := guard.FilterPushable(nil, promoted); len(got) != 0 {
		t.Errorf("FilterPushable of nothing = %v, want empty", got)
	}
}

func ids(in []state.MemoryItem) []string {
	out := make([]string, 0, len(in))
	for _, it := range in {
		out = append(out, it.ID)
	}
	return out
}
