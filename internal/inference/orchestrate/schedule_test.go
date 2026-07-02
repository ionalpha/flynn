package orchestrate

import (
	"reflect"
	"testing"
)

func TestScheduleLaunchesDesiredWithinBudget(t *testing.T) {
	p := Schedule(
		[]Desired{{ModelID: "a", Footprint: 40}, {ModelID: "b", Footprint: 40}},
		nil,
		100,
	)
	if !reflect.DeepEqual(p.Launch, []string{"a", "b"}) {
		t.Fatalf("Launch = %v, want [a b]", p.Launch)
	}
	if len(p.Evict) != 0 || len(p.Unschedulable) != 0 {
		t.Fatalf("unexpected evict/unschedulable: %+v", p)
	}
}

func TestScheduleEvictsSurplusNotDesired(t *testing.T) {
	// "c" is resident but no longer desired and is idle, so it is evicted.
	p := Schedule(
		[]Desired{{ModelID: "a", Footprint: 40}},
		[]Resident{{ModelID: "a", Footprint: 40}, {ModelID: "c", Footprint: 40}},
		100,
	)
	if len(p.Launch) != 0 {
		t.Fatalf("nothing to launch, got %v", p.Launch)
	}
	if !reflect.DeepEqual(p.Evict, []string{"c"}) {
		t.Fatalf("Evict = %v, want [c]", p.Evict)
	}
}

func TestSchedulePinnedNeverEvicted(t *testing.T) {
	// A pinned resident model is kept even when it is not desired and the budget is zero.
	p := Schedule(
		nil,
		[]Resident{{ModelID: "hot", Footprint: 1000, Pinned: true}},
		0,
	)
	if len(p.Evict) != 0 {
		t.Fatalf("a pinned model must never be evicted, got %v", p.Evict)
	}
}

func TestScheduleActiveNeverEvicted(t *testing.T) {
	// An actively decoding model is kept even when it is not desired, so in-flight work is
	// not dropped.
	p := Schedule(
		nil,
		[]Resident{{ModelID: "busy", Footprint: 1000, Active: true}},
		0,
	)
	if len(p.Evict) != 0 {
		t.Fatalf("an active model must never be evicted, got %v", p.Evict)
	}
}

func TestSchedulePriorityWinsUnderPressure(t *testing.T) {
	// Both want to be resident but only one fits; the higher priority is launched and the
	// other is reported unschedulable rather than dropped silently.
	p := Schedule(
		[]Desired{{ModelID: "low", Footprint: 80, Priority: 1}, {ModelID: "high", Footprint: 80, Priority: 9}},
		nil,
		100,
	)
	if !reflect.DeepEqual(p.Launch, []string{"high"}) {
		t.Fatalf("Launch = %v, want [high]", p.Launch)
	}
	if !reflect.DeepEqual(p.Unschedulable, []string{"low"}) {
		t.Fatalf("Unschedulable = %v, want [low]", p.Unschedulable)
	}
}

func TestScheduleEvictsToMakeRoomForHigherPriority(t *testing.T) {
	// A low-priority idle resident is evicted so a higher-priority desired model can be
	// launched in a budget that fits only one.
	p := Schedule(
		[]Desired{{ModelID: "high", Footprint: 80, Priority: 9}},
		[]Resident{{ModelID: "low", Footprint: 80}},
		100,
	)
	if !reflect.DeepEqual(p.Launch, []string{"high"}) {
		t.Fatalf("Launch = %v, want [high]", p.Launch)
	}
	if !reflect.DeepEqual(p.Evict, []string{"low"}) {
		t.Fatalf("Evict = %v, want [low]", p.Evict)
	}
}

func TestScheduleIdempotentWhenConverged(t *testing.T) {
	desired := []Desired{{ModelID: "a", Footprint: 40}, {ModelID: "b", Footprint: 40}}
	resident := []Resident{{ModelID: "a", Footprint: 40}, {ModelID: "b", Footprint: 40}}
	p := Schedule(desired, resident, 100)
	if len(p.Launch) != 0 || len(p.Evict) != 0 {
		t.Fatalf("a converged set must yield an empty plan, got %+v", p)
	}
}

func TestScheduleDesiredPinnedLaunchedDespiteBudget(t *testing.T) {
	// A pinned desired model must be launched even when the budget cannot fit it, because a
	// pin overrides the budget.
	p := Schedule(
		[]Desired{{ModelID: "pin", Footprint: 500, Pinned: true}},
		nil,
		100,
	)
	if !reflect.DeepEqual(p.Launch, []string{"pin"}) {
		t.Fatalf("Launch = %v, want [pin] (a pin overrides the budget)", p.Launch)
	}
	if len(p.Unschedulable) != 0 {
		t.Fatalf("a pinned model must not be unschedulable, got %v", p.Unschedulable)
	}
}

func TestScheduleDesiredPinnedBlocksLowerPriorityUnderPressure(t *testing.T) {
	// The pinned model consumes the budget, so a non-pinned desired model that no longer
	// fits is unschedulable.
	p := Schedule(
		[]Desired{{ModelID: "pin", Footprint: 90, Pinned: true}, {ModelID: "other", Footprint: 90, Priority: 9}},
		nil,
		100,
	)
	if !reflect.DeepEqual(p.Launch, []string{"pin"}) {
		t.Fatalf("Launch = %v, want [pin]", p.Launch)
	}
	if !reflect.DeepEqual(p.Unschedulable, []string{"other"}) {
		t.Fatalf("Unschedulable = %v, want [other]", p.Unschedulable)
	}
}

func TestScheduleDeduplicatesDuplicateDesired(t *testing.T) {
	// A malformed desired list that names the same model twice must not double-count it or
	// report it as unschedulable twice.
	p := Schedule(
		[]Desired{{ModelID: "a", Footprint: 200}, {ModelID: "a", Footprint: 200}},
		nil,
		100,
	)
	if !reflect.DeepEqual(p.Unschedulable, []string{"a"}) {
		t.Fatalf("Unschedulable = %v, want a single [a]", p.Unschedulable)
	}
	if len(p.Launch) != 0 {
		t.Fatalf("Launch = %v, want none", p.Launch)
	}
}

func TestScheduleKeepsActiveOverBudgetWithoutLaunching(t *testing.T) {
	// Forced (active) residents already exceed the budget, so nothing new is launched, but
	// the active model is not evicted either.
	p := Schedule(
		[]Desired{{ModelID: "want", Footprint: 10, Priority: 9}},
		[]Resident{{ModelID: "busy", Footprint: 200, Active: true}},
		100,
	)
	if len(p.Evict) != 0 {
		t.Fatalf("active model must not be evicted, got %v", p.Evict)
	}
	if len(p.Launch) != 0 || !reflect.DeepEqual(p.Unschedulable, []string{"want"}) {
		t.Fatalf("over-budget forced load must block new launches, got %+v", p)
	}
}
