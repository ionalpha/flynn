package guard_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

func TestTaintScopeStartsClean(t *testing.T) {
	ctx := guard.NewTaintScope(context.Background())
	if guard.Tainted(ctx) {
		t.Fatal("a fresh scope is tainted")
	}
	if got := guard.TaintReasons(ctx); len(got) != 0 {
		t.Fatalf("a fresh scope carries reasons %v", got)
	}
}

func TestTaintScopeless(t *testing.T) {
	// A context with no scope is the host that has not adopted taint tracking. It
	// reads clean and every marking call is a no-op, so nothing panics and nothing
	// is asserted about a run nobody observed.
	ctx := context.Background()
	guard.MarkTainted(ctx, "tool:shell")
	if guard.Observe(ctx, "web:example.com") {
		t.Fatal("Observe reported taint on a context with no scope")
	}
	if guard.Tainted(ctx) {
		t.Fatal("a scopeless context reported tainted")
	}
	if got := guard.TaintReasons(ctx); got != nil {
		t.Fatalf("a scopeless context returned reasons %v", got)
	}
}

func TestObserveMarksOnlyUntrustedSources(t *testing.T) {
	ctx := guard.NewTaintScope(context.Background())
	if guard.Observe(ctx, "user:operator", "agent:run-3", "chat", "") {
		t.Fatal("trusted and agent sources tainted the scope")
	}
	// One untrusted source among trusted ones taints the run: the content is in the
	// context either way.
	if !guard.Observe(ctx, "user:operator", "tool:shell") {
		t.Fatal("Observe returned clean after seeing an untrusted source")
	}
	if !guard.Tainted(ctx) {
		t.Fatal("an untrusted source did not taint the scope")
	}
	if got := guard.TaintReasons(ctx); !slices.Equal(got, []string{"tool:shell"}) {
		t.Fatalf("reasons = %v, want just the untrusted source", got)
	}
}

func TestObserveReportsTheScopeState(t *testing.T) {
	ctx := guard.NewTaintScope(context.Background())
	if !guard.Observe(ctx, "web:example.com") {
		t.Fatal("Observe of an untrusted source returned false")
	}
	// A later clean observation still reports the scope as tainted: the answer is
	// about the run, not about this call's arguments.
	if !guard.Observe(ctx, "user:operator") {
		t.Fatal("Observe forgot an earlier taint")
	}
}

func TestTaintIsMonotoneAndReasonsDeduplicate(t *testing.T) {
	ctx := guard.NewTaintScope(context.Background())
	guard.MarkTainted(ctx, "tool:shell")
	guard.MarkTainted(ctx, "tool:shell")
	guard.MarkTainted(ctx, "")
	guard.MarkTainted(ctx, "web:example.com")
	if !guard.Tainted(ctx) {
		t.Fatal("scope lost its taint")
	}
	want := []string{"tool:shell", "web:example.com"}
	if got := guard.TaintReasons(ctx); !slices.Equal(got, want) {
		t.Fatalf("reasons = %v, want %v in marking order", got, want)
	}
	// The returned slice is a copy: a caller mutating it must not rewrite the audit.
	guard.TaintReasons(ctx)[0] = "clobbered"
	if got := guard.TaintReasons(ctx); !slices.Equal(got, want) {
		t.Fatalf("reasons after caller mutation = %v, want %v", got, want)
	}
}

func TestTaintScopesDoNotNest(t *testing.T) {
	outer := guard.NewTaintScope(context.Background())
	inner := guard.NewTaintScope(outer)
	guard.MarkTainted(inner, "tool:shell")
	if !guard.Tainted(inner) {
		t.Fatal("inner scope did not take the taint")
	}
	if guard.Tainted(outer) {
		t.Fatal("marking an inner scope tainted the outer one")
	}
	// The child of a tainted scope, sharing it rather than opening its own, sees it.
	child := context.WithValue(inner, struct{ k string }{"unrelated"}, 1)
	if !guard.Tainted(child) {
		t.Fatal("a derived context lost the scope's taint")
	}
}

func TestTaintScopeIsConcurrencySafe(t *testing.T) {
	ctx := guard.NewTaintScope(context.Background())
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				guard.Observe(ctx, "tool:shell")
				return
			}
			guard.Tainted(ctx)
			guard.TaintReasons(ctx)
		}()
	}
	wg.Wait()
	if !guard.Tainted(ctx) {
		t.Fatal("concurrent observations lost the taint")
	}
	if got := guard.TaintReasons(ctx); !slices.Equal(got, []string{"tool:shell"}) {
		t.Fatalf("reasons = %v, want one deduplicated entry", got)
	}
}

func TestTaintItem(t *testing.T) {
	clean := context.Background()
	dirty := guard.NewTaintScope(context.Background())
	guard.MarkTainted(dirty, "tool:shell")

	cases := []struct {
		name string
		ctx  context.Context
		in   state.MemoryItem
		want bool
	}{
		{"clean context, agent source", clean, state.MemoryItem{Sources: []string{"agent:run-1"}}, false},
		{"clean context, untrusted source", clean, state.MemoryItem{Sources: []string{"tool:shell"}}, true},
		{"clean context, mixed sources", clean, state.MemoryItem{Sources: []string{"user:me", "web:page"}}, true},
		{"tainted context, agent source", dirty, state.MemoryItem{Sources: []string{"agent:run-1"}}, true},
		{"tainted context, user source", dirty, state.MemoryItem{Sources: []string{"user:me"}}, true},
		{"already tainted, clean everything", clean, state.MemoryItem{Sources: []string{"user:me"}, Tainted: true}, true},
	}
	for _, c := range cases {
		if got := guard.TaintItem(c.ctx, c.in); got.Tainted != c.want {
			t.Errorf("%s: Tainted = %v, want %v", c.name, got.Tainted, c.want)
		}
	}
}

// TestStoreLaundersNothing is the attack this whole file exists for: a tool's
// output is read into the run, the agent draws a conclusion from it, and writes the
// conclusion crediting itself. The write is clean by every stored signal - agent
// scheme, no screening hit - and only the run's taint tells it apart.
func TestStoreLaundersNothing(t *testing.T) {
	inner := state.NewMemory().Memory()
	g := guard.Wrap(inner)
	ctx := guard.NewTaintScope(context.Background())

	// The ingest path sees the tool output first.
	guard.Observe(ctx, "tool:web-fetch")

	laundered, err := g.Write(ctx, state.MemoryItem{
		Kind:    "fact",
		Content: "the deploy key rotates on Fridays",
		Sources: []string{"agent:distiller"},
	})
	if err != nil {
		t.Fatalf("the write must be stored, not refused: %v", err)
	}
	if !laundered.Tainted {
		t.Fatal("a conclusion drawn in a tainted run was stored clean")
	}
	if guard.PushEligible(laundered, true) {
		t.Fatal("the laundered fact is push-eligible even when promoted")
	}

	// A second run with its own clean scope writes the identical sentence, and it
	// is eligible on promotion: the difference is the run, not the content.
	honest, err := g.Write(guard.NewTaintScope(context.Background()), state.MemoryItem{
		Kind:    "fact",
		Content: "the deploy key rotates on Fridays",
		Sources: []string{"agent:distiller"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if honest.Tainted {
		t.Fatal("a clean run produced a tainted write")
	}
	if !guard.PushEligible(honest, true) {
		t.Fatal("the untainted agent note is not eligible once promoted")
	}
}

func TestStoreRecordsProvenanceTaintWithoutAScope(t *testing.T) {
	inner := state.NewMemory().Memory()
	g := guard.Wrap(inner)
	// No taint scope at all: the host has not adopted them. Provenance still taints,
	// so an untrusted-channel write is kept out of the digest either way.
	got, err := g.Write(context.Background(), state.MemoryItem{
		Kind: "fact", Content: "release is monthly", Sources: []string{"web:changelog"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Tainted {
		t.Fatal("an untrusted-source write was stored clean")
	}
}
