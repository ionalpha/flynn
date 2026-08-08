package consolidate_test

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/memory/consolidate"
	"github.com/ionalpha/flynn/state"
)

// episodeDraw is one drawn episode: the fields the pass groups, orders and
// inherits by, and nothing else.
type episodeDraw struct {
	Subject string
	Scope   state.Scope
	Source  string
	Tainted bool
}

var (
	propSubjects = []string{"flaky-deploy", "slow-tests"}
	propScopes   = []state.Scope{{}, {Project: "p"}, {Project: "p", Workspace: "w"}}
	propSources  = []string{"agent:run-1", "tool:ci", "user:operator"}
)

func drawEpisodes(rt *rapid.T) []episodeDraw {
	gen := rapid.Custom(func(t *rapid.T) episodeDraw {
		return episodeDraw{
			Subject: rapid.SampledFrom(propSubjects).Draw(t, "subject"),
			Scope:   rapid.SampledFrom(propScopes).Draw(t, "scope"),
			Source:  rapid.SampledFrom(propSources).Draw(t, "source"),
			Tainted: rapid.Bool().Draw(t, "tainted"),
		}
	})
	return rapid.SliceOfN(gen, 0, 14).Draw(rt, "episodes")
}

// propRun seeds a store with the drawn episodes, runs the pass, and returns the
// store with everything written and everything left live.
func propRun(rt *rapid.T, draws []episodeDraw, runs int) (written, live []state.MemoryItem) {
	ctx := context.Background()
	p := state.NewMemory()
	defer func() { _ = p.Close() }()
	store := p.Memory()

	for i, d := range draws {
		it, err := store.Write(ctx, state.MemoryItem{
			Kind: "episode", Subject: d.Subject, Scope: d.Scope,
			Content: fmt.Sprintf("episode %d", i), Sources: []string{d.Source}, Tainted: d.Tainted,
		})
		if err != nil {
			rt.Fatalf("write %d: %v", i, err)
		}
		written = append(written, it)
	}
	pass, err := consolidate.New(store, consolidate.DistillerFunc(
		func(_ context.Context, in consolidate.Series) (consolidate.Lesson, error) {
			return consolidate.Lesson{Content: fmt.Sprintf("lesson on %s: %d", in.Subject, len(in.Episodes))}, nil
		}))
	if err != nil {
		rt.Fatalf("new pass: %v", err)
	}
	for i := range runs {
		rep, err := pass.Run(ctx, state.RecallQuery{})
		if err != nil {
			rt.Fatalf("run %d: %v", i, err)
		}
		if len(rep.Failures) != 0 {
			rt.Fatalf("run %d failed: %+v", i, rep.Failures)
		}
	}
	all, err := store.Recall(ctx, state.RecallQuery{})
	if err != nil {
		rt.Fatalf("recall: %v", err)
	}
	return written, all
}

// Nothing is retired that a lesson does not account for. This is the property
// consolidation lives or dies on: the pass deletes memory, and the only thing
// making that safe rather than lossy is that whatever it deleted is named by
// something still readable.
func TestProp_NothingIsRetiredUnaccounted(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		written, live := propRun(rt, drawEpisodes(rt), 1)

		liveIDs := make(map[string]bool, len(live))
		superseded := make(map[string]bool, len(live))
		for _, it := range live {
			liveIDs[it.ID] = true
			for _, id := range it.Supersedes {
				superseded[id] = true
			}
		}
		for _, it := range written {
			if !liveIDs[it.ID] && !superseded[it.ID] {
				rt.Fatalf("episode %s on %s was retired and no live lesson names it", it.ID, it.Subject)
			}
		}
	})
}

// Running the pass again changes nothing. Stated over arbitrary corpora rather
// than one scenario, because the resume path is exactly the kind of logic that is
// only as correct as the interleavings somebody thought to write down.
func TestProp_RunsAreIdempotent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		draws := drawEpisodes(rt)
		_, once := propRun(rt, draws, 1)
		_, thrice := propRun(rt, draws, 3)

		if len(once) != len(thrice) {
			rt.Fatalf("one run left %d items, three left %d", len(once), len(thrice))
		}
		got, want := contentSet(thrice), contentSet(once)
		for c := range want {
			if !got[c] {
				rt.Fatalf("three runs lost %q, which one run kept", c)
			}
		}
		for c := range got {
			if !want[c] {
				rt.Fatalf("three runs produced %q, which one run did not", c)
			}
		}
	})
}

// A lesson carries every source of every episode it was drawn from, and is
// tainted if any of them was. Provenance is what makes a purge exact, and taint
// only ever spreads: a pass that dropped either would produce a lesson that looks
// cleaner than the material it came from.
func TestProp_LessonsInheritProvenanceAndTaint(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		written, live := propRun(rt, drawEpisodes(rt), 1)

		byID := make(map[string]state.MemoryItem, len(written))
		for _, it := range written {
			byID[it.ID] = it
		}
		for _, it := range live {
			if len(it.Supersedes) == 0 {
				continue
			}
			for _, id := range it.Supersedes {
				ep, ok := byID[id]
				if !ok {
					continue
				}
				if ep.Tainted && !it.Tainted {
					rt.Fatalf("lesson %s is clean but was drawn from tainted episode %s", it.ID, id)
				}
				for _, src := range ep.Sources {
					if !containsID(it.Sources, src) {
						rt.Fatalf("lesson %s lists %v and drops %q from episode %s", it.ID, it.Sources, src, id)
					}
				}
				if ep.Scope != it.Scope || ep.Subject != it.Subject {
					rt.Fatalf("lesson %s on %s/%+v supersedes an episode from %s/%+v",
						it.ID, it.Subject, it.Scope, ep.Subject, ep.Scope)
				}
			}
		}
	})
}

func contentSet(items []state.MemoryItem) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[it.Content] = true
	}
	return out
}
