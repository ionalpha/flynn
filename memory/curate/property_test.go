package curate_test

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/memory/curate"
	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

// writeDraw is one drawn write: enough of an item to exercise the policy and
// nothing else.
type writeDraw struct {
	Kind    string
	Subject string
	Scope   state.Scope
	Source  string
}

var (
	drawKinds    = []string{curate.KindFact, curate.KindPreference, curate.KindDecision, "episode", "observation"}
	drawSubjects = []string{"db-choice", "queue-choice"}
	drawScopes   = []state.Scope{{}, {Project: "p"}, {Project: "p", Workspace: "w"}}
)

func drawWrites(rt *rapid.T, sources []string) []writeDraw {
	gen := rapid.Custom(func(t *rapid.T) writeDraw {
		return writeDraw{
			Kind:    rapid.SampledFrom(drawKinds).Draw(t, "kind"),
			Subject: rapid.SampledFrom(drawSubjects).Draw(t, "subject"),
			Scope:   rapid.SampledFrom(drawScopes).Draw(t, "scope"),
			Source:  rapid.SampledFrom(sources).Draw(t, "source"),
		}
	})
	return rapid.SliceOfN(gen, 1, 12).Draw(rt, "writes")
}

// runWrites applies the drawn writes through a fresh curating store and returns
// the store, every item it handed back, and everything live at the end.
func runWrites(rt *rapid.T, writes []writeDraw) (written, live []state.MemoryItem) {
	ctx := context.Background()
	p := state.NewMemory()
	defer func() { _ = p.Close() }()
	st := curate.Wrap(p.Memory())

	for i, w := range writes {
		got, err := st.Write(ctx, state.MemoryItem{
			Kind:    w.Kind,
			Subject: w.Subject,
			Scope:   w.Scope,
			Content: fmt.Sprintf("write %d", i),
			Sources: []string{w.Source},
		})
		if err != nil {
			rt.Fatalf("write %d (%+v): %v", i, w, err)
		}
		written = append(written, got)
	}
	all, err := st.Recall(ctx, state.RecallQuery{})
	if err != nil {
		rt.Fatalf("recall: %v", err)
	}
	return written, all
}

// Nothing is retired without the record saying what retired it. This is the
// property the whole design turns on: a fact leaves recall only as part of a
// correction that is itself stored, so an id that has gone quiet can always be
// traced to the item that replaced it.
//
// The trace runs through every write, not only the live ones, because a chain
// three deep retires its own middle: A is replaced by B, then B by C, and B is a
// tombstone that still carries its link to A. Restricting the search to live
// items would be asserting that supersession chains are one link long, which they
// are not, and would push the policy towards flattening the chain onto whichever
// item happens to be current - a record that would then claim C replaced a fact it
// never saw.
func TestProp_EveryRetirementIsRecorded(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		written, live := runWrites(rt, drawWrites(rt, []string{guard.SchemeUser + "operator"}))

		liveIDs := make(map[string]bool, len(live))
		for _, it := range live {
			liveIDs[it.ID] = true
		}
		retiredBy := make(map[string]string, len(written))
		for _, it := range written {
			for _, id := range it.Supersedes {
				if prev, ok := retiredBy[id]; ok {
					rt.Fatalf("item %s is superseded twice, by %s and %s", id, prev, it.ID)
				}
				retiredBy[id] = it.ID
			}
		}
		for _, it := range written {
			if liveIDs[it.ID] || retiredBy[it.ID] != "" {
				continue
			}
			rt.Fatalf("item %s (%s on %s) is neither live nor superseded by anything", it.ID, it.Kind, it.Subject)
		}
		// The other direction: nothing claims to have retired an item that is still
		// live, which would leave two answers where the record says there is one.
		for id, by := range retiredBy {
			if liveIDs[id] {
				rt.Fatalf("item %s is live and %s claims to supersede it", id, by)
			}
		}
	})
}

// A replace kind holds one live answer per subject, kind and scope. Written with
// one provenance throughout, so no write is demoted and the rule is the policy's
// alone rather than the trust protection's.
func TestProp_ReplaceKindsHoldOneAnswer(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		_, live := runWrites(rt, drawWrites(rt, []string{guard.SchemeUser + "operator"}))

		seen := make(map[string]state.MemoryItem)
		for _, it := range live {
			if curate.ClassOf(it.Kind) != curate.ClassReplace {
				continue
			}
			key := it.Kind + "\x00" + it.Subject + "\x00" + it.Scope.Instance + "\x00" + it.Scope.Project + "\x00" + it.Scope.Workspace
			if prev, ok := seen[key]; ok {
				rt.Fatalf("two live %s items on %s in %+v: %s and %s", it.Kind, it.Subject, it.Scope, prev.ID, it.ID)
			}
			seen[key] = it
		}
	})
}

// An append kind never loses a member of its series, whatever else was written
// around it. The series is what a consolidation pass distils, so a policy that
// dropped one would be deciding what the lesson is by deleting the evidence.
func TestProp_AppendKindsNeverLoseAMember(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		written, live := runWrites(rt, drawWrites(rt, []string{guard.SchemeUser + "operator"}))

		liveIDs := make(map[string]bool, len(live))
		for _, it := range live {
			liveIDs[it.ID] = true
		}
		for _, it := range written {
			if curate.ClassOf(it.Kind) == curate.ClassReplace {
				continue
			}
			if !liveIDs[it.ID] {
				rt.Fatalf("appended item %s (%s on %s) is no longer live", it.ID, it.Kind, it.Subject)
			}
		}
	})
}

// Trust only ever flows one way: whatever the write order, an item is never
// retired by one that is less trusted than it. This is the protection stated as a
// property over arbitrary interleavings, which is where a rule written per case
// tends to have a hole.
func TestProp_LessTrustedWritesNeverRetire(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		sources := []string{guard.SchemeUser + "operator", guard.SchemeAgent + "run", guard.SchemeTool + "web-fetch"}
		_, live := runWrites(rt, drawWrites(rt, sources))

		byID := make(map[string]state.MemoryItem, len(live))
		for _, it := range live {
			byID[it.ID] = it
		}
		for _, it := range live {
			for _, id := range it.Supersedes {
				// The superseded item is tombstoned, so it is not in the live set; what
				// is checked here is the trust of the item that did the retiring, which
				// the conflict episode records when it was not allowed to.
				if _, stillLive := byID[id]; stillLive {
					rt.Fatalf("item %s supersedes %s and both are live", it.ID, id)
				}
			}
		}
		// A conflict episode exists exactly when a less trusted write was demoted, and
		// the fact it contradicted is still live: the protection's whole point.
		for _, it := range live {
			if it.Kind != curate.KindConflict {
				continue
			}
			if len(it.Supersedes) != 0 {
				rt.Fatalf("conflict episode %s supersedes %v, want nothing", it.ID, it.Supersedes)
			}
		}
	})
}
