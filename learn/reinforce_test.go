package learn

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/state"
)

func TestConfidence(t *testing.T) {
	if c := Confidence(0, 0); c != 0 {
		t.Fatalf("no evidence should be 0 confidence, got %v", c)
	}
	// Small-sample caution: one win out of one must rank below many wins out of
	// many, even though both have a raw win rate of 1.0.
	if Confidence(1, 1) >= Confidence(50, 50) {
		t.Fatalf("1/1 (%.3f) should rank below 50/50 (%.3f)", Confidence(1, 1), Confidence(50, 50))
	}
	// A poor record is low confidence; a strong one is high.
	if Confidence(10, 0) > 0.1 {
		t.Fatalf("0/10 should be near-zero confidence, got %v", Confidence(10, 0))
	}
	if Confidence(50, 48) < 0.8 {
		t.Fatalf("48/50 should be high confidence, got %v", Confidence(50, 48))
	}
}

// Property: confidence is a lower bound in [0, winrate], and rises (never falls)
// as wins increase for a fixed number of reads.
func TestProp_ConfidenceIsAMonotoneLowerBound(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		reads := rapid.IntRange(0, 200).Draw(rt, "reads")
		wins := rapid.IntRange(0, reads).Draw(rt, "wins")
		c := Confidence(reads, wins)
		if c < 0 || c > 1 {
			rt.Fatalf("confidence %v out of [0,1]", c)
		}
		if reads == 0 {
			if c != 0 {
				rt.Fatalf("zero reads must be zero confidence, got %v", c)
			}
			return
		}
		if rate := float64(wins) / float64(reads); c > rate+1e-9 {
			rt.Fatalf("lower bound %v exceeds win rate %v", c, rate)
		}
		if wins < reads && Confidence(reads, wins+1) < c-1e-9 {
			rt.Fatalf("confidence fell when wins rose: %v -> %v", c, Confidence(reads, wins+1))
		}
	})
}

func TestReinforce(t *testing.T) {
	skills, _ := newStores(t)
	ctx := context.Background()
	for _, slug := range []string{"a", "b"} {
		if _, err := skills.Upsert(ctx, state.Skill{Slug: slug, Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	// A converged run that read a (twice, deduped) and b.
	if err := Reinforce(ctx, skills, []string{"a", "a", "b", "ghost"}, true); err != nil {
		t.Fatal(err)
	}
	// A failed run that read a.
	if err := Reinforce(ctx, skills, []string{"a"}, false); err != nil {
		t.Fatal(err)
	}

	a, _ := skills.Get(ctx, "a")
	b, _ := skills.Get(ctx, "b")
	if a.Reads != 2 || a.Wins != 1 {
		t.Fatalf("a evidence = (%d,%d), want (2,1)", a.Reads, a.Wins)
	}
	if b.Reads != 1 || b.Wins != 1 {
		t.Fatalf("b evidence = (%d,%d), want (1,1)", b.Reads, b.Wins)
	}
	// Reinforcement is about reads alone. Neither run was recorded as an offer, so
	// neither skill has one: the two counters cannot be moved by the same call.
	if a.Offers != 0 || b.Offers != 0 {
		t.Fatalf("offers = (%d,%d), want (0,0): reinforcement must not touch them", a.Offers, b.Offers)
	}
}

// TestOfferRecordsTheImpressionAndNothingElse is the defect this split exists for.
// A skill that recall put in front of five runs and that no run ever loaded has to
// end up with five offers, no reads, no wins, and a confidence of zero: it has been
// shown, not tried, and ranking it as though it had been tried is what made the old
// counter a measure of keyword luck.
func TestOfferRecordsTheImpressionAndNothingElse(t *testing.T) {
	skills, _ := newStores(t)
	ctx := context.Background()
	if _, err := skills.Upsert(ctx, state.Skill{Slug: "shown", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := Offer(ctx, skills, []string{"shown", "shown", "ghost", ""}); err != nil {
			t.Fatal(err)
		}
	}
	sk, err := skills.Get(ctx, "shown")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Offers != 5 || sk.Reads != 0 || sk.Wins != 0 {
		t.Fatalf("evidence = (offers %d, reads %d, wins %d), want (5,0,0)", sk.Offers, sk.Reads, sk.Wins)
	}
	if c := Confidence(sk.Reads, sk.Wins); c != 0 {
		t.Fatalf("confidence = %v, want 0: an offered-and-unread skill has no record", c)
	}
}

// A skill offered often and never read is never retired. Decay is about skills that
// were tried and did not help; an unread one has a description problem, and archiving
// it would delete the body nobody has judged.
func TestDecayIgnoresOffers(t *testing.T) {
	skills, _ := newStores(t)
	ctx := context.Background()
	if _, err := skills.Upsert(ctx, state.Skill{Slug: "unread", Body: "x", Offers: 100}); err != nil {
		t.Fatal(err)
	}
	archived, err := Decay(ctx, skills, state.Scope{}, DefaultDecay())
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 0 {
		t.Fatalf("archived %+v, want none: offers are not evidence a skill failed", archived)
	}
}

func TestDecayRetiresProvenLosers(t *testing.T) {
	skills, _ := newStores(t)
	ctx := context.Background()
	seed := func(slug string, reads, wins int) {
		if _, err := skills.Upsert(ctx, state.Skill{Slug: slug, Body: "x", Reads: reads, Wins: wins}); err != nil {
			t.Fatal(err)
		}
	}
	seed("loser", 10, 0)  // enough evidence, never helped -> retire
	seed("winner", 10, 9) // strong record -> keep
	seed("newbie", 2, 0)  // too little evidence -> keep (unproven, not disproven)

	archived, err := Decay(ctx, skills, state.Scope{}, DefaultDecay())
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].Slug != "loser" {
		t.Fatalf("archived = %+v, want only [loser]", archived)
	}
	if _, err := skills.Get(ctx, "loser"); err == nil {
		t.Fatal("loser should have been archived (soft-deleted)")
	}
	if _, err := skills.Get(ctx, "winner"); err != nil {
		t.Fatalf("winner should be kept: %v", err)
	}
	if _, err := skills.Get(ctx, "newbie"); err != nil {
		t.Fatalf("unproven newbie should be kept: %v", err)
	}
}
