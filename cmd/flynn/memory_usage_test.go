package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/memory/digest"
	"github.com/ionalpha/flynn/state"
)

// seedUsage writes an item, pushes it n times and records the given uses, which is
// the sequence the wake digest and the pull side produce between them.
func seedUsage(t *testing.T, mem state.MemoryStore, subject, content string, pushes int, uses ...state.UsageOrigin) state.MemoryItem {
	t.Helper()
	ctx := context.Background()
	item, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Subject: subject, Content: content, Sources: []string{"agent:run-1"}})
	if err != nil {
		t.Fatalf("write %q: %v", subject, err)
	}
	for range pushes {
		if err := mem.RecordPush(ctx, []string{item.ID}); err != nil {
			t.Fatalf("push %q: %v", subject, err)
		}
	}
	for _, origin := range uses {
		if err := mem.RecordUse(ctx, item.ID, origin); err != nil {
			t.Fatalf("use %q: %v", subject, err)
		}
	}
	return item
}

// TestMemoryUsageReportReadsBackWhatWasPushed runs the report over a store driven
// the way a run drives one, and holds the three things a curator opens it for: what
// each item's record is, which items the wake digest is already demoting, and the
// order that puts the ones a policy change would be about at the top.
func TestMemoryUsageReportReadsBackWhatWasPushed(t *testing.T) {
	ctx := context.Background()
	mem := state.NewMemory().Memory()
	seedUsage(t, mem, "deploy-api", "the deploy fails when the migration runs after it", digest.DefaultDemoteAfter)
	seedUsage(t, mem, "git-hooks", "the hooks run gofmt", 2, state.UsageOrganic, state.UsagePrimed)
	seedUsage(t, mem, "test-runner", "the suite needs a build tag", digest.DefaultDemoteAfter-1)
	seedUsage(t, mem, "recall-only", "the tag is behind a flag", 0, state.UsageOrganic)

	var out bytes.Buffer
	if err := renderMemoryUsage(ctx, &out, mem); err != nil {
		t.Fatalf("renderMemoryUsage: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"4 item(s) with a record, 1 demoted:",
		"[fact] deploy-api: pushed 5, never used  [demoted]",
		"the deploy fails when the migration runs after it",
		"[fact] test-runner: pushed 4, never used\n",
		"[fact] git-hooks: pushed 2, used 2 (1 organic, 1 primed)",
		// An item recall found and nothing ever pushed: the record says which half of
		// memory produced the use, so a corpus the digest never reaches is visible
		// rather than reading as an item with a poor return on its pushes.
		"[fact] recall-only: never pushed, used 1 (1 organic, 0 primed)",
		"monoculture: not measurable until a second instance has pushed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	// Demoted first, then ignored-but-under-threshold, then the items still earning
	// their place. A curator reading top-down meets the noise before the rest.
	i, j := strings.Index(got, "deploy-api"), strings.Index(got, "test-runner")
	k, l := strings.Index(got, "git-hooks"), strings.Index(got, "recall-only")
	if i > j || j > k || k > l {
		t.Errorf("order was deploy-api %d, test-runner %d, git-hooks %d, recall-only %d, want that order:\n%s", i, j, k, l, got)
	}
}

// A record whose item has been deleted is still reported. Usage outlives a tombstone
// so somebody can review what was pushed at readers before the item was retired, and
// a report that dropped those rows would hide exactly that.
func TestMemoryUsageReportKeepsRetiredItems(t *testing.T) {
	ctx := context.Background()
	mem := state.NewMemory().Memory()
	item := seedUsage(t, mem, "old-runbook", "restart the box", 3)
	if err := mem.Delete(ctx, item.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var out bytes.Buffer
	if err := renderMemoryUsage(ctx, &out, mem); err != nil {
		t.Fatalf("renderMemoryUsage: %v", err)
	}
	want := "[retired] " + item.ID + ": pushed 3, never used"
	if got := out.String(); !strings.Contains(got, want) {
		t.Fatalf("report is missing %q:\n%s", want, got)
	}
}

// An install nobody has read from says so, rather than printing an empty table and a
// monoculture number over no rows.
func TestMemoryUsageReportOnAnUntouchedStore(t *testing.T) {
	var out bytes.Buffer
	if err := renderMemoryUsage(context.Background(), &out, state.NewMemory().Memory()); err != nil {
		t.Fatalf("renderMemoryUsage: %v", err)
	}
	if got := out.String(); got != "nothing has been pushed at a reader or used yet\n" {
		t.Fatalf("report = %q, want the untouched-store line", got)
	}
}

// unreadableUsage is a store whose usage rows cannot be read.
type unreadableUsage struct {
	state.MemoryStore
	err error
}

func (u unreadableUsage) Usage(context.Context, []string) ([]state.MemoryUsage, error) {
	return nil, u.err
}

// Both reads the report takes are reported rather than rendered around. A report is
// the one place where a partial answer is worse than none: a curator reading "pushed
// and never used" off half the rows would act on a picture the store did not give.
func TestMemoryUsageReportFailsOnAnUnreadableStore(t *testing.T) {
	ctx := context.Background()
	want := errors.New("the usage table is gone")

	if err := renderMemoryUsage(ctx, &bytes.Buffer{}, unreadableUsage{MemoryStore: state.NewMemory().Memory(), err: want}); !errors.Is(err, want) {
		t.Errorf("unreadable usage = %v, want the read failure", err)
	}

	mem := state.NewMemory().Memory()
	seedUsage(t, mem, "deploy-api", "a fact with a record", 1)
	if err := renderMemoryUsage(ctx, &bytes.Buffer{}, failingMemory{MemoryStore: mem, err: want}); !errors.Is(err, want) {
		t.Errorf("unreadable recall = %v, want the read failure", err)
	}
}

// TestUsageLinesSumInstancesAndNameWhatTheyCannot covers the joins usageLines makes
// over rows one in-memory store cannot produce: several instances counting the same
// item, and an item with no subject to print.
func TestUsageLinesSumInstancesAndNameWhatTheyCannot(t *testing.T) {
	rows := []state.MemoryUsage{
		{MemoryID: "m1", InstanceID: "a", PushCount: 3, OrganicUses: 1},
		{MemoryID: "m1", InstanceID: "b", PushCount: 4, PrimedUses: 2},
	}
	items := []state.MemoryItem{{ID: "m1", Content: "a fact nobody gave a subject"}}

	lines := usageLines(rows, items)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want the two instances summed into one", len(lines))
	}
	l := lines[0]
	if l.total.PushCount != 7 || l.total.UseCount() != 3 {
		t.Errorf("total = %d pushes, %d uses, want 7 and 3", l.total.PushCount, l.total.UseCount())
	}
	// An item with neither kind nor subject still has to be identifiable, so the
	// defaults are the kind the store assumes and the id itself.
	if l.kind != "fact" || l.subject != "m1" {
		t.Errorf("label = [%s] %s, want [fact] m1", l.kind, l.subject)
	}
	if l.demoted {
		t.Error("an item with three uses is demoted, want a use to end it however often it was pushed")
	}
}

// TestUsageLinesOrderTiesTotally pins the order down to the last tiebreak, so two
// items with the same record print in the same order on every run.
func TestUsageLinesOrderTiesTotally(t *testing.T) {
	rows := []state.MemoryUsage{
		{MemoryID: "m2", InstanceID: "a", PushCount: 2, OrganicUses: 2},
		{MemoryID: "m1", InstanceID: "a", PushCount: 2, OrganicUses: 2},
		{MemoryID: "m3", InstanceID: "a", PushCount: 2, OrganicUses: 1},
	}
	var got []string
	for _, l := range usageLines(rows, nil) {
		got = append(got, l.total.MemoryID)
	}
	// m3 first: the same pushes bought fewer uses. Then the id breaks the exact tie.
	if want := []string{"m3", "m1", "m2"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestMonocultureLineNamesWhatCannotBeMeasured holds the one thing the metric must
// not do: report the 0 it returns for a single instance as a measured all-clear.
func TestMonocultureLineNamesWhatCannotBeMeasured(t *testing.T) {
	tests := []struct {
		name string
		rows []state.MemoryUsage
		want string
	}{
		{
			name: "one instance",
			rows: []state.MemoryUsage{{MemoryID: "m1", InstanceID: "a", PushCount: 1}},
			want: "not measurable until a second instance has pushed",
		},
		{
			name: "a second instance that has only used",
			rows: []state.MemoryUsage{
				{MemoryID: "m1", InstanceID: "a", PushCount: 1},
				{MemoryID: "m1", InstanceID: "b", OrganicUses: 1},
			},
			want: "not measurable until a second instance has pushed",
		},
		{
			name: "two instances pushing the same item",
			rows: []state.MemoryUsage{
				{MemoryID: "m1", InstanceID: "a", PushCount: 1},
				{MemoryID: "m1", InstanceID: "b", PushCount: 1},
			},
			want: "monoculture: 1.00 across 2 instances",
		},
		{
			name: "two instances sharing nothing",
			rows: []state.MemoryUsage{
				{MemoryID: "m1", InstanceID: "a", PushCount: 1},
				{MemoryID: "m2", InstanceID: "b", PushCount: 1},
			},
			want: "monoculture: 0.00 across 2 instances",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := monocultureLine(tt.rows); !strings.Contains(got, tt.want) {
				t.Fatalf("monocultureLine = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

// TestMemoryUsageCommandOverADataDir runs the subcommand as the CLI runs it, over a
// data directory it opens itself, and pins the routing: an argument the report does
// not take is the usage error rather than a silently ignored word.
func TestMemoryUsageCommandOverADataDir(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := dispatchMemory([]string{"usage"}, "", dir, &out); err != nil {
		t.Fatalf("flynn memory usage: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "nothing has been pushed") {
		t.Fatalf("output = %q, want the untouched-store line", got)
	}
	if err := dispatchMemory([]string{"usage", "--everything"}, "", dir, &out); !errors.Is(err, errMemoryUsage) {
		t.Fatalf("flynn memory usage --everything = %v, want the usage error", err)
	}
}

// A data directory the command cannot open is its failure to report. A report that
// swallowed the open and printed the untouched-store line would tell an operator
// their agent has pushed nothing, when what happened is that nobody looked.
func TestMemoryUsageCommandReportsAnUnusableDataDir(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var out bytes.Buffer
	if err := memoryUsageReport(nil, notADir, &out); err == nil {
		t.Fatalf("memoryUsageReport over a file = nil, want the open failure; output %q", out.String())
	}
}
