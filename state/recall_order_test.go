package state_test

import (
	"testing"
	"time"

	"github.com/ionalpha/flynn/state"
)

// SortRecall is the one place the recall ordering contract is implemented, and
// every backend sorts through it. Testing it directly is the only way to reach
// the key precedence for real: against the bundled backends every match scores 1,
// so a conformance run can never separate two items by score, and the SQLite
// store orders in SQL rather than here.
func TestSortRecallKeyPrecedence(t *testing.T) {
	var (
		t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		t1 = t0.Add(time.Hour)

		project   = state.Scope{Instance: "i", Project: "p"}
		workspace = state.Scope{Instance: "i", Project: "p", Workspace: "w"}
	)
	// One item per axis, so which key wins is visible in the resulting order.
	var (
		oldDeepWeak  = state.MemoryItem{ID: "a", Scope: workspace, CreatedAt: t0, Score: 0.1}
		newShallowOK = state.MemoryItem{ID: "b", Scope: project, CreatedAt: t1, Score: 0.9}
	)

	for _, tc := range []struct {
		name string
		q    state.RecallQuery
		want []string
	}{
		{
			// The default: no widening and no relevance, so recency alone decides.
			name: "recency",
			q:    state.RecallQuery{},
			want: []string{"b", "a"},
		},
		{
			// Widened, so the deeper scope wins even though it is older.
			name: "scope before recency",
			q:    state.RecallQuery{Scope: workspace, IncludeAncestors: true},
			want: []string{"a", "b"},
		},
		{
			// Relevance outranks both: the better match wins despite the shallower
			// scope, so a floor plus a limit really is top-K by relevance.
			name: "relevance before scope",
			q: state.RecallQuery{
				Scope: workspace, IncludeAncestors: true, Order: state.OrderRelevance,
			},
			want: []string{"b", "a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := []state.MemoryItem{oldDeepWeak, newShallowOK}
			state.SortRecall(tc.q, items)
			got := []string{items[0].ID, items[1].ID}
			if got[0] != tc.want[0] || got[1] != tc.want[1] {
				t.Fatalf("SortRecall = %v, want %v", got, tc.want)
			}
		})
	}
}

// Equal on every ranked key, items fall back to ID so the order is total: two
// backends returning the same set in different internal orders still agree.
func TestSortRecallBreaksTiesByID(t *testing.T) {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	items := []state.MemoryItem{
		{ID: "c", CreatedAt: at, Score: 1},
		{ID: "a", CreatedAt: at, Score: 1},
		{ID: "b", CreatedAt: at, Score: 1},
	}
	state.SortRecall(state.RecallQuery{Order: state.OrderRelevance}, items)
	for i, want := range []string{"a", "b", "c"} {
		if items[i].ID != want {
			t.Fatalf("tie-break order = %v, want a, b, c", []string{items[0].ID, items[1].ID, items[2].ID})
		}
	}
}
