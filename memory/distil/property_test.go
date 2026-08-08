package distil

import (
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/memory/guard"
)

// TestPropReplyIsAlwaysALessonOrADecline pins that reading a reply is total.
//
// The distiller is called once per subject over a sweep, so a reply it cannot
// make sense of must not fail the subject: an error means a series stays
// unconsolidated and a report row nobody asked for, where a decline means a
// re-read on the next run. Whatever the model says, the answer is either exactly
// the trimmed reply or nothing.
func TestPropReplyIsAlwaysALessonOrADecline(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		reply := rapid.String().Draw(rt, "reply")
		m := llmtest.NewScripted(llmtest.SayText(reply))

		got, err := New(m).Distil(t.Context(), series(3))
		if err != nil {
			rt.Fatalf("Distil(%q) = error %v, want a lesson or a decline", reply, err)
		}
		trimmed := strings.TrimSpace(reply)
		switch {
		case got.Content == "":
			// A decline is only correct for a reply that says nothing, says the
			// decline token, or smuggles something.
			if trimmed != "" && !strings.EqualFold(trimmed, declineToken) && !smuggles(trimmed) {
				rt.Fatalf("Distil(%q) declined a usable reply", reply)
			}
		case got.Content != trimmed:
			rt.Fatalf("Distil(%q) = %q, want the trimmed reply verbatim", reply, got.Content)
		case smuggles(trimmed):
			rt.Fatalf("Distil(%q) returned a lesson carrying a hidden-instruction payload", reply)
		}
	})
}

// smuggles reports whether s carries a structural hidden-instruction payload.
func smuggles(s string) bool {
	for _, f := range guard.Screen(s) {
		if f.Structural() {
			return true
		}
	}
	return false
}

// TestPropWindowKeepsBothEnds pins what a windowed series still shows: the cap
// is honoured, the oldest and newest episodes always survive it, and the order
// is the series' own. The two ends are the whole reason a series is worth
// distilling, so a window that dropped either would leave the model reading a
// middle it cannot date.
func TestPropWindowKeepsBothEnds(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 60).Draw(rt, "episodes")
		limit := rapid.IntRange(2, 30).Draw(rt, "cap")
		in := series(n)

		w := New(llmtest.NewScripted(), WithMaxEpisodes(limit)).window(in.Episodes)

		if want := min(n, limit); len(w.shown) != want {
			rt.Fatalf("shown = %d episodes, want %d (of %d, cap %d)", len(w.shown), want, n, limit)
		}
		if w.elided != n-len(w.shown) {
			rt.Fatalf("elided = %d, want %d", w.elided, n-len(w.shown))
		}
		if w.shown[0].ID != in.Episodes[0].ID {
			rt.Fatalf("oldest shown = %s, want %s", w.shown[0].ID, in.Episodes[0].ID)
		}
		last := w.shown[len(w.shown)-1]
		if last.ID != in.Episodes[n-1].ID {
			rt.Fatalf("newest shown = %s, want %s", last.ID, in.Episodes[n-1].ID)
		}
		if w.head < 0 || w.head > len(w.shown) {
			rt.Fatalf("head = %d, outside the shown episodes (%d)", w.head, len(w.shown))
		}
		// Order is the information a lesson is drawn from, so what survives the
		// window is still oldest first.
		for i := 1; i < len(w.shown); i++ {
			if !w.shown[i-1].CreatedAt.Before(w.shown[i].CreatedAt) {
				rt.Fatalf("episodes %d and %d are out of order", i-1, i)
			}
		}
	})
}

// TestPropBudgetIsNeverOverspent pins that a call cap is a cap: over any
// sequence of subjects, the model is called at most that many times and every
// call past it is a decline rather than a failure, so an exhausted sweep leaves
// the series it did not reach for the next run.
func TestPropBudgetIsNeverOverspent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		budget := rapid.IntRange(0, 6).Draw(rt, "budget")
		subjects := rapid.IntRange(0, 12).Draw(rt, "subjects")

		turns := make([]llm.Response, budget)
		for i := range turns {
			turns[i] = llmtest.SayText("a lesson")
		}
		m := llmtest.NewScripted(turns...)
		d := New(m, WithMaxCalls(budget))

		lessons := 0
		for range subjects {
			got, err := d.Distil(t.Context(), series(3))
			if err != nil {
				rt.Fatalf("Distil: %v, want a decline once the budget is gone", err)
			}
			if got.Content != "" {
				lessons++
			}
		}
		if want := min(budget, subjects); m.Calls() != want || lessons != want {
			rt.Fatalf("calls = %d and lessons = %d over %d subjects on a budget of %d, want %d of each",
				m.Calls(), lessons, subjects, budget, want)
		}
	})
}
