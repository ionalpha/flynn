package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
)

// TestKillRecordsTheOrderOnARunningGoal: the order is written to the durable record the run is
// reconciled from, which is what lets it stop a run this process is not driving. That is
// the case the command exists for: the run worth killing is the one going wrong in a
// terminal nobody is sitting at.
func TestKillRecordsTheOrderOnARunningGoal(t *testing.T) {
	dir := t.TempDir()
	spec, status := runningRun()
	seedRun(t, dir, "g1", spec, status)

	var out bytes.Buffer
	if err := killRun(&out, []string{"g1", "it is editing the wrong repository"}, dir); err != nil {
		t.Fatalf("kill: %v", err)
	}

	got := readRunSpec(t, dir, "g1")
	if got.Kill == nil {
		t.Fatal("no kill on the record")
	}
	if got.Kill.Reason != "it is editing the wrong repository" {
		t.Fatalf("reason = %q, want the words the operator typed", got.Kill.Reason)
	}
	// Nothing about what the run was asked to do is touched. A kill is not an amendment
	// any more than a redirect is: it stops the run, it does not restate the objective.
	if got.Objective != spec.Objective || got.StopCondition != spec.StopCondition {
		t.Fatalf("killing rewrote the goal: %+v", got)
	}
	if !strings.Contains(out.String(), "cannot be taken back") {
		t.Fatalf("the operator was not told the stop is final:\n%s", out.String())
	}
}

// TestKillWithNoReasonIsComplete: the order is the content and the reason is the courtesy,
// so a kill with nothing written in it is a whole kill. A redirect is the other way round,
// which is why it refuses an empty instruction and this does not.
func TestKillWithNoReasonIsComplete(t *testing.T) {
	dir := t.TempDir()
	spec, status := runningRun()
	seedRun(t, dir, "g1", spec, status)

	var out bytes.Buffer
	if err := killRun(&out, []string{"g1"}, dir); err != nil {
		t.Fatalf("kill: %v", err)
	}
	got := readRunSpec(t, dir, "g1")
	if got.Kill == nil || got.Kill.Reason != "" {
		t.Fatalf("kill = %+v, want the bare order", got.Kill)
	}
}

// TestKillRefusesARunThatHasAlreadyFinished: recording the order anyway would put a kill on
// the record of a run that finished before it arrived, and a reader afterwards has no way
// to tell that apart from a run somebody actually stopped.
func TestKillRefusesARunThatHasAlreadyFinished(t *testing.T) {
	for _, phase := range []goal.Phase{goal.PhaseConverged, goal.PhaseStalled} {
		t.Run(string(phase), func(t *testing.T) {
			dir := t.TempDir()
			spec, _ := runningRun()
			seedRun(t, dir, "g1", spec, goal.Status{Phase: phase, Steps: 3})

			var out bytes.Buffer
			err := killRun(&out, []string{"g1", "stop"}, dir)
			if err == nil {
				t.Fatal("a finished run was killed")
			}
			if !strings.Contains(err.Error(), "already finished") {
				t.Fatalf("error = %q, want it to say the run is over", err)
			}
			if got := readRunSpec(t, dir, "g1"); got.Kill != nil {
				t.Fatalf("the refused kill was recorded anyway: %+v", got.Kill)
			}
		})
	}
}

// TestKillingTwiceKeepsTheFirstAccount: the account of why a run was stopped belongs to
// whoever stopped it, so a second kill is refused rather than overwriting the first
// operator's reason with a second one.
func TestKillingTwiceKeepsTheFirstAccount(t *testing.T) {
	dir := t.TempDir()
	spec, status := runningRun()
	seedRun(t, dir, "g1", spec, status)

	var out bytes.Buffer
	if err := killRun(&out, []string{"g1", "wrong repository"}, dir); err != nil {
		t.Fatalf("kill: %v", err)
	}
	err := killRun(&out, []string{"g1", "some other reason"}, dir)
	if err == nil {
		t.Fatal("a second kill overwrote the first")
	}
	if !strings.Contains(err.Error(), "wrong repository") {
		t.Fatalf("error = %q, want the first operator's reason quoted back", err)
	}
	if got := readRunSpec(t, dir, "g1"); got.Kill.Reason != "wrong repository" {
		t.Fatalf("reason = %q, want the first one kept", got.Kill.Reason)
	}
}

// TestKillNeedsARunToStop: the run id is the whole argument, and a command that guessed one
// would stop something nobody named.
func TestKillNeedsARunToStop(t *testing.T) {
	var out bytes.Buffer
	err := killRun(&out, nil, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("error = %v, want a usage error", err)
	}
}

// TestKillRefusesARunThatIsNotThere: an unknown id is a typo, and a command that shrugged
// at one would let an operator believe they had stopped a run that is still going.
func TestKillRefusesARunThatIsNotThere(t *testing.T) {
	dir := t.TempDir()
	spec, status := runningRun()
	seedRun(t, dir, "g1", spec, status)

	var out bytes.Buffer
	if err := killRun(&out, []string{"g2", "stop"}, dir); err == nil {
		t.Fatal("a run that does not exist was killed")
	}
}

// --- the paths around a store this command does not control -----------------

// TestKillReportsWhatItCouldNotRead: every way the record can fail to be read is reported.
// An operator who is told nothing assumes the run stopped, and the run they meant to stop
// keeps going.
func TestKillReportsWhatItCouldNotRead(t *testing.T) {
	running := `{"phase":"Running"}`
	cases := []struct {
		name  string
		store *steerStubStore
	}{
		{"the run cannot be fetched", &steerStubStore{getErr: errors.New("the store is unavailable")}},
		{"its objective cannot be read", &steerStubStore{res: steerRes(`{`, running)}},
		{"its progress cannot be read", &steerStubStore{res: steerRes(`{"objective":"o"}`, `{`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyKill(context.Background(), tc.store, "id-1", "stop"); err == nil {
				t.Fatal("a run was killed on a record nobody could read")
			}
			if tc.store.puts != 0 {
				t.Fatalf("the failure still wrote to the run (%d puts)", tc.store.puts)
			}
		})
	}
}

// TestKillGivesUpWhenTheRunIsBeingWrittenFaster: the run's own reconciler writes to the same
// record every few seconds, so a lost race is ordinary and re-applied. Losing every attempt
// is reported as what it is, because a kill silently dropped is the one outcome an operator
// must never be left with.
func TestKillGivesUpWhenTheRunIsBeingWrittenFaster(t *testing.T) {
	store := &steerStubStore{
		res:    steerRes(`{"objective":"o"}`, `{"phase":"Running"}`),
		putErr: resource.ErrConflict,
	}

	err := applyKill(context.Background(), store, "id-1", "stop")
	if err == nil {
		t.Fatal("a kill that never landed was reported as applied")
	}
	if !errors.Is(err, resource.ErrConflict) {
		t.Fatalf("error %v does not carry what went wrong", err)
	}
	if store.puts != killRetries {
		t.Fatalf("attempts = %d, want %d", store.puts, killRetries)
	}
}

// TestKillStopsOnAWriteThatIsNotARace: a store that refused for any other reason will refuse
// again, and re-applying would turn one failure into three.
func TestKillStopsOnAWriteThatIsNotARace(t *testing.T) {
	store := &steerStubStore{
		res:    steerRes(`{"objective":"o"}`, `{"phase":"Running"}`),
		putErr: errors.New("the store is read-only"),
	}

	if err := applyKill(context.Background(), store, "id-1", "stop"); err == nil {
		t.Fatal("a store that refused the write reported success")
	}
	if store.puts != 1 {
		t.Fatalf("attempts = %d, want one: a refusal that is not a race is not retried", store.puts)
	}
}

// TestKillDispatchReachesTheCommand: the dispatch wrapper is the only thing between the CLI
// and killRun, so the one thing worth checking is that it passes what it was given.
func TestKillDispatchReachesTheCommand(t *testing.T) {
	dir := t.TempDir()
	spec, status := runningRun()
	seedRun(t, dir, "g1", spec, status)

	if err := dispatchKill([]string{"g1", "wrong repository"}, dir); err != nil {
		t.Fatalf("dispatchKill: %v", err)
	}
	got := readRunSpec(t, dir, "g1")
	if got.Kill == nil || got.Kill.Reason != "wrong repository" {
		t.Fatalf("kill = %+v, want the order the dispatcher was handed", got.Kill)
	}
}

// TestKillReportsAStoreItCannotOpen: the data directory is an operator argument, and a path
// that is not one is said out loud rather than read as a run there is nothing to stop.
func TestKillReportsAStoreItCannotOpen(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "flynn.db")
	if err := os.WriteFile(notADir, []byte("not a data directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := killRun(&out, []string{"g1", "stop"}, notADir); err == nil {
		t.Fatal("a data directory that is not one was read as a run to stop")
	}
}
