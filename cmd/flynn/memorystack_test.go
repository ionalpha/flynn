package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/ionalpha/flynn/memory/curate"
	"github.com/ionalpha/flynn/state"
)

// failingMemory is a memory store whose reads fail.
type failingMemory struct {
	state.MemoryStore
	err error
}

func (f failingMemory) Recall(context.Context, state.RecallQuery) ([]state.MemoryItem, error) {
	return nil, f.err
}

// recallSubject reads one subject's items of one kind.
func recallSubject(t *testing.T, ctx context.Context, st state.MemoryStore, subject, kind string) []state.MemoryItem {
	t.Helper()
	items, err := st.Recall(ctx, state.RecallQuery{Subjects: []string{subject}, Kinds: []string{kind}})
	if err != nil {
		t.Fatalf("recall %s/%s: %v", subject, kind, err)
	}
	return items
}

// TestSessionMemoryIsCuratedAndStable proves a session assembled by hand gets the
// same memory the front door builds, and gets one rather than a fresh stack per
// call: the digest's push counts and the prime scope both belong to the session.
func TestSessionMemoryIsCuratedAndStable(t *testing.T) {
	ctx := context.Background()
	s := &replSession{out: &bytes.Buffer{}, store: memStore(t)}

	mem := s.memory()
	if mem == nil || mem.Store() == nil {
		t.Fatalf("session memory = %+v, want a curated store", mem)
	}
	if again := s.memory(); again != mem {
		t.Fatal("session memory rebuilt on the second call; the push counts would be split across stacks")
	}

	for _, content := range []string{"the deploy target is staging", "the deploy target is production"} {
		if _, err := mem.Store().Write(ctx, state.MemoryItem{
			Kind: curate.KindFact, Subject: "deploy-target", Content: content, Sources: []string{rememberSource},
		}); err != nil {
			t.Fatalf("write %q: %v", content, err)
		}
	}
	if facts := recallSubject(t, ctx, mem.Store(), "deploy-target", curate.KindFact); len(facts) != 1 {
		t.Fatalf("facts = %d, want 1: the session writes through the curated store", len(facts))
	}
}
