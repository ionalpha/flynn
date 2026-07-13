package state_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// errAppendDown is the sentinel the injected append failures carry.
var errAppendDown = errors.New("state test: append rejected")

// TestMutationFailsWhenItsEventCannotBeAppended is the event-sourcing invariant in
// its failure direction: every mutation records its event first, so an append that
// never lands must leave no trace in the read model either. If a store applied the
// mutation anyway, its projection would hold state its own log cannot reproduce,
// and a Replay would silently disagree with the live provider.
//
// Each case fails exactly one append (the one its mutation makes), so the setup
// writes land normally and only the operation under test is starved.
func TestMutationFailsWhenItsEventCannotBeAppended(t *testing.T) {
	for _, tc := range []struct {
		name string
		// failOn is the 1-based index of the append the mutation under test makes.
		failOn int
		setup  func(t *testing.T, p state.Provider) string
		mutate func(t *testing.T, p state.Provider, id string) error
		verify func(t *testing.T, p state.Provider, id string)
	}{
		{
			name:   "session create",
			failOn: 1,
			mutate: func(_ *testing.T, p state.Provider, _ string) error {
				_, err := p.Sessions().Create(context.Background(), state.Session{Title: "s"})
				return err
			},
			verify: func(t *testing.T, p state.Provider, _ string) {
				if got, _ := p.Sessions().List(context.Background()); len(got) != 0 {
					t.Fatalf("%d sessions exist after the create failed to record", len(got))
				}
			},
		},
		{
			name:   "turn append",
			failOn: 2,
			setup:  createSession,
			mutate: func(_ *testing.T, p state.Provider, id string) error {
				_, err := p.Sessions().AppendTurn(context.Background(), state.Turn{SessionID: id, Role: "user", Content: "hi"})
				return err
			},
			verify: func(t *testing.T, p state.Provider, id string) {
				if got, _ := p.Sessions().Turns(context.Background(), id); len(got) != 0 {
					t.Fatalf("the transcript holds %d turns after the append failed to record", len(got))
				}
			},
		},
		{
			name:   "session delete",
			failOn: 2,
			setup:  createSession,
			mutate: func(_ *testing.T, p state.Provider, id string) error {
				return p.Sessions().Delete(context.Background(), id)
			},
			verify: func(t *testing.T, p state.Provider, id string) {
				if _, err := p.Sessions().Get(context.Background(), id); err != nil {
					t.Fatalf("the session was tombstoned even though its delete never recorded: %v", err)
				}
			},
		},
		{
			name:   "skill upsert",
			failOn: 1,
			mutate: func(_ *testing.T, p state.Provider, _ string) error {
				_, err := p.Skills().Upsert(context.Background(), state.Skill{Slug: "deploy", Name: "Deploy"})
				return err
			},
			verify: func(t *testing.T, p state.Provider, _ string) {
				if got, _ := p.Skills().Search(context.Background(), "", 0); len(got) != 0 {
					t.Fatalf("%d skills exist after the upsert failed to record", len(got))
				}
			},
		},
		{
			name:   "skill delete",
			failOn: 2,
			setup:  upsertSkill,
			mutate: func(_ *testing.T, p state.Provider, id string) error {
				return p.Skills().Delete(context.Background(), id)
			},
			verify: func(t *testing.T, p state.Provider, id string) {
				if _, err := p.Skills().Get(context.Background(), id); err != nil {
					t.Fatalf("the skill was tombstoned even though its delete never recorded: %v", err)
				}
			},
		},
		{
			name:   "memory write",
			failOn: 1,
			mutate: func(_ *testing.T, p state.Provider, _ string) error {
				_, err := p.Memory().Write(context.Background(), state.MemoryItem{Kind: "fact", Content: "x"})
				return err
			},
			verify: func(t *testing.T, p state.Provider, _ string) {
				if got, _ := p.Memory().Recall(context.Background(), state.RecallQuery{}); len(got) != 0 {
					t.Fatalf("%d memory items exist after the write failed to record", len(got))
				}
			},
		},
		{
			name:   "memory delete",
			failOn: 2,
			setup:  writeMemory,
			mutate: func(_ *testing.T, p state.Provider, id string) error {
				return p.Memory().Delete(context.Background(), id)
			},
			verify: func(t *testing.T, p state.Provider, _ string) {
				if got, _ := p.Memory().Recall(context.Background(), state.RecallQuery{}); len(got) != 1 {
					t.Fatalf("recall returned %d items; the item was tombstoned even though its delete never recorded", len(got))
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			inner := spine.NewMemoryLog()
			log := testkit.FaultyLog(inner, testkit.FailOnCall(tc.failOn, errAppendDown))
			p := state.NewMemory(state.WithEventLog(log))

			var id string
			if tc.setup != nil {
				id = tc.setup(t, p)
			}
			if err := tc.mutate(t, p, id); !errors.Is(err, errAppendDown) {
				t.Fatalf("mutation error = %v, want the append failure", err)
			}
			tc.verify(t, p, id)

			// The stream carries the setup's events and nothing else: a rejected
			// append records no event and consumes no Seq, so the log stays gap-free.
			events, err := inner.Read(ctx, spine.Query{Stream: state.StateStream})
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.failOn - 1; len(events) != want {
				t.Fatalf("the stream holds %d events, want %d (the failed mutation recorded one)", len(events), want)
			}
			for i, e := range events {
				if e.Seq != int64(i+1) {
					t.Fatalf("event %d has Seq %d: the rejected append left a gap", i, e.Seq)
				}
			}

			// The read model is still exactly a fold of the log.
			rebuilt, err := state.Replay(ctx, inner)
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			assertSameState(ctx, t, p, rebuilt)
		})
	}
}

func createSession(t *testing.T, p state.Provider) string {
	t.Helper()
	s, err := p.Sessions().Create(context.Background(), state.Session{Title: "s"})
	if err != nil {
		t.Fatalf("setup create session: %v", err)
	}
	return s.ID
}

func upsertSkill(t *testing.T, p state.Provider) string {
	t.Helper()
	sk, err := p.Skills().Upsert(context.Background(), state.Skill{Slug: "deploy", Name: "Deploy"})
	if err != nil {
		t.Fatalf("setup upsert skill: %v", err)
	}
	return sk.ID
}

func writeMemory(t *testing.T, p state.Provider) string {
	t.Helper()
	it, err := p.Memory().Write(context.Background(), state.MemoryItem{Kind: "fact", Content: "x"})
	if err != nil {
		t.Fatalf("setup write memory: %v", err)
	}
	return it.ID
}
