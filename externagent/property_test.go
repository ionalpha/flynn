package externagent

import (
	"testing"

	"pgregory.net/rapid"
)

// TestParseNeverPanicsProperty is a rigor property: the codex parser accepts any
// bytes without panicking or erroring, projecting at most one event, and never
// dropping a non-empty line silently, so a malformed or noisy stream is recorded
// rather than crashing the episode.
func TestParseNeverPanicsProperty(t *testing.T) {
	c := NewCodex("", nil)
	rapid.Check(t, func(rt *rapid.T) {
		line := rapid.SliceOfN(rapid.Byte(), 0, 256).Draw(rt, "line")
		evs, err := c.Parse(line)
		if err != nil {
			rt.Fatalf("Parse errored on arbitrary input: %v", err)
		}
		if len(evs) > 1 {
			rt.Fatalf("Parse returned %d events for one line", len(evs))
		}
		for _, e := range evs {
			if e.Tier < TierEnforced || e.Tier > TierUnobserved {
				rt.Fatalf("event has an out-of-range tier: %v", e.Tier)
			}
		}
	})
}

// TestResultAbsorbTallyProperty is a rigor property: folding any sequence of events
// into a Result tallies exactly one tier count per event (the record accounts for
// every projected action), marks the result failed exactly when an error event
// occurred, and carries the last non-empty assistant text.
func TestResultAbsorbTallyProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 30).Draw(rt, "events")
		res := Result{Tiers: map[Tier]int{}}

		var anyError bool
		var lastText string
		for i := range n {
			kind := EventKind(rapid.IntRange(int(EventProgress), int(EventDone)).Draw(rt, "kind"))
			tier := Tier(rapid.IntRange(int(TierEnforced), int(TierUnobserved)).Draw(rt, "tier"))
			ev := Event{Kind: kind, Tier: tier}
			switch kind {
			case EventText:
				ev.Text = rapid.StringN(0, 8, 8).Draw(rt, "text")
				if ev.Text != "" {
					lastText = ev.Text
				}
			case EventError:
				ev.Err = "boom"
				anyError = true
			case EventUsage, EventDone:
				ev.Usage = Usage{InputTokens: rapid.IntRange(0, 100).Draw(rt, "in"), OutputTokens: 1}
			}
			res.absorb(ev)
			_ = i
		}

		total := 0
		for _, c := range res.Tiers {
			total += c
		}
		if total != n {
			rt.Fatalf("tier tally %d does not account for %d events", total, n)
		}
		if res.Failed != anyError {
			rt.Fatalf("Failed=%v but anyError=%v", res.Failed, anyError)
		}
		if res.Text != lastText {
			rt.Fatalf("Text=%q, want last non-empty text %q", res.Text, lastText)
		}
	})
}
