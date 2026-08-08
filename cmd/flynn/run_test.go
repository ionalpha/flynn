package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// memStore opens an ephemeral in-memory durable store for a test. The same handle
// persists across calls within the test, so two runs over it share state.
func memStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// fakeDistiller returns fixed lessons, so capture is deterministic without a model.
type fakeDistiller struct{ lessons []learn.Lesson }

func (f *fakeDistiller) Distill(context.Context, learn.Outcome) ([]learn.Lesson, error) {
	return f.lessons, nil
}

// TestRunWritesFileThroughSandbox is the full-binary proof: the run assembles the
// real runtime, sandbox, and toolset over a durable store, and a scripted model
// drives a goal that writes a file through the sandboxed write tool, then converges
// with a summary. No network: the model is a fake; no capture: distiller is nil.
func TestRunWritesFileThroughSandbox(t *testing.T) {
	dir := t.TempDir()
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi from flynn"}`)),
		llmtest.SayText("Created hello.txt with a greeting."),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer

	result, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, dir, "create hello.txt with a greeting", "", memStore(t), nil, false, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(b) != "hi from flynn" {
		t.Fatalf("file not written through the sandbox: err=%v content=%q", err, b)
	}
	if !strings.Contains(result, "Created hello.txt") {
		t.Fatalf("final result = %q", result)
	}
	if !strings.Contains(out.String(), "write") {
		t.Fatalf("progress did not show the tool action:\n%s", out.String())
	}
}

// TestGoalCommandPlansBeforeItBuilds is the F1a composition proof: the goal path, with
// planning turned on (as the `goal` command turns it on), expands its objective into a
// visible ledger on the goal before it dispatches the first build step. A scripted model
// plans one item, then builds; afterwards the goal carries that item with its verify
// clause, and the very first model call was the planning call, not the build.
func TestGoalCommandPlansBeforeItBuilds(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	model := llmtest.NewScripted(
		llmtest.SayText(`[{"item":"write hello.txt","verify":"the file exists with the greeting"}]`),
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi"}`)),
		llmtest.SayText("done"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out bytes.Buffer

	result, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, dir, "create hello.txt", "", store, nil, false, nil, withPlanning())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(result, "done") {
		t.Fatalf("run did not converge on the build: %q", result)
	}
	// The build ran after planning: the file the build step wrote is there.
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatalf("build step did not run: %v", err)
	}
	// The first model call was the planner, not the build loop.
	if sys := model.Requests()[0].System; !strings.Contains(sys, "planning phase") {
		t.Fatalf("first model call was not the planner: %q", sys)
	}
	// A visible ledger was recorded on the goal, item and verify clause both.
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	goals, err := store.Resources(reg).List(ctx, goal.Kind, resource.Scope{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 1 {
		t.Fatalf("got %d goals, want 1", len(goals))
	}
	spec, err := goal.DecodeSpec(goals[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Ledger) != 1 {
		t.Fatalf("goal ledger has %d items, want 1 (the plan)", len(spec.Ledger))
	}
	if spec.Ledger[0].Item != "write hello.txt" || spec.Ledger[0].Verify == "" {
		t.Fatalf("ledger item = %+v, want the planned item with its verify clause", spec.Ledger[0])
	}
}

// TestRunRejectsSandboxEscape confirms the wired path is confined: a tool call that
// tries to write outside the working directory is denied end to end.
func TestRunRejectsSandboxEscape(t *testing.T) {
	dir := t.TempDir()
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"../escape.txt","content":"nope"}`)),
		llmtest.SayText("done"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer

	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, dir, "try to escape", "", memStore(t), nil, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("a tool wrote outside the sandbox working directory")
	}
}

// TestRecallContext checks the recall block: it surfaces stored skills and memory
// that share a keyword with the objective, and is empty when nothing is on file.
func TestRecallContext(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()

	if block, _, _ := recallContext(ctx, st.Skills(), st.Memory(), "deploy the service"); block != "" {
		t.Fatalf("empty store should yield no recall block, got %q", block)
	}

	if _, err := st.Skills().Upsert(ctx, state.Skill{Slug: "deploy-flow", Name: "Deploy flow", Body: "run the deploy script then verify"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Memory().Write(ctx, state.MemoryItem{Kind: "lesson", Content: "the deploy target is fly.io"}); err != nil {
		t.Fatal(err)
	}

	block, _, _ := recallContext(ctx, st.Skills(), st.Memory(), "deploy the service")
	// The skill is offered under its slug, which is the name skill_read resolves.
	if !strings.Contains(block, "deploy-flow") || !strings.Contains(block, "fly.io") {
		t.Fatalf("recall block missing learned content:\n%s", block)
	}
}

// TestRecallReturnsIDsForReinforcement pins what recall hands to the reinforcement
// path. Recall is scope-blind, so a bundled skill and a learned one can hold the
// same slug; a slug would then credit the run to whichever record the store
// resolves to, which is the older of the two. Ids name the record the model was
// actually shown.
func TestRecallReturnsIDsForReinforcement(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()

	bundled, err := st.Skills().Upsert(ctx, state.Skill{
		Slug: "deploy-flow", Name: "Deploy flow",
		Body: "run the deploy script then verify", Scope: state.BundledScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	learned, err := st.Skills().Upsert(ctx, state.Skill{
		Slug: "deploy-flow", Name: "Deploy flow", Tags: []string{"learned"},
		Body: "run the deploy script then verify", Scope: state.Scope{Instance: "inst"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, recalled, _ := recallContext(ctx, st.Skills(), st.Memory(), "deploy the service")
	if len(recalled) != 2 {
		t.Fatalf("recalled %d skills, want both records: %v", len(recalled), recalled)
	}
	for _, ref := range recalled {
		if ref != bundled.ID && ref != learned.ID {
			t.Fatalf("recalled %q, want a skill id; a slug cannot name one of two records", ref)
		}
	}

	// Reinforcement resolves those references exactly: both records take the credit
	// they earned, which is impossible when the reference is the slug they share.
	if err := learn.Reinforce(ctx, st.Skills(), recalled, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []state.Skill{bundled, learned} {
		got, err := st.Skills().Get(ctx, want.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Reads != 1 || got.Wins != 1 {
			t.Fatalf("skill %s in scope %+v: reads/wins = %d/%d, want 1/1", got.ID, got.Scope, got.Reads, got.Wins)
		}
	}
}

// TestRunRemembersAcrossRuns is the end-to-end proof of the learning loop: a first
// run captures a memory into the durable store, and a second run over the same
// store recalls it into the model's system prompt. The agent starts the second run
// already knowing what the first one learned.
func TestRunRemembersAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer

	// Run 1: converges, and the (fake) distiller crystallizes a memory.
	run1 := llmtest.NewScripted(llmtest.SayText("set up the project"))
	distiller := &fakeDistiller{lessons: []learn.Lesson{
		{Kind: learn.LessonMemory, Body: "the project uses pnpm for installs"},
	}}
	if _, err := runLearningMission(ctx, &out, run1, harness.Plan{}, distiller, dir, "set up the project", "", store, nil, false, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Run 2: shares a keyword ("pnpm") with the stored memory, so recall injects it.
	run2 := llmtest.NewScripted(llmtest.SayText("installed deps"))
	if _, err := runLearningMission(ctx, &out, run2, harness.Plan{}, nil, dir, "install deps with pnpm", "", store, nil, false, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	reqs := run2.Requests()
	if len(reqs) == 0 {
		t.Fatal("run 2 never called the model")
	}
	if !strings.Contains(reqs[0].System, "pnpm for installs") {
		t.Fatalf("run 2 did not recall run 1's memory into its prompt; system =\n%s", reqs[0].System)
	}
}

// TestRecallRanksByRelevanceAndVerification checks that recall orders hits by how
// many objective keywords they carry, with verified skills boosted above equally
// relevant unverified ones.
func TestRecallRanksByRelevanceAndVerification(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	mk := func(slug, name, body string, tags ...string) {
		if _, err := st.Skills().Upsert(ctx, state.Skill{Slug: slug, Name: name, Body: body, Tags: tags}); err != nil {
			t.Fatal(err)
		}
	}
	mk("alpha", "Alpha", "deploy the docker image")             // matches deploy+docker = 2
	mk("bravo", "Bravo", "deploy the docker image", "verified") // 2 + verified boost = 3
	mk("charlie", "Charlie", "notes about the service")         // matches service = 1

	block, _, _ := recallContext(ctx, st.Skills(), st.Memory(), "deploy the docker service")
	iB, iA, iC := strings.Index(block, "bravo"), strings.Index(block, "alpha"), strings.Index(block, "charlie")
	if iB < 0 || iA < 0 || iC < 0 {
		t.Fatalf("recall block missing entries:\n%s", block)
	}
	if iB >= iA || iA >= iC {
		t.Fatalf("recall not ranked (want Bravo<Alpha<Charlie): B=%d A=%d C=%d\n%s", iB, iA, iC, block)
	}
}

// recordingDistiller captures the Outcome it was handed, so a test can assert what
// the run fed it.
type recordingDistiller struct{ got learn.Outcome }

func (r *recordingDistiller) Distill(_ context.Context, o learn.Outcome) ([]learn.Lesson, error) {
	r.got = o
	return nil, nil
}

// TestRunFeedsTranscriptToDistiller proves the distiller learns from how the goal
// was reached, not just the final summary: the captured outcome carries the
// conversation transcript including the tool the agent called.
func TestRunFeedsTranscriptToDistiller(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer

	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"x.txt","content":"hi"}`)),
		llmtest.SayText("wrote x.txt"),
	)
	rec := &recordingDistiller{}
	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, rec, dir, "write x.txt", "", memStore(t), nil, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	if rec.got.Objective != "write x.txt" || rec.got.Result != "wrote x.txt" || !rec.got.Converged {
		t.Fatalf("outcome metadata = %+v", rec.got)
	}
	var sawTool, sawText bool
	for _, m := range rec.got.Transcript {
		if m.TextContent() == "wrote x.txt" {
			sawText = true
		}
		for _, tu := range m.ToolUses() {
			if tu.Name == "write" {
				sawTool = true
			}
		}
	}
	if !sawTool || !sawText {
		t.Fatalf("transcript missing the run's steps: sawTool=%v sawText=%v (%d msgs)", sawTool, sawText, len(rec.got.Transcript))
	}
}

// TestRegradeOverDurableStore proves a skill's check persists through SQLite and
// that re-grading re-confirms a still-passing skill and retires a now-failing one.
func TestRegradeOverDurableStore(t *testing.T) {
	store := memStore(t)
	ctx := context.Background()
	if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "keep", Body: "x", Check: "exit 0", Tags: []string{"unverified"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "drop", Body: "x", Check: "exit 1"}); err != nil {
		t.Fatal(err)
	}

	v := learn.NewSandboxVerifier(func(context.Context) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(t.TempDir())
	})
	res, err := learn.Regrade(ctx, store.Skills(), state.Scope{}, v)
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 2 || len(res.Reconfirmed) != 1 || len(res.Retired) != 1 {
		t.Fatalf("regrade = %+v, want 2/1/1", res)
	}
	keep, err := store.Skills().Get(ctx, "keep")
	if err != nil || keep.Check != "exit 0" {
		t.Fatalf("kept skill = %+v, %v (check should persist)", keep, err)
	}
	if _, err := store.Skills().Get(ctx, "drop"); err == nil {
		t.Fatal("the now-failing skill should have been retired")
	}
}

// TestRunCreditsTheOutcomeToWhatItRead is the whole point of separating the two
// counters. Both skills are recalled into the run, so both are offered; the model
// loads one of them and the run converges. The read one takes the win. The other
// takes nothing, because appearing in a prompt is not evidence that a skill helped,
// and crediting it would make the win rate a fact about the objective's keywords.
func TestRunCreditsTheOutcomeToWhatItRead(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer

	for _, sk := range []state.Skill{
		{Slug: "docker-deploy", Name: "Docker deploy", Body: "how to deploy with docker"},
		{Slug: "docker-registry", Name: "Docker registry", Body: "how to push to a docker registry"},
	} {
		if _, err := store.Skills().Upsert(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "skill_read", json.RawMessage(`{"skill":"docker-deploy"}`)),
		llmtest.SayText("done"),
	)
	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, &fakeDistiller{}, dir, "deploy with docker", "", store, nil, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	read, err := store.Skills().Get(ctx, "docker-deploy")
	if err != nil {
		t.Fatal(err)
	}
	if read.Offers != 1 || read.Reads != 1 || read.Wins != 1 {
		t.Fatalf("the skill the run read = (offers %d, reads %d, wins %d), want (1,1,1)", read.Offers, read.Reads, read.Wins)
	}
	ignored, err := store.Skills().Get(ctx, "docker-registry")
	if err != nil {
		t.Fatal(err)
	}
	if ignored.Offers != 1 {
		t.Fatalf("the skill the run ignored has %d offers, want 1: it was in the prompt", ignored.Offers)
	}
	if ignored.Reads != 0 || ignored.Wins != 0 {
		t.Fatalf("the skill the run ignored = (reads %d, wins %d), want (0,0): it was never loaded", ignored.Reads, ignored.Wins)
	}
}

// TestRunVerifiesCapturedSkill proves the wired path execution-verifies a captured
// skill in the sandbox: a skill whose check fails is dropped (never stored), while
// one whose check passes is kept and tagged verified.
func TestRunVerifiesCapturedSkill(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer

	model := llmtest.NewScripted(llmtest.SayText("did the work"))
	distiller := &fakeDistiller{lessons: []learn.Lesson{
		{Kind: learn.LessonSkill, Title: "Broken skill", Body: "does not work", Check: "exit 1"},
		{Kind: learn.LessonSkill, Title: "Good skill", Body: "works", Check: "exit 0"},
	}}
	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, distiller, dir, "do the work", "", store, nil, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, err := store.Skills().Get(ctx, "broken-skill"); err == nil {
		t.Fatal("a skill whose check failed was crystallized; it should have been dropped")
	}
	good, err := store.Skills().Get(ctx, "good-skill")
	if err != nil {
		t.Fatalf("the verified skill was not stored: %v", err)
	}
	var verified bool
	for _, tag := range good.Tags {
		if tag == "verified" {
			verified = true
		}
	}
	if !verified {
		t.Fatalf("the passing skill is not tagged verified: %v", good.Tags)
	}
}

// runIDFromOutput extracts the run id the binary prints ("  run <id>"), the
// user-facing handle a later replay or audit addresses the run by.
func runIDFromOutput(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "run "); ok {
			return id
		}
	}
	t.Fatalf("no run id in output:\n%s", out)
	return ""
}

// TestRunSpineIsDurableAndAddressable proves the identity keystone end to end and
// across a process boundary: a run records its conversation on a file-backed spine
// under a stable id, and after the store is closed and reopened that id still
// addresses the run's event stream and names its goal resource. One value
// identifies the whole run, and it survives the process.
func TestRunSpineIsDurableAndAddressable(t *testing.T) {
	workdir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "flynn.db")
	model := llmtest.NewScripted(llmtest.SayText("done"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out bytes.Buffer

	// Run a goal over a file-backed store, then close it: the process is "gone".
	store1, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, workdir, "do the thing", "", store1, nil, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	runID := runIDFromOutput(t, out.String())
	if err := store1.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Reopen the same database file: the run must still be addressable by its id.
	store2, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store2.Close() }()

	evs, err := store2.Log().Read(ctx, spine.Query{Stream: runID})
	if err != nil {
		t.Fatalf("read run spine %q after reopen: %v", runID, err)
	}
	if len(evs) == 0 {
		t.Fatalf("run %s left no events on the durable spine after reopen", runID)
	}
	if evs[0].Type != string(session.KindSessionStarted) {
		t.Fatalf("first spine event = %q, want %q", evs[0].Type, session.KindSessionStarted)
	}
	var converged bool
	for _, e := range evs {
		if e.Type == string(session.KindConverged) {
			converged = true
		}
	}
	if !converged {
		t.Fatalf("run %s spine has no %q event after reopen", runID, session.KindConverged)
	}

	// The same id names the run's goal resource on the reopened store.
	if _, err := store2.Resources(mustRegistry(t)).Get(ctx, "Goal", resource.Scope{}, runID); err != nil {
		t.Fatalf("run id %q does not name a goal resource after reopen: %v", runID, err)
	}
}

// mustRegistry builds the mission resource registry or fails the test.
func mustRegistry(t *testing.T) *resource.Registry {
	t.Helper()
	reg, err := missionRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}
