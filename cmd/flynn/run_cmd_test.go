package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// runOneMission drives a single converging run over the durable store under dataDir and
// returns its run id, so the read commands (runs, inspect) have a real run to read.
func runOneMission(t *testing.T, dataDir, objective string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"note.txt","content":"hi"}`)),
		llmtest.SayText("wrote note.txt"),
	)
	var out bytes.Buffer
	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, t.TempDir(),
		objective, "", store, nil, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	return runIDFromOutput(t, out.String())
}

// TestListRunsShowsWhatRan: `flynn runs` reports nothing on a fresh data dir, and after
// a run it names the run by the id the run printed, with its phase, step count, and
// objective, which is what makes a run findable to inspect or resume.
func TestListRunsShowsWhatRan(t *testing.T) {
	dataDir := t.TempDir()

	var empty bytes.Buffer
	if err := listRuns(&empty, dataDir); err != nil {
		t.Fatalf("runs: %v", err)
	}
	if !strings.Contains(empty.String(), "no runs yet") {
		t.Fatalf("a fresh data dir listed %q", empty.String())
	}

	runID := runOneMission(t, dataDir, "write a note about the deployment")

	var out bytes.Buffer
	if err := listRuns(&out, dataDir); err != nil {
		t.Fatalf("runs: %v", err)
	}
	got := out.String()
	for _, want := range []string{runID, "write a note about the deployment", "step "} {
		if !strings.Contains(got, want) {
			t.Fatalf("run listing missing %q:\n%s", want, got)
		}
	}
}

// TestInspectRunReplaysTheRecordedRun: inspect renders a past run's events from the
// durable spine, shows the tool detail only when asked, and refuses an id that names no
// run rather than printing an empty replay.
func TestInspectRunReplaysTheRecordedRun(t *testing.T) {
	dataDir := t.TempDir()
	runID := runOneMission(t, dataDir, "write a note")

	var plain bytes.Buffer
	if err := inspectRun(&plain, dataDir, runID, false); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !strings.Contains(plain.String(), "write") {
		t.Fatalf("the replay does not show the run's tool call:\n%s", plain.String())
	}

	var verbose bytes.Buffer
	if err := inspectRun(&verbose, dataDir, runID, true); err != nil {
		t.Fatalf("inspect --verbose: %v", err)
	}
	// The verbose replay carries the tool's arguments, which the default view elides.
	if !strings.Contains(verbose.String(), "note.txt") {
		t.Fatalf("the verbose replay does not show the tool arguments:\n%s", verbose.String())
	}
	if len(verbose.String()) <= len(plain.String()) {
		t.Fatal("the verbose replay showed no more than the default one")
	}

	err := inspectRun(&plain, dataDir, "run/absent", false)
	if err == nil || !strings.Contains(err.Error(), "run/absent") {
		t.Fatalf("inspecting an unknown run = %v, want a refusal naming it", err)
	}
}

// TestRegradeSkillsReportsTheTally: regrade re-runs every stored skill's check and
// reports what it reconfirmed and what it retired, which is the command's whole output.
func TestRegradeSkillsReportsTheTally(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "keeps-passing", Body: "x", Check: "exit 0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "now-broken", Body: "x", Check: "exit 1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := regradeSkills(&out, dataDir); err != nil {
		t.Fatalf("regrade: %v", err)
	}
	if !strings.Contains(out.String(), "regrade: 2 checked, 1 reconfirmed, 1 retired") {
		t.Fatalf("regrade tally = %q", out.String())
	}
}

// TestDriveOptionsApplyToTheRunConfig: each lever a run is driven with lands on the
// config the assembly reads, and a budget with no ceiling on either axis is not a
// budget at all (so an uncapped run opens no pool).
func TestDriveOptionsApplyToTheRunConfig(t *testing.T) {
	var cfg driveConfig
	if cfg.budgeted() {
		t.Fatal("the zero config claims a ceiling")
	}

	withBudget(budgetpkg.Limits{Tokens: 0, Cost: 0})(&cfg)
	if cfg.budgeted() {
		t.Fatal("a zero-limit budget is unlimited, not a ceiling")
	}
	withBudget(budgetpkg.Limits{Tokens: 5000})(&cfg)
	if !cfg.budgeted() || cfg.budget.Tokens != 5000 {
		t.Fatalf("token ceiling not applied: %+v", cfg.budget)
	}
	withBudget(budgetpkg.Limits{Cost: 1.5})(&cfg)
	if !cfg.budgeted() || cfg.budget.Cost != 1.5 {
		t.Fatalf("cost ceiling not applied: %+v", cfg.budget)
	}

	withResourceLimits(sandbox.ResourceLimits{MaxProcesses: 4})(&cfg)
	if cfg.resLimits.MaxProcesses != 4 {
		t.Fatalf("resource limits not applied: %+v", cfg.resLimits)
	}

	ea := &externAgent{model: "gpt-5-codex"}
	withExternalAgent(ea)(&cfg)
	if cfg.extAgent != ea {
		t.Fatal("the external agent backend was not applied")
	}
	if externalModel(cfg.extAgent) != "gpt-5-codex" {
		t.Fatalf("externalModel = %q", externalModel(cfg.extAgent))
	}
	if externalModel(nil) != "" {
		t.Fatal("a native run must name no external model")
	}

	ts := &boundToolset{}
	withToolset(ts)(&cfg)
	if cfg.toolset != ts {
		t.Fatal("the bound toolset was not applied")
	}

	var seen int
	withEventObserver(func(session.Event) { seen++ })(&cfg)
	cfg.observe(session.Event{})
	if seen != 1 {
		t.Fatal("the event observer was not applied")
	}
}

// TestDriveUnderABudgetOpensThePoolBeforeTheFirstAction: a run driven with a ceiling
// opens its spend pool keyed by the run id, so the limit is in force from the first
// action rather than after a race.
func TestDriveUnderABudgetOpensThePool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := memStore(t)
	reg := mustRegistry(t)

	model := llmtest.NewScripted(llmtest.SayText("done"))
	var out bytes.Buffer
	_, runID, _, err := drive(
		ctx, &out, model, harness.Plan{}, t.TempDir(),
		"do the thing", defaultSystemPrompt, store.Resources(reg), store.Jobs(), store.Log(), false, "", nil,
		withBudget(budgetpkg.Limits{Tokens: 100_000, Cost: 10}),
	)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if _, err := store.Resources(reg).Get(ctx, budgetpkg.Kind, resource.Scope{}, runID); err != nil {
		t.Fatalf("the run's spend pool was not opened under its id: %v", err)
	}
}

// TestDriveRefusesToResumeARunThatIsNotThere: resuming an id no goal was recorded under
// is an error, not a fresh run quietly started in its place.
func TestDriveRefusesToResumeARunThatIsNotThere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := memStore(t)
	reg := mustRegistry(t)

	model := llmtest.NewScripted(llmtest.SayText("done"))
	_, _, _, err := drive(ctx, &bytes.Buffer{}, model, harness.Plan{}, t.TempDir(),
		"", defaultSystemPrompt, store.Resources(reg), store.Jobs(), store.Log(), false, "run/absent", nil)
	if err == nil {
		t.Fatal("resuming a run that does not exist was accepted")
	}
}

// TestRunGroundTruthReportsAnUnrecordableCheck: the ground-truth verdict is recorded on
// the run's own stream, and when the log cannot take it the run says so rather than
// reporting a check that is not in the record.
func TestRunGroundTruthReportsAnUnrecordableCheck(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	recordGroundTruth(ctx, &out, refusingLog{}, "run/x", t.TempDir(), "exit 0")
	if !strings.Contains(out.String(), "ground-truth not recorded") {
		t.Fatalf("an unrecordable check was reported as %q", out.String())
	}

	// A check that cannot even run is recorded honestly as a failure, never as a pass.
	store := memStore(t)
	var ok, bad bytes.Buffer
	recordGroundTruth(ctx, &ok, store.Log(), "run/pass", t.TempDir(), "exit 0")
	recordGroundTruth(ctx, &bad, store.Log(), "run/fail", t.TempDir(), "exit 7")
	if !strings.Contains(ok.String(), "check passed") {
		t.Fatalf("a passing check reported %q", ok.String())
	}
	if !strings.Contains(bad.String(), "did not pass") {
		t.Fatalf("a failing check reported %q", bad.String())
	}

	events, err := store.Log().Read(ctx, spine.Query{Stream: "run/fail"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("a failing check recorded %d events, want the check and the outcome", len(events))
	}
}

// refusingLog is a spine.Log that accepts nothing, so a caller's handling of a log that
// cannot take an event is exercised.
type refusingLog struct{}

func (refusingLog) Append(context.Context, spine.AppendInput) (spine.Event, error) {
	return spine.Event{}, errRefusedAppend
}

func (refusingLog) Read(context.Context, spine.Query) ([]spine.Event, error) { return nil, nil }

func (refusingLog) SaveSnapshot(context.Context, spine.Snapshot) error { return errRefusedAppend }

func (refusingLog) LatestSnapshot(context.Context, string, int64) (spine.Snapshot, bool, error) {
	return spine.Snapshot{}, false, nil
}

var errRefusedAppend = errors.New("the log refused the event")

// TestGovernedDistillerRunsThroughTheWaist: the distiller's model call is dispatched
// like every other action, and a reply carrying no lessons captures nothing without
// failing the run.
func TestGovernedDistillerRunsThroughTheWaist(t *testing.T) {
	ctx := context.Background()
	model := llmtest.NewScripted(llmtest.SayText("nothing worth keeping here"))
	d := governedDistiller(model)
	if d == nil {
		t.Fatal("no distiller built")
	}
	lessons, err := d.Distill(ctx, learn.Outcome{Objective: "o", Result: "r", Converged: true})
	if err != nil {
		t.Fatalf("distill: %v", err)
	}
	if len(lessons) != 0 {
		t.Fatalf("a reply with no lessons captured %d", len(lessons))
	}
	if len(model.Requests()) == 0 {
		t.Fatal("the governed distiller never called the model")
	}
}

// TestChildModelResolverResolvesAHostedChild: a delegated child that names a hosted
// model resolves through the same credential chain the root model uses, and one that
// names nothing resolvable is refused rather than silently downgraded to the root's
// model.
func TestChildModelResolverResolvesAHostedChild(t *testing.T) {
	ctx := context.Background()
	dataDir := fileVaultEnv(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:1/v1")

	resolve := childModelResolver(ctx, dataDir)
	m, err := resolve("openai:gpt-test")
	if err != nil {
		t.Fatalf("resolving a hosted child model: %v", err)
	}
	if m == nil {
		t.Fatal("no model resolved for the child")
	}
	if _, err := resolve("not-a-provider:whatever"); err == nil {
		t.Fatal("a child naming an unresolvable model was accepted")
	}
}

// TestMissionRunCloseIsSafeOnAToolsetRun: a toolset run has no sandbox to release, and
// closing it (or a nil run) is still correct, because every caller closes what it owns.
func TestMissionRunCloseIsSafeOnAToolsetRun(t *testing.T) {
	var nilRun *missionRun
	if err := nilRun.Close(); err != nil {
		t.Fatalf("closing a nil run: %v", err)
	}
	var parts *missionParts
	if err := parts.Close(); err != nil {
		t.Fatalf("closing nil parts: %v", err)
	}
	if err := (&missionParts{}).Close(); err != nil {
		t.Fatalf("closing parts with no sandbox: %v", err)
	}
}

// TestAssembleExternalMissionWithholdsSpawn: an external harness owns its own loop, so
// the run it drives is assembled with the same sandbox, session, toolset, and governance
// recording as a native run, but without the spawn action: an external run cannot fan
// out. The model call and the tools are still in the grant, so every effect the harness
// makes still crosses the waist.
func TestAssembleExternalMissionWithholdsSpawn(t *testing.T) {
	store := memStore(t)
	reg := mustRegistry(t)
	ea := &externAgent{
		model:  "gpt-5-codex",
		driver: externagent.NewDriver(externagent.NewCodex("", nil), nil, t.TempDir()),
	}
	t.Cleanup(ea.close)

	run, err := assembleExternalMission(ea, t.TempDir(), defaultSystemPrompt,
		store.Resources(reg), store.Jobs(), store.Log(), store.Skills(), "", sandbox.ResourceLimits{})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })

	if run.sess == nil || run.rt == nil {
		t.Fatal("the external run was assembled without a session or a runtime")
	}
	grant := run.parts.grant
	if grant.Allows(mission.ActionSpawn) {
		t.Fatal("an external run was granted the spawn action; its harness owns the loop and must not fan out")
	}
	if !grant.Allows(mission.ActionModelGenerate) {
		t.Fatal("an external run cannot make a model call under its own grant")
	}
	if !grant.Allows("write") {
		t.Fatalf("the sandboxed toolset is not in the grant: %v", grant.Actions())
	}

	// A native fan-out run, by contrast, is granted the delegation it exists to make.
	fan, err := assembleFanoutMission(llmtest.NewScripted(llmtest.SayText("done")), harness.Plan{},
		t.TempDir(), defaultSystemPrompt, store.Resources(reg), store.Jobs(), store.Log(), store.Skills(), "",
		nil, sandbox.ResourceLimits{})
	if err != nil {
		t.Fatalf("assemble fan-out: %v", err)
	}
	t.Cleanup(func() { _ = fan.Close() })
	if !fan.parts.grant.Allows(mission.ActionSpawn) {
		t.Fatal("a fan-out run cannot delegate under its own grant")
	}
}

// TestRankMemoryOrdersByRelevanceThenRecency: an item carrying more of the objective's
// keywords ranks first, and two equally relevant items are ordered newest first.
func TestRankMemoryOrdersByRelevanceThenRecency(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	items := []state.MemoryItem{
		{ID: "old-relevant", Content: "the deploy target is fly.io", CreatedAt: older},
		{ID: "new-relevant", Content: "the deploy target is fly.io", CreatedAt: newer},
		{ID: "irrelevant", Content: "unrelated note", CreatedAt: newer},
	}
	ranked := rankMemory([]string{"deploy", "target"}, items)
	if len(ranked) != 3 {
		t.Fatalf("ranked %d items, want 3", len(ranked))
	}
	if ranked[0].ID != "new-relevant" || ranked[1].ID != "old-relevant" {
		t.Fatalf("ranking = %s, %s, %s; want the newer of the two relevant items first",
			ranked[0].ID, ranked[1].ID, ranked[2].ID)
	}
	if ranked[2].ID != "irrelevant" {
		t.Fatalf("an item carrying no keyword outranked one that does: %s", ranked[2].ID)
	}
	if len(rankMemory(nil, nil)) != 0 {
		t.Fatal("ranking nothing produced something")
	}
}

// TestRunLearningMissionSealsAndVerifiesItsOwnRun: with a signer, a converged run is
// sealed into a signed record on its own stream and the resource projection is
// snapshotted, so the run verifies from the durable store alone afterwards.
func TestRunLearningMissionSealsAndVerifiesItsOwnRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dataDir := t.TempDir()
	signer, _ := spineSelfSigner(t, 29)

	store, err := openDataStore(ctx, dataDir, snapshotOptions(signer)...)
	if err != nil {
		t.Fatal(err)
	}

	model := llmtest.NewScripted(llmtest.SayText("done"))
	var out bytes.Buffer
	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, t.TempDir(),
		"reply done and stop", "exit 0", store, signer, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "run sealed") {
		t.Fatalf("the run was not sealed:\n%s", got)
	}
	if !strings.Contains(got, "ground-truth check passed") {
		t.Fatalf("the independent check was not recorded:\n%s", got)
	}

	// The sealed run verifies from the reopened store, and its record is grounded: the
	// success is backed by the check the run could not have graded itself.
	runID := runIDFromOutput(t, got)
	if err := verifyRun(dataDir, runID); err != nil {
		t.Fatalf("the sealed run does not verify from the store: %v", err)
	}
}
