package goal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// testNow is the fixed instant the pure ledger tests stamp proofs with. The record
// tests do not run a clock, and a real one would make the assertions about when an
// item was proven meaningless.
var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// --- fakes ------------------------------------------------------------------

// fakePlanner returns a fixed set of items, or an error.
type fakePlanner struct {
	items []LedgerItem
	err   error
	calls int
}

func (p *fakePlanner) Plan(context.Context, resource.Resource) ([]LedgerItem, error) {
	p.calls++
	return p.items, p.err
}

// neverStop never reports the stop condition met, so a test drives the phases
// itself rather than racing a convergence.
type neverStop struct{}

func (neverStop) Met(context.Context, Spec, Status) (bool, string, error) { return false, "", nil }

// planHarness wires a reconciler gated on planning to a worker that can plan, which
// is the pairing WithPlanning documents.
type planHarness struct {
	*harness
	w *Worker
	p *fakePlanner
}

func newPlanHarness(t *testing.T, stop StopEvaluator, planner *fakePlanner) *planHarness {
	t.Helper()
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg, resource.WithClock(m))
	q := jobs.NewMemory(jobs.WithClock(m))
	gr := NewReconciler(store, q, m, stop, WithPlanning())
	w := NewWorker(store, q, m, &fakeExec{}, WithPlanner(planner), WithLease(time.Minute))
	h := &harness{ctx: context.Background(), store: store, jobs: q, gr: gr, clk: m}
	return &planHarness{harness: h, w: w, p: planner}
}

func (h *planHarness) spec(t *testing.T, ref reconcile.Ref) Spec {
	t.Helper()
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	sp, err := DecodeSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func item(text, verify string) LedgerItem {
	return LedgerItem{ID: ItemID(text, verify), Item: text, Verify: verify}
}

// --- the record itself ------------------------------------------------------

// TestAppendItemsStampsContentAddresses pins that an item's identity is its
// content, which is what makes a rewrite detectable as a rewrite: the planner does
// not get to choose an id, so it cannot give changed text the id of the text it
// replaced.
func TestAppendItemsStampsContentAddresses(t *testing.T) {
	got, err := AppendItems(nil, LedgerItem{Item: "add the parser", Verify: "go test ./parser"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("appended %d items, want 1", len(got))
	}
	if want := ItemID("add the parser", "go test ./parser"); got[0].ID != want {
		t.Fatalf("id = %q, want the content address %q", got[0].ID, want)
	}
	// A different verify clause is a different item, not the same item restated.
	other, err := AppendItems(got, LedgerItem{Item: "add the parser", Verify: "eyeball it"})
	if err != nil {
		t.Fatal(err)
	}
	if other[1].ID == other[0].ID {
		t.Fatal("items differing only in their verify clause share an id")
	}
}

// TestAppendItemsRejectsAnItemWithNoWayToCheckIt is the rule that an item must
// arrive with its own verification. An item with no check can only ever be
// asserted, and an asserted item is exactly what the ledger exists to replace.
func TestAppendItemsRejectsAnItemWithNoWayToCheckIt(t *testing.T) {
	for _, tc := range []struct{ name, text, verify string }{
		{"no verify", "add the parser", ""},
		{"blank verify", "add the parser", "   "},
		{"no item", "", "go test ./parser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := AppendItems(nil, LedgerItem{Item: tc.text, Verify: tc.verify}); !errors.Is(err, ErrLedgerIncomplete) {
				t.Fatalf("err = %v, want ErrLedgerIncomplete", err)
			}
		})
	}
}

func TestAppendItemsRejectsADuplicate(t *testing.T) {
	base, err := AppendItems(nil, LedgerItem{Item: "a", Verify: "check a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppendItems(base, LedgerItem{Item: "a", Verify: "check a"}); !errors.Is(err, ErrLedgerDuplicate) {
		t.Fatalf("err = %v, want ErrLedgerDuplicate", err)
	}
}

// TestPlanExtension is the planner write path's idempotency rule: an exact
// re-statement of an item already on the ledger is dropped (so a re-run plan is a
// no-op), a reworded near-duplicate addresses to a new id and is genuinely appended,
// and the same new item twice in one batch is still refused as a duplicate.
func TestPlanExtension(t *testing.T) {
	base, err := AppendItems(nil, LedgerItem{Item: "a", Verify: "check a"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("an exact re-statement is dropped", func(t *testing.T) {
		got, err := PlanExtension(base, LedgerItem{Item: "a", Verify: "check a"})
		if err != nil {
			t.Fatalf("PlanExtension: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1 (the re-statement was dropped)", len(got))
		}
	})

	t.Run("a reworded near-duplicate is a new item", func(t *testing.T) {
		got, err := PlanExtension(base, LedgerItem{Item: "a", Verify: "check a a different way"})
		if err != nil {
			t.Fatalf("PlanExtension: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2 (a new content address is a new item)", len(got))
		}
	})

	t.Run("a re-statement mixed with a fresh item appends only the fresh one", func(t *testing.T) {
		got, err := PlanExtension(base,
			LedgerItem{Item: "a", Verify: "check a"}, // already present: dropped
			LedgerItem{Item: "b", Verify: "check b"}, // new: appended
		)
		if err != nil {
			t.Fatalf("PlanExtension: %v", err)
		}
		if len(got) != 2 || got[1].Item != "b" {
			t.Fatalf("got %+v, want base plus b", got)
		}
	})

	t.Run("the same new item twice in one batch is still a duplicate", func(t *testing.T) {
		if _, err := PlanExtension(base,
			LedgerItem{Item: "c", Verify: "check c"},
			LedgerItem{Item: "c", Verify: "check c"},
		); !errors.Is(err, ErrLedgerDuplicate) {
			t.Fatalf("err = %v, want ErrLedgerDuplicate", err)
		}
	})
}

// TestAppendItemsLeavesTheInputLedgerAlone matters because a rejected append must
// not half-apply: the caller still holds the ledger it had.
func TestAppendItemsLeavesTheInputLedgerAlone(t *testing.T) {
	base := []LedgerItem{item("a", "check a")}
	if _, err := AppendItems(base, LedgerItem{Item: "b", Verify: ""}); err == nil {
		t.Fatal("expected the incomplete item to be refused")
	}
	if len(base) != 1 || base[0].Item != "a" {
		t.Fatalf("input ledger was mutated: %+v", base)
	}
}

// TestValidateExtensionAcceptsOnlyAppends is the append-and-mark-only rule stated
// as a test. Every rejected case here is a way an agent under completion pressure
// could otherwise edit the definition of done: drop an item it did not do, reorder
// so a prefix check passes, or narrow an item's verify clause until the check it
// promised is no longer the check it faces.
func TestValidateExtensionAcceptsOnlyAppends(t *testing.T) {
	a, b := item("a", "check a"), item("b", "check b")
	prev := []LedgerItem{a, b}

	t.Run("append is allowed", func(t *testing.T) {
		next := []LedgerItem{a, b, item("c", "check c")}
		if err := ValidateExtension(prev, next); err != nil {
			t.Fatalf("append refused: %v", err)
		}
	})
	t.Run("unchanged is allowed", func(t *testing.T) {
		if err := ValidateExtension(prev, []LedgerItem{a, b}); err != nil {
			t.Fatalf("unchanged ledger refused: %v", err)
		}
	})
	t.Run("removal is refused", func(t *testing.T) {
		if err := ValidateExtension(prev, []LedgerItem{a}); !errors.Is(err, ErrLedgerRegressed) {
			t.Fatalf("err = %v, want ErrLedgerRegressed", err)
		}
	})
	t.Run("reorder is refused", func(t *testing.T) {
		if err := ValidateExtension(prev, []LedgerItem{b, a}); !errors.Is(err, ErrLedgerRegressed) {
			t.Fatalf("err = %v, want ErrLedgerRegressed", err)
		}
	})
	t.Run("rewrite is refused", func(t *testing.T) {
		next := []LedgerItem{a, item("b", "just look at it")}
		if err := ValidateExtension(prev, next); !errors.Is(err, ErrLedgerRegressed) {
			t.Fatalf("err = %v, want ErrLedgerRegressed", err)
		}
	})
	t.Run("a rewrite wearing the old id is refused", func(t *testing.T) {
		// The most direct attack on the rule: keep the id, change what it stands
		// for. The self-addressing check is what catches it.
		forged := LedgerItem{ID: b.ID, Item: "b", Verify: "just look at it"}
		if err := ValidateExtension(prev, []LedgerItem{a, forged}); !errors.Is(err, ErrLedgerRegressed) {
			t.Fatalf("err = %v, want ErrLedgerRegressed", err)
		}
	})
}

// TestValidateExtensionChecksAppendedItems covers the checks that only a newly
// appended item can trip, past the shared prefix: an appended item still has to
// carry its text and verify clause, still has to address to its own id, and may not
// repeat an id already in the same batch. The prefix cases (removal, reorder,
// rewrite) live in TestValidateExtensionAcceptsOnlyAppends; these are the tail.
func TestValidateExtensionChecksAppendedItems(t *testing.T) {
	a := item("a", "check a")
	prev := []LedgerItem{a}

	t.Run("an appended item with no verify is incomplete", func(t *testing.T) {
		next := []LedgerItem{a, {ID: ItemID("b", ""), Item: "b", Verify: ""}}
		if err := ValidateExtension(prev, next); !errors.Is(err, ErrLedgerIncomplete) {
			t.Fatalf("err = %v, want ErrLedgerIncomplete", err)
		}
	})
	t.Run("an appended item whose id does not address its content is a rewrite", func(t *testing.T) {
		// A forged id on a brand-new item: it clears no prefix check, so the
		// self-addressing check is the only thing between it and the ledger.
		next := []LedgerItem{a, {ID: "0000000000000000", Item: "b", Verify: "check b"}}
		if err := ValidateExtension(prev, next); !errors.Is(err, ErrLedgerRegressed) {
			t.Fatalf("err = %v, want ErrLedgerRegressed", err)
		}
	})
	t.Run("the same item appended twice in one batch is a duplicate", func(t *testing.T) {
		b := item("b", "check b")
		if err := ValidateExtension(prev, []LedgerItem{a, b, b}); !errors.Is(err, ErrLedgerDuplicate) {
			t.Fatalf("err = %v, want ErrLedgerDuplicate", err)
		}
	})
}

// TestValidateLedgerRejectsATamperedLedger drives the reconcile-time half of the
// extension rule directly on the Status method: the state the status already records
// must stay a prefix of the spec ledger by id, and every ledger item must still
// address to its own content. Each case is a way a ledger edited around the write
// path could otherwise be adopted as the new definition of done.
func TestValidateLedgerRejectsATamperedLedger(t *testing.T) {
	t.Run("a recorded item that is no longer the ledger's is a regression", func(t *testing.T) {
		var st Status
		st.SyncLedger([]LedgerItem{item("a", "check a")}) // status now records item a
		if err := st.ValidateLedger([]LedgerItem{item("z", "check z")}); !errors.Is(err, ErrLedgerRegressed) {
			t.Fatalf("err = %v, want ErrLedgerRegressed", err)
		}
	})
	t.Run("a ledger item with no verify is incomplete", func(t *testing.T) {
		var st Status // no recorded state, so only the ledger itself is checked
		bad := LedgerItem{ID: ItemID("b", ""), Item: "b", Verify: ""}
		if err := st.ValidateLedger([]LedgerItem{bad}); !errors.Is(err, ErrLedgerIncomplete) {
			t.Fatalf("err = %v, want ErrLedgerIncomplete", err)
		}
	})
	t.Run("a ledger item whose id does not address its content is a rewrite", func(t *testing.T) {
		var st Status
		forged := LedgerItem{ID: "0000000000000000", Item: "b", Verify: "check b"}
		if err := st.ValidateLedger([]LedgerItem{forged}); !errors.Is(err, ErrLedgerRegressed) {
			t.Fatalf("err = %v, want ErrLedgerRegressed", err)
		}
	})
}

// TestSyncLedgerStartsEveryItemUnproven is the property that a run begins from a
// record saying nothing is done, and that a ledger which grows mid-run does not
// bring proven items with it.
func TestSyncLedgerStartsEveryItemUnproven(t *testing.T) {
	a, b := item("a", "check a"), item("b", "check b")
	var st Status
	st.SyncLedger([]LedgerItem{a, b})
	if len(st.Ledger) != 2 {
		t.Fatalf("synced %d entries, want 2", len(st.Ledger))
	}
	for _, e := range st.Ledger {
		if e.Proven {
			t.Fatalf("item %s arrived proven", e.ID)
		}
	}
	if err := st.MarkProven(a.ID, "go test ./a: ok", testNow); err != nil {
		t.Fatal(err)
	}

	c := item("c", "check c")
	st.SyncLedger([]LedgerItem{a, b, c})
	if len(st.Ledger) != 3 {
		t.Fatalf("synced %d entries, want 3", len(st.Ledger))
	}
	if !st.Ledger[0].Proven {
		t.Fatal("the existing proof was lost by a re-sync")
	}
	if st.Ledger[2].Proven {
		t.Fatal("a newly planned item arrived proven")
	}
}

func TestMarkProvenRefusesAnItemThatWasNeverPlanned(t *testing.T) {
	var st Status
	st.SyncLedger([]LedgerItem{item("a", "check a")})
	if err := st.MarkProven("deadbeefdeadbeef", "trust me", testNow); !errors.Is(err, ErrLedgerUnknownItem) {
		t.Fatalf("err = %v, want ErrLedgerUnknownItem", err)
	}
}

// TestMarkProvenKeepsTheFirstProof: the record says when an item was settled, not
// when it was most recently re-asserted.
func TestMarkProvenKeepsTheFirstProof(t *testing.T) {
	a := item("a", "check a")
	var st Status
	st.SyncLedger([]LedgerItem{a})
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := st.MarkProven(a.ID, "the real run", first); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkProven(a.ID, "a later claim", first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := st.Ledger[0].Evidence; got != "the real run" {
		t.Fatalf("evidence = %q, want the first proof", got)
	}
	if !st.Ledger[0].ProvenAt.Equal(first) {
		t.Fatalf("provenAt = %v, want %v", st.Ledger[0].ProvenAt, first)
	}
}

// TestLedgerSettledIsNotTrueForAnEmptyLedger: a goal with nothing planned has not
// finished, it has not started. Getting this backwards is the false-complete on
// iteration one.
func TestLedgerSettledIsNotTrueForAnEmptyLedger(t *testing.T) {
	var st Status
	if st.LedgerSettled() {
		t.Fatal("an empty ledger reported settled")
	}
	a := item("a", "check a")
	st.SyncLedger([]LedgerItem{a})
	if st.LedgerSettled() {
		t.Fatal("an unproven ledger reported settled")
	}
	if err := st.MarkProven(a.ID, "ok", testNow); err != nil {
		t.Fatal(err)
	}
	if !st.LedgerSettled() {
		t.Fatal("a fully proven ledger did not report settled")
	}
	if got := st.Unproven(); len(got) != 0 {
		t.Fatalf("unproven = %v, want none", got)
	}
}

// --- the phase --------------------------------------------------------------

// TestGoalPlansBeforeItBuilds is the gate: the first job a planning goal dispatches
// is a plan, not a step, and the ledger exists before any build step is claimed.
func TestGoalPlansBeforeItBuilds(t *testing.T) {
	p := &fakePlanner{items: []LedgerItem{
		{Item: "add the parser", Verify: "go test ./parser"},
		{Item: "wire the CLI flag", Verify: "flynn parse --help mentions it"},
	}}
	h := newPlanHarness(t, neverStop{}, p)
	ref := h.createGoal(t, "g", Spec{Objective: "ship the parser", StopCondition: "it parses"})

	h.reconcile(t, ref)
	if st := h.status(t, ref); st.Phase != PhasePlanning {
		t.Fatalf("phase = %q, want %q", st.Phase, PhasePlanning)
	}
	claimed, err := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(time.Minute)})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("no job dispatched (err=%v)", err)
	}
	if claimed[0].Kind != PlanJobKind {
		t.Fatalf("first dispatched job kind = %q, want %q", claimed[0].Kind, PlanJobKind)
	}
	// Hand it back so the worker can claim and run it for real.
	if err := h.jobs.Fail(h.ctx, claimed[0].ID, "released for the worker", h.clk.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}

	sp := h.spec(t, ref)
	if len(sp.Ledger) != 2 {
		t.Fatalf("spec ledger has %d items, want 2: %+v", len(sp.Ledger), sp.Ledger)
	}
	st := h.status(t, ref)
	if !st.Planned {
		t.Fatal("goal is not marked planned after the plan step")
	}
	if len(st.Ledger) != 2 {
		t.Fatalf("status ledger has %d entries, want 2", len(st.Ledger))
	}
	if got := st.Unproven(); len(got) != 2 {
		t.Fatalf("%d items unproven, want all 2", len(got))
	}

	// Only now does a build step get dispatched.
	h.reconcile(t, ref)
	claimed, err = h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(time.Minute)})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("no build job dispatched (err=%v)", err)
	}
	if claimed[0].Kind != StepJobKind {
		t.Fatalf("second dispatched job kind = %q, want %q", claimed[0].Kind, StepJobKind)
	}
}

// TestPlanningDoesNotSpendTheBuildBudget: planning decides what the budget is spent
// on, so charging the budget for it would make a goal that plans strictly poorer
// than one that does not, and a tight MaxSteps would leave nothing to build with.
func TestPlanningDoesNotSpendTheBuildBudget(t *testing.T) {
	p := &fakePlanner{items: []LedgerItem{{Item: "a", Verify: "check a"}}}
	h := newPlanHarness(t, neverStop{}, p)
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", MaxSteps: 1})

	h.reconcile(t, ref) // dispatch the plan
	if _, err := h.w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}
	h.reconcile(t, ref) // observe the plan, then dispatch the first build step

	st := h.status(t, ref)
	if st.Steps != 0 {
		t.Fatalf("steps = %d after planning, want 0", st.Steps)
	}
	if st.Phase != PhaseRunning {
		t.Fatalf("phase = %q, want %q: the one step of budget was spent on planning", st.Phase, PhaseRunning)
	}
}

// TestAnEmptyPlanStallsTheGoal: a planner that produced nothing leaves no
// definition of done. Building anyway is how a run claims success against a record
// that never said what success was.
func TestAnEmptyPlanStallsTheGoal(t *testing.T) {
	h := newPlanHarness(t, neverStop{}, &fakePlanner{})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	h.reconcile(t, ref)
	if _, err := h.w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q", st.Phase, PhaseStalled)
	}
	if st.Message != "planning produced an empty ledger" {
		t.Fatalf("message = %q", st.Message)
	}
}

// TestPlanningRunsOnce pins that the planning mark, not the ledger's emptiness, is
// what ends the phase, and that a re-reconcile does not plan the goal again.
func TestPlanningRunsOnce(t *testing.T) {
	p := &fakePlanner{items: []LedgerItem{{Item: "a", Verify: "check a"}}}
	h := newPlanHarness(t, neverStop{}, p)
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	for range 4 {
		h.reconcile(t, ref)
		if _, err := h.w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("worker: %v", err)
		}
	}
	if p.calls != 1 {
		t.Fatalf("planner ran %d times, want 1", p.calls)
	}
}

// TestAnEditedLedgerStallsTheGoal is the reconcile-time half of the extension rule:
// a ledger edited around the write path is refused rather than adopted as the new
// definition of done. The goal settles as stalled with the cause, because a run
// whose record was tampered with has failed, not paused.
func TestAnEditedLedgerStallsTheGoal(t *testing.T) {
	p := &fakePlanner{items: []LedgerItem{
		{Item: "a", Verify: "check a"},
		{Item: "b", Verify: "check b"},
	}}
	h := newPlanHarness(t, neverStop{}, p)
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref)
	if _, err := h.w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}
	h.reconcile(t, ref)

	// Drop the second item straight onto the store, the way an agent with a write
	// tool and a deadline would.
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := DecodeSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	sp.Ledger = sp.Ledger[:1]
	raw, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	r.Spec = raw
	if _, err := h.store.Put(h.ctx, r); err != nil {
		t.Fatal(err)
	}

	h.reconcile(t, ref)
	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q", st.Phase, PhaseStalled)
	}
	if !containsAll(st.Message, "ledger", "regressed") {
		t.Fatalf("message does not name the cause: %q", st.Message)
	}
}

// TestAPlanJobWithNoPlannerFailsTerminally: a reconciler gated on planning wired to
// a worker that cannot plan is a misconfiguration no retry fixes, and the goal would
// otherwise sit unplanned and undispatched with nothing saying why.
func TestAPlanJobWithNoPlannerFailsTerminally(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg, resource.WithClock(m))
	q := jobs.NewMemory(jobs.WithClock(m))
	gr := NewReconciler(store, q, m, neverStop{}, WithPlanning())
	w := NewWorker(store, q, m, &fakeExec{}, WithLease(time.Minute)) // no planner
	h := &harness{ctx: context.Background(), store: store, jobs: q, gr: gr, clk: m}

	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref)
	if _, err := w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q", st.Phase, PhaseStalled)
	}
	if !containsAll(st.Message, "planner") {
		t.Fatalf("message does not name the missing planner: %q", st.Message)
	}
}

// TestAGoalWithoutPlanningIsUnchanged: the gate is opt-in, so every goal composed
// before planning existed still dispatches a build step first and never waits on a
// ledger it has no way to produce.
func TestAGoalWithoutPlanningIsUnchanged(t *testing.T) {
	h := newHarness(t, stopAfter{at: 1})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref)
	if st := h.status(t, ref); st.Phase != PhaseRunning {
		t.Fatalf("phase = %q, want %q", st.Phase, PhaseRunning)
	}
	h.completeStep(t) // asserts the job kind is a build step
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
