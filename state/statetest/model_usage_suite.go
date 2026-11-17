package statetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/usage"
)

// RunModelUsageSuite runs the [state.ModelUsageStore] contract against stores built by
// newStore. Each subtest gets a fresh, empty store. A host that stores usage in
// its own database runs it to show its store returns the same records, in the
// same order, and so the same [usage.Sum] totals, as every other.
func RunModelUsageSuite(t *testing.T, newStore func() state.ModelUsageStore) {
	t.Helper()
	for _, c := range usageCases {
		t.Run(c.name, func(t *testing.T) { c.run(t, newStore()) })
	}
}

var usageCases = []struct {
	name string
	run  func(*testing.T, state.ModelUsageStore)
}{
	{"RoundTrip", testUsageRoundTrip},
	{"IdempotentAppend", testUsageIdempotent},
	{"ConflictRefused", testUsageConflict},
	{"InvalidRefused", testUsageInvalid},
	{"BatchIsAtomic", testUsageBatchAtomic},
	{"BatchDuplicates", testUsageBatchDuplicates},
	{"ByRun", testUsageByRun},
	{"ByTree", testUsageByTree},
	{"Window", testUsageWindow},
	{"Order", testUsageOrder},
	{"TimeZones", testUsageTimeZones},
	{"SumParity", testUsageSumParity},
	{"ConcurrentRetry", testUsageConcurrentRetry},
}

var usageT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// urec builds a valid, priced record whose figures scale with n, so two records
// built with different n are never identical.
func urec(run, parent, call, principal string, at time.Time, n int64) usage.Record {
	return usage.Record{
		Run: run, ParentRun: parent, Principal: principal, Call: call,
		Provider: "anthropic", Model: "claude-opus-5-5", At: at,
		Tokens: usage.Tokens{
			Input:      usage.Count{N: 100 * n, Basis: usage.Reported},
			Output:     usage.Count{N: 10 * n, Basis: usage.Reported},
			CacheRead:  usage.Count{N: 20 * n, Basis: usage.Reported},
			CacheWrite: usage.Count{N: 5 * n, Basis: usage.Reported},
		},
		Cost: usage.Cost{Known: true, Amount: usage.Nanos(1_000_000 * n), Currency: "USD", Basis: usage.Derived, Price: "test-table"},
	}
}

func mustAppend(t *testing.T, s state.ModelUsageStore, rs ...usage.Record) {
	t.Helper()
	if err := s.Append(context.Background(), rs...); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func mustRun(t *testing.T, s state.ModelUsageStore, run string) []usage.Record {
	t.Helper()
	got, err := s.ByRun(context.Background(), run)
	if err != nil {
		t.Fatalf("ByRun(%q): %v", run, err)
	}
	return got
}

// wantCalls fails unless got holds exactly the calls named, in that order.
func wantCalls(t *testing.T, what string, got []usage.Record, calls ...string) {
	t.Helper()
	ids := make([]string, len(got))
	for i, r := range got {
		ids[i] = r.Call
	}
	if len(ids) != len(calls) || (len(ids) > 0 && !reflect.DeepEqual(ids, calls)) {
		t.Errorf("%s = %v, want %v", what, ids, calls)
	}
}

func testUsageRoundTrip(t *testing.T, s state.ModelUsageStore) {
	in := urec("r1", "root", "c1", "alice", usageT0.Add(123456789*time.Nanosecond), 3)
	in.Cost.Reason = "kept"
	in.Tokens.Output = usage.Count{N: 7, Basis: usage.Estimated, Method: "recounted from stream deltas"}
	mustAppend(t, s, in)
	got := mustRun(t, s, "r1")
	if len(got) != 1 || !state.SameModelUsageRecord(got[0], in) {
		t.Fatalf("ByRun = %+v, want the appended record %+v", got, in)
	}
	if !got[0].At.Equal(in.At) {
		t.Errorf("At = %v, want %v: the nanoseconds must survive", got[0].At, in.At)
	}
	if got[0].Tokens != in.Tokens || got[0].Cost != in.Cost {
		t.Errorf("figures changed in storage:\n got %+v\nwant %+v", got[0], in)
	}
}

func testUsageIdempotent(t *testing.T, s state.ModelUsageStore) {
	r := urec("r1", "", "c1", "alice", usageT0, 1)
	mustAppend(t, s, r)
	mustAppend(t, s, r)
	mustAppend(t, s, r, r)
	wantCalls(t, "ByRun after the same record four times", mustRun(t, s, "r1"), "c1")
}

func testUsageConflict(t *testing.T, s state.ModelUsageStore) {
	first := urec("r1", "", "c1", "alice", usageT0, 1)
	mustAppend(t, s, first)
	for name, other := range map[string]usage.Record{
		"different tokens":    urec("r1", "", "c1", "alice", usageT0, 2),
		"different principal": urec("r1", "", "c1", "bob", usageT0, 1),
		"different time":      urec("r1", "", "c1", "alice", usageT0.Add(time.Second), 1),
		"different parent":    urec("r1", "other", "c1", "alice", usageT0, 1),
	} {
		if err := s.Append(context.Background(), other); !errors.Is(err, state.ErrConflict) {
			t.Errorf("%s: Append = %v, want ErrConflict", name, err)
		}
	}
	got := mustRun(t, s, "r1")
	if len(got) != 1 || !state.SameModelUsageRecord(got[0], first) {
		t.Errorf("a refused append changed the stored record: %+v", got)
	}
}

func testUsageInvalid(t *testing.T, s state.ModelUsageStore) {
	noProvider := urec("r1", "", "c1", "", usageT0, 1)
	noProvider.Provider = ""
	noBasis := urec("r1", "", "c1", "", usageT0, 1)
	noBasis.Tokens.Input.Basis = ""
	bad := map[string]usage.Record{
		"no run":      urec("", "", "c1", "", usageT0, 1),
		"no call":     urec("r1", "", "", "", usageT0, 1),
		"no time":     urec("r1", "", "c1", "", time.Time{}, 1),
		"no provider": noProvider,
		"no basis":    noBasis,
	}
	for name, r := range bad {
		if err := s.Append(context.Background(), r); !errors.Is(err, state.ErrInvalid) {
			t.Errorf("%s: Append = %v, want ErrInvalid", name, err)
		}
	}
	if got := mustRun(t, s, "r1"); len(got) != 0 {
		t.Errorf("invalid records were stored: %+v", got)
	}
}

func testUsageBatchAtomic(t *testing.T, s state.ModelUsageStore) {
	ctx := context.Background()
	mustAppend(t, s, urec("r1", "", "c1", "", usageT0, 1))

	if err := s.Append(ctx, urec("r2", "", "good", "", usageT0, 1), urec("r2", "", "", "", usageT0, 1)); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("batch with an invalid record: Append = %v, want ErrInvalid", err)
	}
	if err := s.Append(ctx, urec("r3", "", "good", "", usageT0, 1), urec("r1", "", "c1", "", usageT0, 9)); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("batch with a conflicting record: Append = %v, want ErrConflict", err)
	}
	for _, run := range []string{"r2", "r3"} {
		if got := mustRun(t, s, run); len(got) != 0 {
			t.Errorf("a refused batch stored part of itself under %s: %+v", run, got)
		}
	}
}

func testUsageBatchDuplicates(t *testing.T, s state.ModelUsageStore) {
	ctx := context.Background()
	a := urec("r1", "", "c1", "", usageT0, 1)
	mustAppend(t, s, a, urec("r1", "", "c2", "", usageT0.Add(time.Second), 2), a)
	wantCalls(t, "ByRun", mustRun(t, s, "r1"), "c1", "c2")

	if err := s.Append(ctx, urec("r9", "", "c1", "", usageT0, 1), urec("r9", "", "c1", "", usageT0, 2)); !errors.Is(err, state.ErrConflict) {
		t.Errorf("one batch naming a key twice with different content: Append = %v, want ErrConflict", err)
	}
	if err := s.Append(ctx); err != nil {
		t.Errorf("Append of nothing = %v, want nil", err)
	}
}

func testUsageByRun(t *testing.T, s state.ModelUsageStore) {
	mustAppend(t, s,
		urec("r1", "", "c1", "", usageT0, 1),
		urec("r1", "", "c2", "", usageT0.Add(time.Second), 1),
		urec("child", "r1", "c1", "", usageT0.Add(2*time.Second), 1),
		urec("r2", "", "c1", "", usageT0, 1),
	)
	wantCalls(t, "ByRun(r1)", mustRun(t, s, "r1"), "c1", "c2")
	wantCalls(t, "ByRun(child)", mustRun(t, s, "child"), "c1")
	if got := mustRun(t, s, "nobody"); got == nil || len(got) != 0 {
		t.Errorf("ByRun(unknown) = %#v, want an empty list and no error", got)
	}
	if got := mustRun(t, s, ""); len(got) != 0 {
		t.Errorf("ByRun(\"\") = %+v, want none", got)
	}
}

func testUsageByTree(t *testing.T, s state.ModelUsageStore) {
	ctx := context.Background()
	at := func(i int) time.Time { return usageT0.Add(time.Duration(i) * time.Second) }
	mustAppend(t, s,
		urec("root", "", "root-1", "", at(0), 1),
		urec("kid", "root", "kid-1", "", at(1), 1),
		urec("kid", "root", "kid-2", "", at(2), 1),
		urec("grandkid", "kid", "grand-1", "", at(3), 1),
		urec("great", "grandkid", "great-1", "", at(4), 1),
		urec("sibling", "root", "sib-1", "", at(5), 1),
		urec("stranger", "", "str-1", "", at(6), 1),
		urec("stranger-kid", "stranger", "strk-1", "", at(7), 1),
	)
	tree := func(root string) []usage.Record {
		got, err := s.ByTree(ctx, root)
		if err != nil {
			t.Fatalf("ByTree(%q): %v", root, err)
		}
		return got
	}
	wantCalls(t, "ByTree(root)", tree("root"), "root-1", "kid-1", "kid-2", "grand-1", "great-1", "sib-1")
	wantCalls(t, "ByTree(kid)", tree("kid"), "kid-1", "kid-2", "grand-1", "great-1")
	wantCalls(t, "ByTree(great)", tree("great"), "great-1")
	wantCalls(t, "ByTree(stranger)", tree("stranger"), "str-1", "strk-1")
	if got := tree("unknown"); got == nil || len(got) != 0 {
		t.Errorf("ByTree(unknown) = %#v, want an empty list", got)
	}
	if got := tree(""); len(got) != 0 {
		t.Errorf("ByTree(\"\") = %+v: an empty root must not select every run without a parent", got)
	}

	// A run whose records name different parents belongs to both trees.
	mustAppend(t, s, urec("shared", "root", "sh-1", "", at(8), 1), urec("shared", "stranger", "sh-2", "", at(9), 1))
	wantCalls(t, "ByTree(stranger) with a shared run", tree("stranger"), "str-1", "strk-1", "sh-1", "sh-2")

	// A cycle in ParentRun must end the walk, not loop.
	mustAppend(t, s, urec("loop-a", "loop-b", "a-1", "", at(10), 1), urec("loop-b", "loop-a", "b-1", "", at(11), 1))
	wantCalls(t, "ByTree(loop-a)", tree("loop-a"), "a-1", "b-1")
}

func testUsageWindow(t *testing.T, s state.ModelUsageStore) {
	ctx := context.Background()
	at := func(i int) time.Time { return usageT0.Add(time.Duration(i) * time.Hour) }
	mustAppend(t, s,
		urec("r1", "", "h0", "alice", at(0), 1),
		urec("r1", "", "h1", "bob", at(1), 1),
		urec("r2", "", "h2", "alice", at(2), 1),
		urec("r2", "", "h3", "", at(3), 1),
	)
	win := func(from, to time.Time, principal string) []usage.Record {
		got, err := s.Window(ctx, from, to, principal)
		if err != nil {
			t.Fatalf("Window: %v", err)
		}
		return got
	}
	var none time.Time
	wantCalls(t, "Window(all)", win(none, none, ""), "h0", "h1", "h2", "h3")
	wantCalls(t, "Window(from h1)", win(at(1), none, ""), "h1", "h2", "h3")
	wantCalls(t, "Window(to h2): the end is exclusive", win(none, at(2), ""), "h0", "h1")
	wantCalls(t, "Window(h1, h3)", win(at(1), at(3), ""), "h1", "h2")
	wantCalls(t, "Window(alice)", win(none, none, "alice"), "h0", "h2")
	wantCalls(t, "Window(h1, h3, alice)", win(at(1), at(3), "alice"), "h2")
	wantCalls(t, "Window(nobody)", win(none, none, "nobody"))
	wantCalls(t, "Window(empty range)", win(at(2), at(2), ""))
	wantCalls(t, "Window(reversed range)", win(at(3), at(1), ""))
	wantCalls(t, "Window(a nanosecond either side of h1)", win(at(1).Add(-time.Nanosecond), at(1).Add(time.Nanosecond), ""), "h1")
}

func testUsageOrder(t *testing.T, s state.ModelUsageStore) {
	ctx := context.Background()
	// Appended out of order, with ties on time and on call id across runs.
	mustAppend(t, s, urec("rb", "", "c2", "", usageT0.Add(time.Second), 1))
	mustAppend(t, s, urec("rb", "", "c1", "", usageT0.Add(time.Second), 1))
	mustAppend(t, s, urec("ra", "", "c1", "", usageT0.Add(time.Second), 1))
	mustAppend(t, s, urec("rc", "", "c9", "", usageT0, 1))

	got, err := s.Window(ctx, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range got {
		order = append(order, r.Run+"/"+r.Call)
	}
	want := []string{"rc/c9", "ra/c1", "rb/c1", "rb/c2"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v (At, then Call, then Run)", order, want)
	}
}

func testUsageTimeZones(t *testing.T, s state.ModelUsageStore) {
	local := usageT0.In(time.FixedZone("UTC+7", 7*60*60))
	mustAppend(t, s, urec("r1", "", "c1", "", local, 1))
	// The same instant reported in UTC is the same record, not a conflict.
	mustAppend(t, s, urec("r1", "", "c1", "", usageT0, 1))
	got := mustRun(t, s, "r1")
	if len(got) != 1 || !got[0].At.Equal(usageT0) {
		t.Fatalf("ByRun = %+v, want one record at %v", got, usageT0)
	}
	if _, off := got[0].At.Zone(); off != 0 {
		t.Errorf("At has offset %ds, want UTC", off)
	}
	win, err := s.Window(context.Background(), usageT0, usageT0.Add(time.Nanosecond), "")
	if err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "Window around the instant", win, "c1")
}

// testUsageSumParity is the guarantee a budget relies on: totals taken from the
// store equal totals taken from what was put in, whichever query reads them.
func testUsageSumParity(t *testing.T, s state.ModelUsageStore) {
	ctx := context.Background()
	var all []usage.Record
	for i := range 12 {
		r := urec(fmt.Sprintf("r%d", i%3), "", fmt.Sprintf("c%d", i), "alice", usageT0.Add(time.Duration(i)*time.Minute), int64(i+1))
		if i%3 != 0 {
			r.ParentRun = "r0"
		}
		switch i {
		case 4:
			r.Cost = usage.Cost{Known: false, Reason: "no price row"}
		case 5:
			r.Cost.Currency = "EUR"
		case 6:
			r.Tokens.Output = usage.Count{N: 3, Basis: usage.Estimated, Method: "recounted"}
		}
		all = append(all, r)
	}
	mustAppend(t, s, all...)
	mustAppend(t, s, all...)

	want, err := usage.Sum(all)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]func() ([]usage.Record, error){
		"Window": func() ([]usage.Record, error) { return s.Window(ctx, time.Time{}, time.Time{}, "") },
		"ByTree": func() ([]usage.Record, error) { return s.ByTree(ctx, "r0") },
		"ByRun": func() ([]usage.Record, error) {
			var got []usage.Record
			for _, run := range []string{"r0", "r1", "r2"} {
				got = append(got, mustRun(t, s, run)...)
			}
			return got, nil
		},
	}
	for name, read := range reads {
		got, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sum, err := usage.Sum(got)
		if err != nil {
			t.Fatalf("%s: Sum: %v", name, err)
		}
		if !reflect.DeepEqual(sum, want) {
			t.Errorf("%s: Sum over the store = %+v, want %+v", name, sum, want)
		}
	}
}

// testUsageConcurrentRetry has several collectors send the same records at once,
// as a retrying fleet would. The call must be counted once.
func testUsageConcurrentRetry(t *testing.T, s state.ModelUsageStore) {
	batch := []usage.Record{
		urec("r1", "", "c1", "", usageT0, 1),
		urec("r1", "", "c2", "", usageT0.Add(time.Second), 2),
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Append(context.Background(), batch...)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent retry of identical records failed: %v", err)
		}
	}
	wantCalls(t, "ByRun", mustRun(t, s, "r1"), "c1", "c2")
}
