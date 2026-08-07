package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

func TestUsageCountsAndIgnored(t *testing.T) {
	u := state.MemoryUsage{PushCount: 50}
	if !u.Ignored() {
		t.Fatal("pushed 50 times and never used is the definition of ignored")
	}
	u.PrimedUses = 1
	if u.Ignored() || u.UseCount() != 1 {
		t.Fatalf("a primed use is still a use: %+v", u)
	}
	u.OrganicUses = 2
	if u.UseCount() != 3 {
		t.Fatalf("UseCount = %d, want 3", u.UseCount())
	}
	// Never pushed and never used is not "ignored": nothing put it in front of
	// anybody, so there is nothing for it to have been ignored by.
	if (state.MemoryUsage{}).Ignored() {
		t.Fatal("an untouched item must not read as ignored")
	}
}

func TestTotalUsageSumsPerInstanceRows(t *testing.T) {
	early := time.Unix(1, 0).UTC()
	late := time.Unix(2, 0).UTC()
	rows := []state.MemoryUsage{
		{MemoryID: "m", InstanceID: "a", PushCount: 2, OrganicUses: 1, LastPushedAt: early, LastUsedAt: late},
		{MemoryID: "m", InstanceID: "b", PushCount: 3, PrimedUses: 4, LastPushedAt: late, LastUsedAt: early},
	}
	got := state.TotalUsage(rows)
	if got.PushCount != 5 || got.OrganicUses != 1 || got.PrimedUses != 4 || got.UseCount() != 5 {
		t.Fatalf("total = %+v, want the counters summed", got)
	}
	if !got.LastPushedAt.Equal(late) || !got.LastUsedAt.Equal(late) {
		t.Fatalf("total timestamps = (%v, %v), want the latest of either instance", got.LastPushedAt, got.LastUsedAt)
	}
	// The sum belongs to no one instance, so it does not claim one.
	if got.InstanceID != "" {
		t.Fatalf("total is attributed to instance %q, want none", got.InstanceID)
	}
	if state.TotalUsage(nil) != (state.MemoryUsage{}) {
		t.Fatal("the total of no rows is the zero record")
	}
}

func TestMonocultureMeasuresOverlapBetweenInstances(t *testing.T) {
	push := func(instance string, ids ...string) []state.MemoryUsage {
		out := make([]state.MemoryUsage, 0, len(ids))
		for _, id := range ids {
			out = append(out, state.MemoryUsage{MemoryID: id, InstanceID: instance, PushCount: 1})
		}
		return out
	}
	cases := []struct {
		name string
		rows []state.MemoryUsage
		want float64
	}{
		{"no rows at all", nil, 0},
		// One instance has nothing to overlap with, and reporting 1 there would read
		// as total monoculture on the setup where the metric means nothing.
		{"a single instance", push("a", "1", "2"), 0},
		{"disjoint sets", append(push("a", "1"), push("b", "2")...), 0},
		{"identical sets", append(push("a", "1", "2"), push("b", "1", "2")...), 1},
		// {1,2} against {2,3}: one shared item over three distinct ones.
		{"half-shared sets", append(push("a", "1", "2"), push("b", "2", "3")...), 1.0 / 3.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := state.Monoculture(tc.rows); got != tc.want {
				t.Fatalf("Monoculture = %v, want %v", got, tc.want)
			}
		})
	}

	// A row with no push on it is not evidence of a shared digest, so it does not
	// count toward the overlap: two instances that each only ever recalled the same
	// item organically are not a monoculture the selection policy created.
	rows := []state.MemoryUsage{
		{MemoryID: "1", InstanceID: "a", OrganicUses: 1},
		{MemoryID: "1", InstanceID: "b", OrganicUses: 1},
	}
	if got := state.Monoculture(rows); got != 0 {
		t.Fatalf("Monoculture over uses with no pushes = %v, want 0", got)
	}
}

func TestSortUsageOrdersByItemThenInstance(t *testing.T) {
	rows := []state.MemoryUsage{
		{MemoryID: "b", InstanceID: "a"},
		{MemoryID: "a", InstanceID: "z"},
		{MemoryID: "a", InstanceID: "b"},
	}
	state.SortUsage(rows)
	want := []state.MemoryUsage{
		{MemoryID: "a", InstanceID: "b"},
		{MemoryID: "a", InstanceID: "z"},
		{MemoryID: "b", InstanceID: "a"},
	}
	for i := range rows {
		if rows[i] != want[i] {
			t.Fatalf("sorted[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// TestUsageSurvivesReplayAndSnapshot pins usage as ordinary event-sourced state:
// a provider folded from the log alone reports the same counters, and so does one
// restored from a snapshot rather than from the whole stream. A snapshot that
// dropped usage would look correct until the stream got long enough to check
// point, and then quietly reset every counter.
func TestUsageSurvivesReplayAndSnapshot(t *testing.T) {
	ctx := context.Background()
	for _, snapshotEvery := range []int{0, 1} {
		log := spine.NewMemoryLog()
		opts := []state.Option{state.WithEventLog(log)}
		if snapshotEvery > 0 {
			opts = append(opts, state.WithSnapshotEvery(snapshotEvery))
		}
		mem := state.NewMemory(opts...).Memory()

		a, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "one"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "two"})
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.RecordPush(ctx, []string{a.ID, b.ID}); err != nil {
			t.Fatal(err)
		}
		if err := mem.RecordUse(ctx, a.ID, state.UsagePrimed); err != nil {
			t.Fatal(err)
		}
		want, err := mem.Usage(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}

		replayed, err := state.Replay(ctx, log)
		if err != nil {
			t.Fatal(err)
		}
		got, err := replayed.Memory().Usage(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("snapshotEvery=%d: replayed %d usage rows, want %d", snapshotEvery, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("snapshotEvery=%d: replayed usage %+v, want %+v", snapshotEvery, got[i], want[i])
			}
		}
	}
}
