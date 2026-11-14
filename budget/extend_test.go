package budget_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// trail is the order the collaborators were called in, plus the ceiling the ledger
// showed each time. It is how the tests see that the raise comes first, the record
// second and the reopen last.
type trail struct {
	mu     sync.Mutex
	steps  []string
	ceil   map[string]budget.Limits
	ledger func() budget.Limits
}

func (t *trail) note(step string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, step)
	if t.ceil == nil {
		t.ceil = map[string]budget.Limits{}
	}
	if t.ledger != nil {
		t.ceil[step] = t.ledger()
	}
}

type fakeRecorder struct {
	trail *trail
	err   error
	got   []budget.Extended
	mu    sync.Mutex
}

func (f *fakeRecorder) RecordExtended(_ context.Context, e budget.Extended) error {
	f.trail.note("record")
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, e)
	return nil
}

type fakeGoal struct {
	trail    *trail
	admitErr error
	reopen   error
	reopened []budget.Limits
	mu       sync.Mutex
}

func (f *fakeGoal) Admit(context.Context) error {
	f.trail.note("admit")
	return f.admitErr
}

func (f *fakeGoal) Reopen(_ context.Context, c budget.Limits) error {
	f.trail.note("reopen")
	if f.reopen != nil {
		return f.reopen
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reopened = append(f.reopened, c)
	return nil
}

type rig struct {
	t      *testing.T
	store  resource.Store
	ledger *budget.Ledger
	trail  *trail
	rec    *fakeRecorder
	goal   *fakeGoal
}

func newRig(t *testing.T, start, limit budget.Limits) *rig {
	t.Helper()
	store := newStore(t)
	base := budget.NewLedger(store)
	openBudget(t, base, "run", start)
	tr := &trail{}
	r := &rig{
		t: t, store: store, ledger: base.WithExtensionCap(limit), trail: tr,
		rec: &fakeRecorder{trail: tr}, goal: &fakeGoal{trail: tr},
	}
	tr.ledger = r.ceiling
	return r
}

func (r *rig) ceiling() budget.Limits {
	r.t.Helper()
	res, err := r.store.Get(context.Background(), budget.Kind, resource.Scope{}, "run")
	if err != nil {
		r.t.Fatal(err)
	}
	spec, err := budget.DecodeSpec(res)
	if err != nil {
		r.t.Fatal(err)
	}
	return spec.Limits
}

func (r *rig) extend(ctx context.Context, to budget.Limits) (budget.Extended, error) {
	return r.ledger.Extend(ctx, "run", resource.Scope{}, budget.Extension{
		To: to, Principal: "ada", Reason: "more room", Record: r.rec, Goal: r.goal,
	})
}

func TestExtendRaisesRecordsThenReopensInThatOrder(t *testing.T) {
	r := newRig(t, budget.Limits{Tokens: 100, Cost: 1}, budget.Limits{Tokens: 500, Cost: 5})
	got, err := r.extend(context.Background(), budget.Limits{Tokens: 250, Cost: 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"admit", "record", "reopen"}; len(r.trail.steps) != 3 || r.trail.steps[0] != want[0] || r.trail.steps[1] != want[1] || r.trail.steps[2] != want[2] {
		t.Fatalf("calls = %v, want %v", r.trail.steps, want)
	}
	// The ledger was already raised when the record was written, and not before the
	// admission: that is the order a crash between the steps is safe under.
	if c := r.trail.ceil["admit"]; c != (budget.Limits{Tokens: 100, Cost: 1}) {
		t.Errorf("ceiling at admit = %+v, want it untouched", c)
	}
	if c := r.trail.ceil["record"]; c != (budget.Limits{Tokens: 250, Cost: 2}) {
		t.Errorf("ceiling at record = %+v, want it already raised", c)
	}
	want := budget.Extended{
		Run: "run", Principal: "ada", Reason: "more room",
		From: budget.Limits{Tokens: 100, Cost: 1}, To: budget.Limits{Tokens: 250, Cost: 2},
		Added: budget.Limits{Tokens: 150, Cost: 1}, Total: budget.Limits{Tokens: 150, Cost: 1},
		Cap: budget.Limits{Tokens: 500, Cost: 5},
	}
	if got != want {
		t.Errorf("extended = %+v, want %+v", got, want)
	}
	if len(r.rec.got) != 1 || r.rec.got[0] != want {
		t.Errorf("recorded %+v, want exactly %+v", r.rec.got, want)
	}
	if len(r.goal.reopened) != 1 || r.goal.reopened[0] != want.To {
		t.Errorf("reopened with %+v, want %+v", r.goal.reopened, want.To)
	}
}

func TestExtendWithNoCapRefusesAndChangesNothing(t *testing.T) {
	r := newRig(t, budget.Limits{Tokens: 100}, budget.Limits{})
	_, err := r.extend(context.Background(), budget.Limits{Tokens: 200})
	if !errors.Is(err, budget.ErrNoExtensionCap) {
		t.Fatalf("err = %v, want ErrNoExtensionCap", err)
	}
	if c := r.ceiling(); c.Tokens != 100 {
		t.Errorf("ceiling = %+v, want it unchanged", c)
	}
	if len(r.trail.steps) != 0 {
		t.Errorf("collaborators called: %v", r.trail.steps)
	}
	// A ledger that never had WithExtensionCap called is the same: the zero value of
	// the cap is a refusal.
	bare := budget.NewLedger(r.store)
	if _, err := bare.Extend(context.Background(), "run", resource.Scope{}, budget.Extension{
		To: budget.Limits{Tokens: 200}, Principal: "ada", Record: r.rec, Goal: r.goal,
	}); !errors.Is(err, budget.ErrNoExtensionCap) {
		t.Fatalf("bare ledger err = %v, want ErrNoExtensionCap", err)
	}
}

func TestExtendRefusals(t *testing.T) {
	humanCtx := context.Background()
	tests := []struct {
		name  string
		ctx   context.Context
		start budget.Limits
		limit budget.Limits
		to    budget.Limits
		who   string
		goal  error
		want  error
	}{
		{"action context", budget.Into(humanCtx, "run"), budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500}, budget.Limits{Tokens: 200}, "ada", nil, budget.ErrNotHuman},
		{"no principal", humanCtx, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500}, budget.Limits{Tokens: 200}, "", nil, budget.ErrNotHuman},
		{"blank principal", humanCtx, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500}, budget.Limits{Tokens: 200}, "  ", nil, budget.ErrNotHuman},
		{"nothing asked", humanCtx, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500}, budget.Limits{}, "ada", nil, budget.ErrNotRaised},
		{"negative", humanCtx, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500}, budget.Limits{Tokens: -5}, "ada", nil, budget.ErrLowered},
		{"lower tokens", humanCtx, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500}, budget.Limits{Tokens: 50}, "ada", nil, budget.ErrLowered},
		{"lower cost", humanCtx, budget.Limits{Cost: 4}, budget.Limits{Cost: 9}, budget.Limits{Cost: 2}, "ada", nil, budget.ErrLowered},
		{"axis with no ceiling", humanCtx, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500, Cost: 5}, budget.Limits{Cost: 3}, "ada", nil, budget.ErrNoCeiling},
		{"past the cap", humanCtx, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500}, budget.Limits{Tokens: 601}, "ada", nil, budget.ErrExtensionCap},
		{"axis the cap forbids", humanCtx, budget.Limits{Tokens: 100, Cost: 1}, budget.Limits{Tokens: 500}, budget.Limits{Cost: 2}, "ada", nil, budget.ErrExtensionCap},
		{"goal killed", humanCtx, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500}, budget.Limits{Tokens: 200}, "ada", budget.ErrKilled, budget.ErrKilled},
		{"goal not on a budget stop", humanCtx, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500}, budget.Limits{Tokens: 200}, "ada", budget.ErrNotBudgetStop, budget.ErrNotBudgetStop},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.start, tc.limit)
			r.goal.admitErr = tc.goal
			before := r.ceiling()
			_, err := r.ledger.Extend(tc.ctx, "run", resource.Scope{}, budget.Extension{
				To: tc.to, Principal: tc.who, Record: r.rec, Goal: r.goal,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if c := r.ceiling(); c != before {
				t.Errorf("ceiling = %+v, want %+v unchanged by a refusal", c, before)
			}
			if len(r.rec.got) != 0 || len(r.goal.reopened) != 0 {
				t.Errorf("a refusal recorded %v and reopened %v", r.rec.got, r.goal.reopened)
			}
		})
	}
}

func TestExtendRefusesWithoutRecorderOrGoal(t *testing.T) {
	r := newRig(t, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500})
	for name, ext := range map[string]budget.Extension{
		"no recorder": {To: budget.Limits{Tokens: 200}, Principal: "ada", Goal: r.goal},
		"no goal":     {To: budget.Limits{Tokens: 200}, Principal: "ada", Record: r.rec},
	} {
		if _, err := r.ledger.Extend(context.Background(), "run", resource.Scope{}, ext); !errors.Is(err, budget.ErrExtendUnwired) {
			t.Errorf("%s: err = %v, want ErrExtendUnwired", name, err)
		}
	}
	if c := r.ceiling(); c.Tokens != 100 {
		t.Errorf("ceiling = %+v, want unchanged", c)
	}
}

func TestExtendRefusalErrorsCarryStableCodes(t *testing.T) {
	seen := map[string]error{}
	for _, err := range []error{
		budget.ErrNoExtensionCap, budget.ErrExtensionCap, budget.ErrNotHuman, budget.ErrNotRaised,
		budget.ErrLowered, budget.ErrNoCeiling, budget.ErrNotBudgetStop, budget.ErrKilled, budget.ErrExtendUnwired,
	} {
		code := fault.CodeOf(err)
		if code == "" {
			t.Errorf("%v has no code", err)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("%v and %v share code %q, so a client cannot tell them apart", prev, err, code)
		}
		seen[code] = err
	}
}

// A crash between the raise and the record leaves the ceiling raised, nothing recorded
// and the goal still stopped. Repeating the request finishes the job and raises
// nothing a second time.
func TestExtendRetryAfterFailedRecordConverges(t *testing.T) {
	r := newRig(t, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500})
	r.rec.err = errors.New("spine unavailable")
	if _, err := r.extend(context.Background(), budget.Limits{Tokens: 300}); err == nil {
		t.Fatal("extend succeeded with a recorder that failed")
	}
	if c := r.ceiling(); c.Tokens != 300 {
		t.Fatalf("ceiling = %+v, want the raise to stand", c)
	}
	if len(r.goal.reopened) != 0 {
		t.Fatalf("goal reopened %v although the extension was never recorded", r.goal.reopened)
	}

	r.rec.err = nil
	got, err := r.extend(context.Background(), budget.Limits{Tokens: 300})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got.Added.Tokens != 0 || got.Total.Tokens != 200 {
		t.Errorf("retry = %+v, want nothing added and the total left at 200", got)
	}
	if len(r.rec.got) != 1 || len(r.goal.reopened) != 1 {
		t.Errorf("retry recorded %d and reopened %d, want 1 and 1", len(r.rec.got), len(r.goal.reopened))
	}
	if c := r.ceiling(); c.Tokens != 300 {
		t.Errorf("ceiling = %+v after the retry", c)
	}
}

func TestExtendFailedReopenLeavesRaisedCeilingAndReportsIt(t *testing.T) {
	r := newRig(t, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500})
	r.goal.reopen = errors.New("goal store down")
	got, err := r.extend(context.Background(), budget.Limits{Tokens: 300})
	if err == nil {
		t.Fatal("extend succeeded with a goal that would not reopen")
	}
	if got.To.Tokens != 300 || len(r.rec.got) != 1 {
		t.Errorf("got %+v, recorded %d: the raise and its record should both be reported", got, len(r.rec.got))
	}
}

func TestExtendUnknownRunIsNotFound(t *testing.T) {
	r := newRig(t, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500})
	_, err := r.ledger.Extend(context.Background(), "nope", resource.Scope{}, budget.Extension{
		To: budget.Limits{Tokens: 200}, Principal: "ada", Record: r.rec, Goal: r.goal,
	})
	if !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(r.rec.got) != 0 {
		t.Errorf("recorded an extension for a run that does not exist")
	}
}

// The ceiling only ever rises, and the total raised never passes the cap, whatever
// sequence of requests arrives: small ones, repeats, lower ones, ones past the cap.
func TestExtendMonotonicAndCappedAcrossAnySequenceProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		startTokens := rapid.Int64Range(1, 1_000).Draw(rt, "startTokens")
		startCost := float64(rapid.IntRange(1, 100).Draw(rt, "startCost"))
		capTokens := rapid.Int64Range(1, 2_000).Draw(rt, "capTokens")
		capCost := float64(rapid.IntRange(1, 200).Draw(rt, "capCost"))
		start := budget.Limits{Tokens: startTokens, Cost: startCost}
		limit := budget.Limits{Tokens: capTokens, Cost: capCost}
		store := newStore(t)
		base := budget.NewLedger(store)
		openBudget(t, base, "run", start)
		ledger := base.WithExtensionCap(limit)
		tr := &trail{}
		rec, goal := &fakeRecorder{trail: tr}, &fakeGoal{trail: tr}

		ceiling := func() budget.Limits {
			res, err := store.Get(context.Background(), budget.Kind, resource.Scope{}, "run")
			if err != nil {
				rt.Fatal(err)
			}
			spec, err := budget.DecodeSpec(res)
			if err != nil {
				rt.Fatal(err)
			}
			return spec.Limits
		}

		prev := ceiling()
		for i := range rapid.IntRange(1, 60).Draw(rt, "requests") {
			ask := budget.Limits{
				Tokens: rapid.Int64Range(0, 3_000).Draw(rt, "askTokens"),
				Cost:   float64(rapid.IntRange(0, 300).Draw(rt, "askCost")),
			}
			_, err := ledger.Extend(context.Background(), "run", resource.Scope{}, budget.Extension{
				To: ask, Principal: "ada", Record: rec, Goal: goal,
			})
			now := ceiling()
			if now.Tokens < prev.Tokens || now.Cost < prev.Cost {
				rt.Fatalf("request %d lowered the ceiling from %+v to %+v (%v)", i, prev, now, err)
			}
			if now.Tokens-startTokens > capTokens || now.Cost-startCost > capCost {
				rt.Fatalf("request %d put the ceiling at %+v: raised past the cap %+v from %+v", i, now, limit, start)
			}
			if err != nil && now != prev {
				rt.Fatalf("request %d failed with %v but moved the ceiling %+v -> %+v", i, err, prev, now)
			}
			prev = now
		}
	})
}

// Many small extensions are not a way round the cap: the total they add is what is
// bounded, not each one.
func TestExtendCapHoldsAcrossManySmallExtensions(t *testing.T) {
	r := newRig(t, budget.Limits{Tokens: 1_000}, budget.Limits{Tokens: 50})
	granted := int64(0)
	for i := range 200 {
		to := r.ceiling()
		to.Tokens++
		if _, err := r.extend(context.Background(), to); err == nil {
			granted++
		} else if !errors.Is(err, budget.ErrExtensionCap) {
			t.Fatalf("extension %d: %v", i, err)
		}
	}
	if granted != 50 {
		t.Fatalf("granted %d one-token extensions, want exactly the cap, 50", granted)
	}
	if c := r.ceiling(); c.Tokens != 1_050 {
		t.Fatalf("ceiling = %+v, want 1050", c)
	}
}

// Extensions arriving together cannot jointly pass a cap that each passes alone.
func TestExtendConcurrentExtensionsCannotJointlyPassTheCap(t *testing.T) {
	r := newRig(t, budget.Limits{Tokens: 1_000}, budget.Limits{Tokens: 100})
	var wg sync.WaitGroup
	var granted atomic.Int64
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each asks for 60 more than the ceiling it last saw: any two of these
			// together exceed the cap of 100.
			cur := r.ceiling()
			to := budget.Limits{Tokens: cur.Tokens + 60}
			if _, err := r.extend(context.Background(), to); err == nil {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	c := r.ceiling()
	if c.Tokens-1_000 > 100 {
		t.Fatalf("ceiling = %d, raised %d past the cap of 100", c.Tokens, c.Tokens-1_000)
	}
	if granted.Load() == 0 {
		t.Fatal("no extension was granted")
	}
}

func TestExtendStatusExtendedIsAbsentUntilUsed(t *testing.T) {
	r := newRig(t, budget.Limits{Tokens: 100}, budget.Limits{Tokens: 500})
	res, err := r.store.Get(context.Background(), budget.Kind, resource.Scope{}, "run")
	if err != nil {
		t.Fatal(err)
	}
	st, err := budget.DecodeStatus(res)
	if err != nil {
		t.Fatal(err)
	}
	if st.Extended != nil {
		t.Fatalf("Extended = %+v on a run never extended", st.Extended)
	}
}
