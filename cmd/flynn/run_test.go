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

	result, err := runLearningMission(ctx, &out, model, nil, dir, "create hello.txt with a greeting", memStore(t))
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

	if _, err := runLearningMission(ctx, &out, model, nil, dir, "try to escape", memStore(t)); err != nil {
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

	if block, _ := recallContext(ctx, st.Skills(), st.Memory(), "deploy the service"); block != "" {
		t.Fatalf("empty store should yield no recall block, got %q", block)
	}

	if _, err := st.Skills().Upsert(ctx, state.Skill{Slug: "deploy-flow", Name: "Deploy flow", Body: "run the deploy script then verify"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Memory().Write(ctx, state.MemoryItem{Kind: "lesson", Content: "the deploy target is fly.io"}); err != nil {
		t.Fatal(err)
	}

	block, _ := recallContext(ctx, st.Skills(), st.Memory(), "deploy the service")
	if !strings.Contains(block, "Deploy flow") || !strings.Contains(block, "fly.io") {
		t.Fatalf("recall block missing learned content:\n%s", block)
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
	if _, err := runLearningMission(ctx, &out, run1, distiller, dir, "set up the project", store); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Run 2: shares a keyword ("pnpm") with the stored memory, so recall injects it.
	run2 := llmtest.NewScripted(llmtest.SayText("installed deps"))
	if _, err := runLearningMission(ctx, &out, run2, nil, dir, "install deps with pnpm", store); err != nil {
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

	block, _ := recallContext(ctx, st.Skills(), st.Memory(), "deploy the docker service")
	iB, iA, iC := strings.Index(block, "Bravo"), strings.Index(block, "Alpha"), strings.Index(block, "Charlie")
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
	if _, err := runLearningMission(ctx, &out, model, rec, dir, "write x.txt", memStore(t)); err != nil {
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

// TestRunReinforcesRecalledSkill proves the outcome loop closes: a skill recalled
// into a run that converges earns a use and a win.
func TestRunReinforcesRecalledSkill(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer

	if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "docker-deploy", Name: "Docker deploy", Body: "how to deploy with docker"}); err != nil {
		t.Fatal(err)
	}
	model := llmtest.NewScripted(llmtest.SayText("done"))
	if _, err := runLearningMission(ctx, &out, model, &fakeDistiller{}, dir, "deploy with docker", store); err != nil {
		t.Fatalf("run: %v", err)
	}

	sk, err := store.Skills().Get(ctx, "docker-deploy")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Uses != 1 || sk.Wins != 1 {
		t.Fatalf("recalled skill evidence = (%d,%d), want (1,1)", sk.Uses, sk.Wins)
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
	if _, err := runLearningMission(ctx, &out, model, distiller, dir, "do the work", store); err != nil {
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
	if _, err := runLearningMission(ctx, &out, model, nil, workdir, "do the thing", store1); err != nil {
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
