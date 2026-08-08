package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/memory/curate"
	"github.com/ionalpha/flynn/memory/ridealong"
	"github.com/ionalpha/flynn/state"
)

// TestMemoryStackWriteSemantics is the write half of the wiring: a fact written
// through the stack supersedes the standing answer on its subject rather than
// stacking a second one beside it, and an episode joins the series.
//
// Before this was wired the binary wrote straight through the facade, so a user
// correcting a fact left both versions in the store with nothing saying which one
// was current, and the wake digest had no way to choose between them.
func TestMemoryStackWriteSemantics(t *testing.T) {
	ctx := context.Background()
	mem := newMemoryStack(state.NewMemory().Memory(), nil)

	for _, content := range []string{"the deploy target is staging", "the deploy target is production"} {
		if _, err := mem.store.Write(ctx, state.MemoryItem{
			Kind: curate.KindFact, Subject: "deploy-target", Content: content, Sources: []string{rememberSource},
		}); err != nil {
			t.Fatalf("write %q: %v", content, err)
		}
	}
	facts := recallSubject(t, ctx, mem.store, "deploy-target", curate.KindFact)
	if len(facts) != 1 {
		t.Fatalf("facts on the subject = %d, want 1: a fact replaces the standing answer", len(facts))
	}
	if facts[0].Content != "the deploy target is production" {
		t.Fatalf("standing fact = %q, want the correction", facts[0].Content)
	}

	for i, content := range []string{"the deploy failed", "it failed again"} {
		if _, err := mem.store.Write(ctx, state.MemoryItem{
			Kind: "episode", Subject: "deploy-target", Content: content, Sources: []string{"agent:run-1"},
		}); err != nil {
			t.Fatalf("write episode %d: %v", i, err)
		}
	}
	if eps := recallSubject(t, ctx, mem.store, "deploy-target", "episode"); len(eps) != 2 {
		t.Fatalf("episodes on the subject = %d, want both: an episode joins the series", len(eps))
	}
}

// TestMemoryStackWakeBlockPushes is the push half: the digest carries what the
// store holds, and building it records the push, so a memory that reaches a
// reader unasked is counted rather than looking like the reader found it.
func TestMemoryStackWakeBlockPushes(t *testing.T) {
	ctx := ridealong.NewPrimeScope(context.Background())
	mem := newMemoryStack(state.NewMemory().Memory(), nil)

	written, err := mem.store.Write(ctx, state.MemoryItem{
		Kind: curate.KindPreference, Subject: "review-style", Content: "state the risk before the fix", Sources: []string{rememberSource},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	block, err := mem.wakeBlock(ctx, state.Scope{})
	if err != nil {
		t.Fatalf("wakeBlock: %v", err)
	}
	if !strings.Contains(block, "state the risk before the fix") {
		t.Fatalf("digest block does not carry the memory:\n%s", block)
	}
	// The block introduces itself. A bare list of sentences dropped into the
	// standing instructions reads as something the user asked for this turn.
	if !strings.Contains(block, "not asked for") {
		t.Errorf("digest block does not frame itself as background:\n%s", block)
	}
	if !ridealong.Primed(ctx, written.ID) {
		t.Error("the pushed memory is not marked primed, so a later use would be credited to the reader")
	}
	usage, err := mem.store.Usage(ctx, []string{written.ID})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(usage) != 1 || usage[0].PushCount != 1 {
		t.Fatalf("usage = %+v, want one push recorded: the decay policy has nothing to read otherwise", usage)
	}
}

// TestMemoryStackWakeBlockEmptyStore proves an install with nothing learned yet
// contributes no block at all, rather than a header introducing an empty list.
func TestMemoryStackWakeBlockEmptyStore(t *testing.T) {
	ctx := ridealong.NewPrimeScope(context.Background())
	mem := newMemoryStack(state.NewMemory().Memory(), nil)
	block, err := mem.wakeBlock(ctx, state.Scope{})
	if err != nil {
		t.Fatalf("wakeBlock: %v", err)
	}
	if block != "" {
		t.Fatalf("block = %q, want empty on a store with nothing in it", block)
	}
}

// failingMemory is a memory store whose reads fail.
type failingMemory struct {
	state.MemoryStore
	err error
}

func (f failingMemory) Recall(context.Context, state.RecallQuery) ([]state.MemoryItem, error) {
	return nil, f.err
}

// TestMemoryStackWakeBlockSurvivesAFailedRead pins that memory is an advantage
// and not a precondition: a store that cannot be read costs the run its head
// start, not its life.
func TestMemoryStackWakeBlockSurvivesAFailedRead(t *testing.T) {
	want := errors.New("the store is gone")
	mem := newMemoryStack(failingMemory{MemoryStore: state.NewMemory().Memory(), err: want}, nil)
	block, err := mem.wakeBlock(ridealong.NewPrimeScope(context.Background()), state.Scope{})
	if block != "" {
		t.Errorf("block = %q, want empty", block)
	}
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want the read failure reported rather than swallowed", err)
	}
}

// TestNoticeWriterReportsWhatTheWritePathNoticed proves a conflict the write path
// recorded is also said out loud. The store keeps it either way; printing it is
// what stops the record being the only place it exists.
func TestNoticeWriterReportsWhatTheWritePathNoticed(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	mem := newMemoryStack(state.NewMemory().Memory(), noticeWriter(&out))

	// A user-pinned fact, then an agent-sourced contradiction of it: the agent is
	// not trusted to overwrite what the user said, so the write path records the
	// conflict instead of acting on it.
	if _, err := mem.store.Write(ctx, state.MemoryItem{
		Kind: curate.KindFact, Subject: "deploy-target", Content: "the deploy target is staging", Sources: []string{rememberSource},
	}); err != nil {
		t.Fatalf("write the pinned fact: %v", err)
	}
	if _, err := mem.store.Write(ctx, state.MemoryItem{
		Kind: curate.KindFact, Subject: "deploy-target", Content: "the deploy target is production", Sources: []string{"tool:ci"},
	}); err != nil {
		t.Fatalf("write the contradiction: %v", err)
	}
	if !strings.Contains(out.String(), "memory:") {
		t.Fatalf("nothing reported for a recorded conflict:\n%s", out.String())
	}
}

// TestNoticeWriterWithNoWriter returns nil rather than a sink that writes nowhere,
// so the store is built with no notifier at all.
func TestNoticeWriterWithNoWriter(t *testing.T) {
	if noticeWriter(nil) != nil {
		t.Fatal("noticeWriter(nil) = a function, want nil")
	}
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
	if mem == nil || mem.store == nil || mem.wake == nil {
		t.Fatalf("session memory = %+v, want a curated store and a digest builder", mem)
	}
	if again := s.memory(); again != mem {
		t.Fatal("session memory rebuilt on the second call; the push counts would be split across stacks")
	}

	for _, content := range []string{"the deploy target is staging", "the deploy target is production"} {
		if _, err := mem.store.Write(ctx, state.MemoryItem{
			Kind: curate.KindFact, Subject: "deploy-target", Content: content, Sources: []string{rememberSource},
		}); err != nil {
			t.Fatalf("write %q: %v", content, err)
		}
	}
	if facts := recallSubject(t, ctx, mem.store, "deploy-target", curate.KindFact); len(facts) != 1 {
		t.Fatalf("facts = %d, want 1: the session writes through the curated store", len(facts))
	}
}

// TestWithWakeFoldsTheDigestIn covers the three answers the wake step has: a
// digest to add, nothing to add, and a store that could not be read.
func TestWithWakeFoldsTheDigestIn(t *testing.T) {
	ctx := ridealong.NewPrimeScope(context.Background())

	t.Run("a digest is appended to the prompt", func(t *testing.T) {
		mem := newMemoryStack(state.NewMemory().Memory(), nil)
		if _, err := mem.store.Write(ctx, state.MemoryItem{
			Kind: curate.KindPreference, Subject: "review-style", Content: "state the risk before the fix", Sources: []string{rememberSource},
		}); err != nil {
			t.Fatalf("write: %v", err)
		}
		var out bytes.Buffer
		got := withWake(ctx, mem, "standing instructions", &out)
		if !strings.HasPrefix(got, "standing instructions\n\n") {
			t.Fatalf("prompt = %q, want the digest appended to what was there", got)
		}
		if !strings.Contains(got, "state the risk before the fix") {
			t.Fatalf("prompt does not carry the memory:\n%s", got)
		}
		if out.Len() != 0 {
			t.Errorf("wrote %q on a successful build, want nothing", out.String())
		}
	})

	t.Run("an empty store changes nothing", func(t *testing.T) {
		var out bytes.Buffer
		mem := newMemoryStack(state.NewMemory().Memory(), nil)
		if got := withWake(ctx, mem, "standing instructions", &out); got != "standing instructions" {
			t.Fatalf("prompt = %q, want it untouched", got)
		}
		if out.Len() != 0 {
			t.Errorf("wrote %q with nothing to report, want nothing", out.String())
		}
	})

	t.Run("a failed read costs the head start, not the run", func(t *testing.T) {
		var out bytes.Buffer
		mem := newMemoryStack(failingMemory{MemoryStore: state.NewMemory().Memory(), err: errors.New("the store is gone")}, nil)
		if got := withWake(ctx, mem, "standing instructions", &out); got != "standing instructions" {
			t.Fatalf("prompt = %q, want the run to carry on without a digest", got)
		}
		if !strings.Contains(out.String(), "no memory digest") {
			t.Fatalf("nothing reported for a failed read:\n%s", out.String())
		}
		// A caller with nowhere to put it says so by passing no writer, and must not
		// then be the reason the run stops.
		if got := withWake(ctx, mem, "standing instructions", nil); got != "standing instructions" {
			t.Fatalf("prompt = %q with no writer, want it untouched", got)
		}
	})
}
