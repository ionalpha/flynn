package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
)

// seedRun writes one goal into the durable store under dataDir and closes it, so the
// command (which opens the store itself) sees a run another process left behind, which is
// the case this command exists for.
func seedRun(t *testing.T, dataDir, name string, spec goal.Spec, status goal.Status) {
	t.Helper()
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()
	rawSpec, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	rawStatus, err := status.Encode()
	if err != nil {
		t.Fatal(err)
	}
	rs := store.Resources(mustRegistry(t))
	if _, err := rs.Put(ctx, resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: name,
		Spec: rawSpec, Status: rawStatus,
	}); err != nil {
		t.Fatalf("put run: %v", err)
	}
}

// readRunSpec reads back what the command wrote.
func readRunSpec(t *testing.T, dataDir, name string) goal.Spec {
	t.Helper()
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()
	r, err := store.Resources(mustRegistry(t)).Get(ctx, goal.Kind, resource.Scope{}, name)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func runningRun() (goal.Spec, goal.Status) {
	return goal.Spec{Objective: "add the audit trail", StopCondition: "the trail is written"},
		goal.Status{Phase: goal.PhaseRunning, Steps: 3}
}

// TestSteerRecordsARedirectOnARunningGoal: the redirect lands on the durable record the
// run is reconciled from, which is what lets it reach a run this process is not driving.
func TestSteerRecordsARedirectOnARunningGoal(t *testing.T) {
	dir := t.TempDir()
	spec, status := runningRun()
	seedRun(t, dir, "g1", spec, status)

	var out bytes.Buffer
	if err := steerRun(&out, []string{"g1", "you are writing to sessions;", "write to events instead"}, dir); err != nil {
		t.Fatalf("steer: %v", err)
	}

	got := readRunSpec(t, dir, "g1")
	if len(got.Steers) != 1 {
		t.Fatalf("steers = %+v, want one", got.Steers)
	}
	if got.Steers[0].Instruction != "you are writing to sessions; write to events instead" {
		t.Fatalf("instruction = %q, want the words the operator typed", got.Steers[0].Instruction)
	}
	// The objective and the definition of done are untouched: that separation is the
	// difference between a redirect and an amendment.
	if got.Objective != spec.Objective || got.StopCondition != spec.StopCondition {
		t.Fatalf("steering rewrote the goal: %+v", got)
	}
	if !strings.Contains(out.String(), "cannot report success") {
		t.Fatalf("the operator was not told what a redirect does:\n%s", out.String())
	}
}

// TestSteerTwiceKeepsBothWithDistinctIDs: a redirect is never withdrawn, so correcting one
// means issuing another, and the two have to be separately answerable.
func TestSteerTwiceKeepsBothWithDistinctIDs(t *testing.T) {
	dir := t.TempDir()
	spec, status := runningRun()
	seedRun(t, dir, "g1", spec, status)

	var out bytes.Buffer
	for _, instruction := range []string{"write to events instead", "leave the migration alone"} {
		if err := steerRun(&out, []string{"g1", instruction}, dir); err != nil {
			t.Fatalf("steer %q: %v", instruction, err)
		}
	}

	got := readRunSpec(t, dir, "g1")
	if len(got.Steers) != 2 {
		t.Fatalf("steers = %+v, want both", got.Steers)
	}
	if got.Steers[0].ID == got.Steers[1].ID {
		t.Fatalf("two redirects share an id: %+v", got.Steers)
	}
	if err := goal.ValidateSteers(got.Steers); err != nil {
		t.Fatalf("the command wrote a set the reconciler would refuse: %v", err)
	}
}

// TestSteerRefusesARunThatHasFinished: appending here would hold a finished account
// against an instruction that did not exist when it was written, which stops the run for
// something nobody could have acted on.
func TestSteerRefusesARunThatHasFinished(t *testing.T) {
	for _, phase := range []goal.Phase{goal.PhaseConverged, goal.PhaseStalled} {
		t.Run(string(phase), func(t *testing.T) {
			dir := t.TempDir()
			spec, _ := runningRun()
			seedRun(t, dir, "g1", spec, goal.Status{Phase: phase})

			var out bytes.Buffer
			err := steerRun(&out, []string{"g1", "write to events instead"}, dir)
			if err == nil {
				t.Fatal("a finished run was redirected")
			}
			if !strings.Contains(err.Error(), "already finished") {
				t.Fatalf("error = %q, want it to say the run is over", err)
			}
			if got := readRunSpec(t, dir, "g1"); len(got.Steers) != 0 {
				t.Fatalf("the refusal still wrote a redirect: %+v", got.Steers)
			}
		})
	}
}

// steerStubStore serves one resource and answers every Put the same way, so the paths
// applySteer takes around a store it does not control are reachable without one.
type steerStubStore struct {
	resource.Store
	res    resource.Resource
	getErr error
	putErr error
	puts   int
}

func (s *steerStubStore) GetByID(context.Context, string) (resource.Resource, error) {
	return s.res, s.getErr
}

func (s *steerStubStore) Put(_ context.Context, r resource.Resource) (resource.Resource, error) {
	s.puts++
	return r, s.putErr
}

// steerRes is a goal resource carrying spec and status verbatim, including bytes no decoder
// would accept.
func steerRes(spec, status string) resource.Resource {
	return resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g1", ID: "id-1",
		Spec: json.RawMessage(spec), Status: json.RawMessage(status),
	}
}

// TestSteerReportsWhatItCouldNotRead: a redirect is a write against a record, and every way
// that record can fail to be read is reported rather than being applied to a guess about
// what it said.
func TestSteerReportsWhatItCouldNotRead(t *testing.T) {
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
			if _, err := applySteer(context.Background(), tc.store, "id-1", "write to events instead"); err == nil {
				t.Fatal("a redirect was applied to a record nobody could read")
			}
			if tc.store.puts != 0 {
				t.Fatalf("the failure still wrote to the run (%d puts)", tc.store.puts)
			}
		})
	}
}

// TestSteerRefusesToWriteASetTheRunCouldNotAnswer: the id is derived from how many redirects
// are already there, so a record whose redirects were numbered by something else can collide.
// Writing it would leave the reconciler refusing the run at admission for a set this command
// produced.
func TestSteerRefusesToWriteASetTheRunCouldNotAnswer(t *testing.T) {
	store := &steerStubStore{res: steerRes(`{"objective":"o","steers":[{"id":"steer-2","instruction":"leave the migration alone"}]}`, `{"phase":"Running"}`)}

	if _, err := applySteer(context.Background(), store, "id-1", "write to events instead"); err == nil {
		t.Fatal("a colliding redirect id was written")
	}
	if store.puts != 0 {
		t.Fatalf("the refusal still wrote to the run (%d puts)", store.puts)
	}
}

// TestSteerGivesUpWhenTheRunIsBeingWrittenFaster: the run's own reconciler writes to the
// same record every few seconds, so a conflict is ordinary and retried. Losing every attempt
// is reported as what it is rather than as a redirect that landed.
func TestSteerGivesUpWhenTheRunIsBeingWrittenFaster(t *testing.T) {
	store := &steerStubStore{
		res:    steerRes(`{"objective":"o"}`, `{"phase":"Running"}`),
		putErr: resource.ErrConflict,
	}

	_, err := applySteer(context.Background(), store, "id-1", "write to events instead")
	if err == nil {
		t.Fatal("a redirect that never landed was reported as applied")
	}
	if !errors.Is(err, resource.ErrConflict) {
		t.Fatalf("error %v does not carry what went wrong", err)
	}
	if store.puts != steerRetries {
		t.Fatalf("attempts = %d, want %d: a conflict here is ordinary and worth re-applying", store.puts, steerRetries)
	}
}

// TestSteerStopsOnAWriteThatIsNotARace: a store that refused for any other reason will refuse
// again, and re-applying would turn one failure into three.
func TestSteerStopsOnAWriteThatIsNotARace(t *testing.T) {
	store := &steerStubStore{
		res:    steerRes(`{"objective":"o"}`, `{"phase":"Running"}`),
		putErr: errors.New("the store is read-only"),
	}

	if _, err := applySteer(context.Background(), store, "id-1", "write to events instead"); err == nil {
		t.Fatal("a store that refused the write reported success")
	}
	if store.puts != 1 {
		t.Fatalf("attempts = %d, want one: a refusal that is not a race is not retried", store.puts)
	}
}

// TestSteerDispatchReachesTheCommand: the dispatch wrapper is the only thing between the CLI
// and steerRun, so the one thing worth checking is that it passes what it was given.
func TestSteerDispatchReachesTheCommand(t *testing.T) {
	dir := t.TempDir()
	spec, status := runningRun()
	seedRun(t, dir, "g1", spec, status)

	if err := dispatchSteer([]string{"g1", "write to events instead"}, dir); err != nil {
		t.Fatalf("dispatchSteer: %v", err)
	}
	if got := readRunSpec(t, dir, "g1"); len(got.Steers) != 1 {
		t.Fatalf("steers = %+v, want the redirect the dispatcher was handed", got.Steers)
	}
}

// TestSteerReportsAStoreItCannotOpen: the data directory is an operator argument, and a path
// that is not one is said out loud rather than read as a run with no redirects.
func TestSteerReportsAStoreItCannotOpen(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "flynn.db")
	if err := os.WriteFile(notADir, []byte("not a data directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := steerRun(&out, []string{"g1", "write to events instead"}, notADir); err == nil {
		t.Fatal("a data directory that is a file was opened")
	}
}

func TestSteerUsage(t *testing.T) {
	dir := t.TempDir()
	spec, status := runningRun()
	seedRun(t, dir, "g1", spec, status)

	var out bytes.Buffer
	if err := steerRun(&out, []string{"g1"}, dir); err == nil {
		t.Fatal("a redirect with nothing to say was accepted")
	}
	if err := steerRun(&out, []string{"g1", "   "}, dir); err == nil {
		t.Fatal("a blank redirect was accepted")
	}
	if err := steerRun(&out, []string{"no-such-run", "write to events instead"}, dir); err == nil {
		t.Fatal("a run that does not exist was redirected")
	}
}
