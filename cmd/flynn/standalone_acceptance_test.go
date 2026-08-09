package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/memory/consolidate"
	"github.com/ionalpha/flynn/memory/curate"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// Standalone acceptance. One pass per capability the boundary register calls
// `shipped`, driven the way the binary drives it, over a temp data directory with a
// scripted model and no host.
//
// The register and the drift guard are static: they check that a producer exists and
// is referenced. Neither proves the capability works from the binary, and the whole
// epic is written against the claim that it does. This is that claim, tested.
//
// The model is the one thing that stays scripted, because the point is the absence of
// a host and not the absence of a model. Everything else is real: the store is SQLite
// on disk, the sandbox is the local one with its confinement, every action crosses the
// dispatch waist, and the record is the run's own spine.
//
// A capability that turns out to need a host here is a finding for the register, not
// an exception to code around. Where a pass cannot be written, the register's
// `justified` row for that seam is being tested from the other side, and the reason
// has to already be written down.

// dataDir is a temp data directory with the durable store inside it, the way an
// install has one. On disk rather than in memory, so a pass proves the file the
// binary actually opens.
func dataDir(t *testing.T) (dir string, store *sqlite.Store) {
	t.Helper()
	dir = t.TempDir()
	store, err := openStore(context.Background(), filepath.Join(dir, "flynn.db"))
	if err != nil {
		t.Fatalf("open the store the binary opens: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return dir, store
}

// acceptCtx bounds a pass. Generous, because a pass runs a real sandbox.
func acceptCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Capability: a goal runs, acts through the sandbox, and converges.
//
// Register rows exercised: goal.StepExecutor, goal.StopEvaluator, goal.ProgressProbe,
// dispatch.Admitter, dispatch.EventSink, sandbox.Sandbox, spine.Log, state.Provider.
//
// The file is checked on disk rather than the summary taken at its word. A run that
// says it wrote something and did not is the failure a scripted model makes easiest
// to miss.
func TestStandaloneAGoalRunsThroughTheSandboxAndConverges(t *testing.T) {
	_, store := dataDir(t)
	work := t.TempDir()
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi from flynn"}`)),
		llmtest.SayText("Created hello.txt."),
	)
	var out bytes.Buffer

	result, err := runLearningMission(acceptCtx(t), &out, model, harness.Plan{}, nil,
		work, "create hello.txt with a greeting", "", store, nil, false, nil)
	if err != nil {
		t.Fatalf("the goal did not run standalone: %v", err)
	}
	if !strings.Contains(result, "hello.txt") {
		t.Fatalf("final result = %q, want the run's own summary", result)
	}
	if got := readWorkFile(t, work, "hello.txt"); got != "hi from flynn" {
		t.Fatalf("the sandboxed write did not land: %q", got)
	}
}

// Capability: a goal carrying a unit graph dispatches its units as governed children.
//
// Covered by TestFanoutAssemblyRunsAUnitGraph, which is this pass in everything but
// name: the shipped fan-out assembly, a real spawner, the dispatch waist, the sandbox
// the verify clause runs in, the evidence gate and the settlement, with only the model
// scripted. It is named here so the suite is a complete account of the register's
// shipped rows rather than a partial one that reads as complete.

// Capability: a goal that states terms of its run has them audited, and a breach
// stops it before its stop condition is consulted.
//
// Register rows exercised: goal.InvariantAuditor, evidence.CommandAuditor.
//
// It is driven through a shipped assembly rather than through runLearningMission,
// because the command surface has no way to state a term: nothing in cmd/flynn sets
// goal.Spec.Invariants. The auditor is wired on every assembly and the engine enforces
// the rule, so the capability is real and only the operator-facing surface is missing.
// That is a finding, recorded on the register's row, rather than something to paper
// over by testing the engine and calling it acceptance.
//
// The fan-out assembly, because it drives its own reconcile loop from Start; the
// single-conversation assembly is driven by the session the CLI opens around it, and
// a goal submitted straight to its runtime sits with its step in flight.
//
// The breach is the half worth insisting on. A run whose terms were never checked
// finishes looking exactly like one whose terms held, so the pass states a term whose
// check fails and asserts the goal stops on it.
func TestStandaloneAStatedTermIsAuditedAndABreachStopsTheRun(t *testing.T) {
	ctx := acceptCtx(t)
	_, store := dataDir(t)
	reg := mustRegistry(t)
	rstore := store.Resources(reg)

	run, err := assembleFanoutMission(alwaysDone{}, harness.Plan{}, t.TempDir(), defaultSystemPrompt,
		rstore, store.Jobs(), store.Log(), nil, "", nil, sandbox.ResourceLimits{}, approvalSetup{})
	if err != nil {
		t.Fatalf("assemble the mission the binary assembles: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	go func() { _ = run.rt.Start(ctx) }()

	if _, err := run.rt.SubmitGoal(ctx, "termed", goal.Spec{
		Objective:     "do the work",
		StopCondition: "the work is done",
		Grant:         []string{mission.ActionModelGenerate},
		Invariants:    []goal.Invariant{{ID: "clean-tree", Statement: "the tree stays clean", Check: "exit 1"}},
	}); err != nil {
		t.Fatalf("submit a goal that states its terms: %v", err)
	}

	status := waitForBreach(ctx, t, rstore, "termed")
	if status.Phase != goal.PhaseStalled {
		t.Fatalf("a breached term did not stop the goal: %+v", status)
	}
}

// waitForBreach polls until the goal records a breached invariant or the context ends.
func waitForBreach(ctx context.Context, t *testing.T, rstore resource.Store, name string) goal.Status {
	t.Helper()
	for {
		r, err := rstore.Get(ctx, goal.Kind, resource.Scope{}, name)
		if err == nil {
			st, derr := goal.DecodeStatus(r)
			if derr == nil {
				if _, breached := st.BreachedInvariant(); breached || st.Phase == goal.PhaseStalled {
					return st
				}
			}
		}
		select {
		case <-ctx.Done():
			r, _ := rstore.Get(context.Background(), goal.Kind, resource.Scope{}, name)
			st, _ := goal.DecodeStatus(r)
			t.Fatalf("the goal never recorded a verdict on its term: %+v", st)
			return goal.Status{}
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Capability: a converged run is distilled into a durable skill and a memory item,
// and the skill's own check is run in the sandbox before it is kept.
//
// Register rows exercised: learn.Distiller, learn.Verifier, memory/curate write
// policy, state.SkillStore, state.MemoryStore.
//
// A skill whose check fails is dropped rather than learned, so the pass offers two
// skills and expects one. Learning something broken is worse than learning nothing:
// the next run reaches for it.
func TestStandaloneAConvergedRunIsDistilledWithItsCheckRun(t *testing.T) {
	_, store := dataDir(t)
	work := t.TempDir()
	ctx := acceptCtx(t)
	distiller := &fakeDistiller{lessons: []learn.Lesson{
		{Kind: learn.LessonSkill, Title: "Sound procedure", Body: "Do the thing.", Check: "exit 0"},
		{Kind: learn.LessonSkill, Title: "Broken procedure", Body: "Do the other thing.", Check: "exit 1"},
		{Kind: learn.LessonMemory, Body: "the greeting file belongs at the tree root"},
	}}
	var out bytes.Buffer

	if _, err := runLearningMission(ctx, &out, llmtest.NewScripted(llmtest.SayText("done")),
		harness.Plan{}, distiller, work, "learn something", "", store, nil, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	skills, err := store.Skills().List(ctx, state.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, s := range skills {
		kept = append(kept, s.Slug)
	}
	if len(kept) != 1 || kept[0] != "sound-procedure" {
		t.Fatalf("skills kept = %v, want only the one whose check passed", kept)
	}
	if !hasTag(skills[0].Tags, "verified") {
		t.Errorf("the kept skill is not tagged verified, so its check did not run: %v", skills[0].Tags)
	}
	items, err := store.Memory().Recall(ctx, state.RecallQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Content, "greeting file") {
		t.Fatalf("memory = %+v, want the run's one lesson", items)
	}
}

// Capability: a session wakes with a digest of what the install already knows, and
// the push is counted so the decay policy has a signal to read.
//
// Register rows exercised: memory/digest.Builder, memory/digest.Pusher,
// memory/ridealong.Surfacer, memory/curate write policy.
//
// The counting is not incidental. Without it last-used-at degrades into a record of
// what the digest keeps putting in front of people, and the whole ranking rests on
// telling a memory that earns its place from one that is merely offered.
func TestStandaloneASessionWakesWithADigestAndCountsThePush(t *testing.T) {
	_, store := dataDir(t)
	ctx := wakeContext(acceptCtx(t))
	mem := newMemoryStack(store.Memory(), nil)

	written, err := mem.store.Write(ctx, state.MemoryItem{
		Kind: curate.KindPreference, Subject: "review-style",
		Content: "state the risk before the fix", Sources: []string{rememberSource},
	})
	if err != nil {
		t.Fatalf("pin a preference: %v", err)
	}

	system := withWake(ctx, mem, defaultSystemPrompt, nil)
	if !strings.Contains(system, "state the risk before the fix") {
		t.Fatalf("the wake digest did not reach the standing instructions:\n%s", system)
	}
	usage, err := mem.store.Usage(ctx, []string{written.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].PushCount != 1 {
		t.Fatalf("usage = %+v, want the push counted", usage)
	}
}

// Capability: repeated failures accumulate as a series, and a consolidation pass
// distils the series into one standing lesson with no host.
//
// Register rows exercised: memory/consolidate.Distiller, memory/distil.ModelDistiller,
// memory/curate write policy.
//
// It is driven through runConsolidation, which is the whole of `flynn memory
// consolidate` bar resolving a model and opening the database file, both of which the
// pass does itself.
func TestStandaloneASeriesConsolidatesIntoALesson(t *testing.T) {
	_, store := dataDir(t)
	ctx := acceptCtx(t)
	mem := newMemoryStack(store.Memory(), nil)

	for _, what := range []string{
		"the deploy failed: the migration ran before the schema lock",
		"the deploy failed again: same migration, same lock",
		"the deploy failed a third time on the migration",
	} {
		if _, err := mem.store.Write(ctx, state.MemoryItem{
			Kind: "episode", Subject: "deploy", Content: what, Sources: []string{"agent:run-1"},
		}); err != nil {
			t.Fatalf("write an episode: %v", err)
		}
	}

	var out bytes.Buffer
	if err := runConsolidation(ctx, store.Memory(), scriptedConsolidator{}, &out); err != nil {
		t.Fatalf("consolidate: %v", err)
	}

	items, err := store.Memory().Recall(ctx, state.RecallQuery{Subjects: []string{"deploy"}, Kinds: []string{"lesson"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("lessons on the subject = %d, want the series distilled into one: %s", len(items), out.String())
	}
	if !strings.Contains(items[0].Content, "schema lock") {
		t.Fatalf("the lesson does not carry what the series taught: %q", items[0].Content)
	}
}

// Capability: a run's record seals and verifies from the durable store alone.
//
// Register rows exercised: chain.RootSigner, chain.NodeStore, chain.CheckpointStore,
// spine.Log, spine.SnapshotCodec.
//
// Verification reads the store and nothing else, which is the property that makes the
// record worth having: a reader who was not there, and has no host to ask, can still
// check that the run happened as recorded.
func TestStandaloneARunSealsAndItsRecordVerifies(t *testing.T) {
	_, store := dataDir(t)
	work := t.TempDir()
	ctx := acceptCtx(t)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	// A self-certifying key id: the id is derived from the public key, so a verifier
	// checks the signature against the record itself rather than against a registry
	// somebody else keeps. That is what makes the record verifiable with no host.
	signer, err := chain.NewEd25519RootSigner(controlplane.PrincipalID(pub), priv)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := runLearningMission(ctx, &out, llmtest.NewScripted(llmtest.SayText("done")),
		harness.Plan{}, nil, work, "do a small thing", "", store, signer, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "run sealed") {
		t.Fatalf("the run did not seal:\n%s", out.String())
	}

	runID := sealedRunID(t, out.String())
	var verified bytes.Buffer
	if err := verifyStoredRun(ctx, &verified, store, runID); err != nil {
		t.Fatalf("the sealed record does not verify from the store alone: %v\n%s", err, verified.String())
	}
}

// readWorkFile reads a file the run wrote through the sandbox.
func readWorkFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

// sealedRunID pulls the run id out of the line the run prints when it seals, which is
// the same line an operator copies to verify it.
func sealedRunID(t *testing.T, progress string) string {
	t.Helper()
	const marker = "flynn spine verify "
	i := strings.Index(progress, marker)
	if i < 0 {
		t.Fatalf("no verify instruction in the run's own output:\n%s", progress)
	}
	id, _, _ := strings.Cut(progress[i+len(marker):], "\n")
	return strings.TrimSpace(id)
}

// scriptedConsolidator stands in for the model half of consolidation, so the pass
// exercises the sweep, the write semantics and the retirement rather than a model's
// prose. The register's `shipped` producer for this seam is memory/distil's model
// distiller, which storage/sqlite/distil_test.go drives end to end over a database.
type scriptedConsolidator struct{}

func (scriptedConsolidator) Distil(_ context.Context, in consolidate.Series) (consolidate.Lesson, error) {
	// An empty Content declines, which is the honest answer for a series with nothing
	// in it and the one a real distiller has to be able to give.
	if len(in.Episodes) == 0 {
		return consolidate.Lesson{}, nil
	}
	return consolidate.Lesson{
		Content: "deploys on " + in.Subject + " fail when the migration runs before the schema lock",
	}, nil
}
