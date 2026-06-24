package learn

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/state"
)

// Property: from a converged run, the curator persists exactly the non-empty
// lessons the distiller produced, every skill recoverable by its slug and every
// memory item recallable, while a non-converged run with the same lessons persists
// nothing. This is the capture contract: outcome-gated, lossless, and provenance
// stamped.
func TestProp_CuratorCapturesGatedAndComplete(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		text := rapid.StringMatching(`[A-Za-z ]{1,12}`)
		lessonGen := rapid.Custom(func(t *rapid.T) Lesson {
			kind := LessonMemory
			if rapid.Bool().Draw(t, "isSkill") {
				kind = LessonSkill
			}
			return Lesson{
				Kind:  kind,
				Title: text.Draw(t, "title"),
				Body:  rapid.StringMatching(`[A-Za-z ]{0,12}`).Draw(t, "body"),
				Tags:  rapid.SliceOfN(text, 0, 2).Draw(t, "tags"),
			}
		})
		lessons := rapid.SliceOfN(lessonGen, 0, 6).Draw(rt, "lessons")
		converged := rapid.Bool().Draw(rt, "converged")

		skills, memories := newStores(t)
		o := convergedOutcome()
		o.Converged = converged
		c := NewCurator(&fakeDistiller{lessons: lessons}, skills, memories)

		captured, err := c.Curate(context.Background(), o)
		if err != nil {
			rt.Fatalf("curate: %v", err)
		}

		// Count the lessons that should have been kept: non-empty bodies, only when
		// the run converged.
		var wantSkills, wantMem int
		if converged {
			for _, l := range lessons {
				if trimEmpty(l.Body) {
					continue
				}
				if l.Kind == LessonSkill {
					wantSkills++
				} else {
					wantMem++
				}
			}
		}

		// Skills dedupe by slug, so the stored count may be lower than the captured
		// count; the captured slice reflects every write the curator made.
		if len(captured.Skills) != wantSkills {
			rt.Fatalf("captured %d skills, want %d", len(captured.Skills), wantSkills)
		}
		if len(captured.Memories) != wantMem {
			rt.Fatalf("captured %d memories, want %d", len(captured.Memories), wantMem)
		}
		for _, sk := range captured.Skills {
			got, err := skills.Get(context.Background(), sk.Slug)
			if err != nil {
				rt.Fatalf("captured skill %q not retrievable: %v", sk.Slug, err)
			}
			if !hasTag(got.Tags, provenanceTag) {
				rt.Fatalf("captured skill %q missing provenance tag", sk.Slug)
			}
		}
		items, err := memories.Recall(context.Background(), state.RecallQuery{Scope: o.Scope})
		if err != nil {
			rt.Fatalf("recall: %v", err)
		}
		if len(items) != wantMem {
			rt.Fatalf("recalled %d memories, want %d", len(items), wantMem)
		}
		for _, it := range items {
			if it.Source != o.Source {
				rt.Fatalf("memory source = %q, want %q", it.Source, o.Source)
			}
		}
	})
}

func trimEmpty(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' {
			return false
		}
	}
	return true
}
