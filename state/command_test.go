package state_test

import (
	"context"
	mrand "math/rand"
	"reflect"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// TestReplayReconstructsState is the keystone property in the small: a sequence
// of mutations is recorded on the log, and a provider rebuilt purely by folding
// that log is identical to the live one. It also pins that each mutation emits
// exactly one event of the expected type — proof no write bypasses the log.
func TestReplayReconstructsState(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewManual(time.Unix(1_700_000_000, 0))
	log := spine.NewMemoryLog(spine.WithClock(clk))
	p := state.NewMemory(state.WithEventLog(log), state.WithClock(clk), state.WithInstanceID("a"))

	ses, err := p.Sessions().Create(ctx, state.Session{Title: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	clk.Advance(time.Millisecond)
	if _, err := p.Sessions().AppendTurn(ctx, state.Turn{SessionID: ses.ID, Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("append turn: %v", err)
	}
	sk, err := p.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "steps"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := p.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy v2", Body: "more"}); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	mi, err := p.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "the sky is blue"})
	if err != nil {
		t.Fatalf("write memory: %v", err)
	}
	if err := p.Memory().Delete(ctx, mi.ID); err != nil {
		t.Fatalf("delete memory: %v", err)
	}
	if err := p.Skills().Delete(ctx, sk.ID); err != nil {
		t.Fatalf("delete skill: %v", err)
	}

	// Six mutations → six events, in order, each of the expected type.
	events, err := log.Read(ctx, spine.Query{Stream: state.StateStream})
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	wantTypes := []string{
		"session.created", "session.turn_appended", "skill.upserted",
		"skill.upserted", "memory.written", "memory.deleted", "skill.deleted",
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d", len(events), len(wantTypes))
	}
	for i, w := range wantTypes {
		if events[i].Type != w {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, w)
		}
		if events[i].Seq != int64(i+1) {
			t.Fatalf("event %d Seq = %d, want %d", i, events[i].Seq, i+1)
		}
	}

	rebuilt, err := state.Replay(ctx, log)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	assertSameState(ctx, t, p, rebuilt)
}

// TestReplayPropertyEquivalence drives random valid mutation sequences and
// asserts (1) the count of successful mutations equals the number of events, and
// (2) a provider rebuilt from the log reads identically to the live one.
func TestReplayPropertyEquivalence(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		clk := clock.NewManual(time.Unix(1_700_000_000, 0))
		log := spine.NewMemoryLog(spine.WithClock(clk))
		p := state.NewMemory(state.WithEventLog(log), state.WithClock(clk), state.WithInstanceID("a"))

		var sessionIDs, memIDs []string
		liveSlugs := map[string]bool{}
		mutations := 0

		ops := rapid.SliceOfN(rapid.SampledFrom([]string{
			"createSession", "appendTurn", "deleteSession",
			"upsertSkill", "deleteSkill", "writeMemory", "deleteMemory",
		}), 1, 60).Draw(rt, "ops")

		for i, op := range ops {
			clk.Advance(time.Duration(rapid.IntRange(0, 3).Draw(rt, "adv")) * time.Millisecond)
			switch op {
			case "createSession":
				s, err := p.Sessions().Create(ctx, state.Session{Title: rapid.String().Draw(rt, "title")})
				if err != nil {
					rt.Fatalf("create: %v", err)
				}
				sessionIDs = append(sessionIDs, s.ID)
				mutations++
			case "appendTurn":
				if len(sessionIDs) == 0 {
					continue
				}
				id := rapid.SampledFrom(sessionIDs).Draw(rt, "sid")
				if _, err := p.Sessions().AppendTurn(ctx, state.Turn{SessionID: id, Role: "user", Content: rapid.String().Draw(rt, "c")}); err != nil {
					rt.Fatalf("appendTurn: %v", err)
				}
				mutations++
			case "deleteSession":
				if len(sessionIDs) == 0 {
					continue
				}
				idx := rapid.IntRange(0, len(sessionIDs)-1).Draw(rt, "didx")
				if err := p.Sessions().Delete(ctx, sessionIDs[idx]); err != nil {
					rt.Fatalf("deleteSession: %v", err)
				}
				sessionIDs = append(sessionIDs[:idx], sessionIDs[idx+1:]...)
				mutations++
			case "upsertSkill":
				slug := rapid.SampledFrom([]string{"a", "b", "c", "d"}).Draw(rt, "slug")
				if _, err := p.Skills().Upsert(ctx, state.Skill{Slug: slug, Name: slug, Body: rapid.String().Draw(rt, "body")}); err != nil {
					rt.Fatalf("upsert: %v", err)
				}
				liveSlugs[slug] = true
				mutations++
			case "deleteSkill":
				live := keysOf(liveSlugs)
				if len(live) == 0 {
					continue
				}
				slug := rapid.SampledFrom(live).Draw(rt, "dslug")
				if err := p.Skills().Delete(ctx, slug); err != nil {
					rt.Fatalf("deleteSkill: %v", err)
				}
				delete(liveSlugs, slug)
				mutations++
			case "writeMemory":
				m, err := p.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: rapid.String().Draw(rt, "mc")})
				if err != nil {
					rt.Fatalf("writeMemory: %v", err)
				}
				memIDs = append(memIDs, m.ID)
				mutations++
			case "deleteMemory":
				if len(memIDs) == 0 {
					continue
				}
				idx := rapid.IntRange(0, len(memIDs)-1).Draw(rt, "midx")
				if err := p.Memory().Delete(ctx, memIDs[idx]); err != nil {
					rt.Fatalf("deleteMemory: %v", err)
				}
				memIDs = append(memIDs[:idx], memIDs[idx+1:]...)
				mutations++
			}
			_ = i
		}

		events, err := log.Read(ctx, spine.Query{Stream: state.StateStream})
		if err != nil {
			rt.Fatalf("read log: %v", err)
		}
		if len(events) != mutations {
			rt.Fatalf("got %d events, want %d mutations (no write may bypass the log)", len(events), mutations)
		}

		rebuilt, err := state.Replay(ctx, log)
		if err != nil {
			rt.Fatalf("replay: %v", err)
		}
		assertSameState(ctx, rt, p, rebuilt)
	})
}

// TestRunIsDeterministic proves the determinism the command path exists for:
// two independent runs given the same seeded clock and entropy execute the same
// mutation sequence and produce byte-identical event logs — same IDs, same
// timestamps, same payloads. This is what makes a recorded run reproducible
// (deterministic replay), not merely rebuildable from its own log.
func TestRunIsDeterministic(t *testing.T) {
	ctx := context.Background()
	run := func() []spine.Event {
		clk := clock.NewManual(time.Unix(1_700_000_000, 0))
		// Deterministic entropy is the whole point here: a seeded source makes the
		// generated IDs reproducible across runs, which is what we are asserting.
		gen := ids.NewGenerator(ids.WithClock(clk), ids.WithEntropy(mrand.New(mrand.NewSource(42)))) //nolint:gosec // seeded RNG is intentional for reproducibility
		log := spine.NewMemoryLog(spine.WithClock(clk))
		p := state.NewMemory(state.WithEventLog(log), state.WithClock(clk), state.WithIDGenerator(gen))

		s, err := p.Sessions().Create(ctx, state.Session{Title: "t"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		clk.Advance(time.Millisecond)
		if _, err := p.Sessions().AppendTurn(ctx, state.Turn{SessionID: s.ID, Role: "user", Content: "hi"}); err != nil {
			t.Fatalf("appendTurn: %v", err)
		}
		clk.Advance(time.Millisecond)
		if _, err := p.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "steps"}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if _, err := p.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "x"}); err != nil {
			t.Fatalf("write: %v", err)
		}
		events, err := log.Read(ctx, spine.Query{Stream: state.StateStream})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return events
	}

	a, b := run(), run()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("runs diverged (non-deterministic):\n a=%+v\n b=%+v", a, b)
	}
	// Guard against the test trivially passing on empty logs.
	if len(a) != 4 {
		t.Fatalf("got %d events, want 4", len(a))
	}
}

// fataler is satisfied by both *testing.T and *rapid.T.
type fataler interface{ Fatalf(string, ...any) }

// assertSameState asserts two providers expose identical state through their
// read APIs — the operational meaning of "state is a projection of the log".
func assertSameState(ctx context.Context, t fataler, a, b state.Provider) {
	sa, _ := a.Sessions().List(ctx)
	sb, _ := b.Sessions().List(ctx)
	if !reflect.DeepEqual(sa, sb) {
		t.Fatalf("sessions differ:\n live=%+v\n rebuilt=%+v", sa, sb)
	}
	for _, s := range sa {
		ta, _ := a.Sessions().Turns(ctx, s.ID)
		tb, _ := b.Sessions().Turns(ctx, s.ID)
		if !reflect.DeepEqual(ta, tb) {
			t.Fatalf("turns for %s differ:\n live=%+v\n rebuilt=%+v", s.ID, ta, tb)
		}
	}
	ka, _ := a.Skills().Search(ctx, "", 0)
	kb, _ := b.Skills().Search(ctx, "", 0)
	if !reflect.DeepEqual(ka, kb) {
		t.Fatalf("skills differ:\n live=%+v\n rebuilt=%+v", ka, kb)
	}
	ma, _ := a.Memory().Recall(ctx, state.RecallQuery{})
	mb, _ := b.Memory().Recall(ctx, state.RecallQuery{})
	if !reflect.DeepEqual(ma, mb) {
		t.Fatalf("memory differs:\n live=%+v\n rebuilt=%+v", ma, mb)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
