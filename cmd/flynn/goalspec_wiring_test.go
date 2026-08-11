package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/evidence"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
)

// What the operator wrote in the file has to end up on the goal the command submits.
// The loader and the drive option are each covered on their own; this is the join, and
// it is the join that decides whether a run is governed or merely reads as though it is.
//
// It asserts on the submitted goal rather than on a breach, because whether an audit
// lands before a converging conversation stops reconciling is a race. Adoption is not:
// the terms are recorded on the status at admission, before the first step, and the
// mark is the fingerprint the engine holds the run to for the rest of the run. The
// breach itself is asserted in TestStandaloneAStatedTermIsAuditedAndABreachStopsTheRun,
// over an assembly that keeps reconciling.
func TestGoalSpecTermsReachTheSubmittedGoal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := memStore(t)
	rstore := store.Resources(mustRegistry(t))
	spec := loadWrittenGoalSpec(t, `{
	  "stopCondition": "the client is upgraded and the suite passes",
	  "invariants": [
	    {"id": "public-api", "statement": "the exported API of ./client does not change", "check": "exit 0"},
	    {"id": "scope", "statement": "only files under ./client are modified", "check": "exit 0"}
	  ]
	}`)

	// The run's own outcome is not what this asserts, and it differs by host: where the
	// sandbox cannot contain semi-trusted work the checks are refused and the run stops
	// saying so, which is the fail-closed answer and is covered by
	// TestTheAuditorRunsAChecksCommandInTheRealSandbox. What was submitted is the same
	// either way, and it is settled at admission before any of that.
	var out bytes.Buffer
	_, runID, _, _ := drive(ctx, &out, llmtest.NewScripted(llmtest.SayText("done")), harness.Plan{},
		t.TempDir(), "upgrade the http client", defaultSystemPrompt,
		rstore, store.Jobs(), store.Log(), false, "", nil, withGoalSpec(spec))

	r, err := rstore.Get(ctx, goal.Kind, resource.Scope{}, runID)
	if err != nil {
		t.Fatalf("get the goal the run submitted: %v", err)
	}
	submitted, err := goal.DecodeSpec(r)
	if err != nil {
		t.Fatalf("decode the submitted spec: %v", err)
	}

	if len(submitted.Invariants) != 2 {
		t.Fatalf("the file's terms did not reach the goal: %+v", submitted.Invariants)
	}
	if submitted.Invariants[0].ID != "public-api" || submitted.Invariants[0].Check != "exit 0" {
		t.Fatalf("a term reached the goal without its check: %+v", submitted.Invariants[0])
	}
	// The operator's own words for what being finished means, not the default.
	if submitted.StopCondition != "the client is upgraded and the suite passes" {
		t.Fatalf("stop condition = %q, want the file's", submitted.StopCondition)
	}

	// Adopted, which is what commits the run to them: from here the engine refuses a
	// spec that drops one or rewords it.
	status, err := goal.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode the goal's status: %v", err)
	}
	if len(status.Invariants) != 2 || status.Invariants[0].Mark != goal.InvariantMark(spec.Invariants[0]) {
		t.Fatalf("the terms were carried but never adopted: %+v", status.Invariants)
	}

	// And the operator can see it from the run's own output, which is the only place
	// they can check the file was picked up at all.
	if !strings.Contains(out.String(), "public-api") || !strings.Contains(out.String(), "checked by: exit 0") {
		t.Fatalf("the run did not read its terms back:\n%s", out.String())
	}
}

// The auditor the binary wires, against the sandbox the binary runs commands in, with
// no reconciler around it. Nothing covered this: every test of CommandAuditor supplies
// a fake sandbox, so "the check runs" was an assumption on every platform the binary
// ships to.
//
// It is a separate pass from the acceptance one because the two fail differently. This
// one failing says the check cannot be run here at all; the acceptance pass failing
// says the engine did not act on what the check found.
func TestTheAuditorRunsAChecksCommandInTheRealSandbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := memStore(t)
	rstore := store.Resources(mustRegistry(t))
	run, err := assembleMission(alwaysDone{}, harness.Plan{}, t.TempDir(), defaultSystemPrompt,
		rstore, store.Jobs(), store.Log(), nil, "", sandbox.ResourceLimits{}, false, false, gateSetup{})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })

	auditor := evidence.NewCommandAuditor(run.parts.sandbox, store.Log(), nil,
		dispatch.WithAdmitter(capability.Admitter{}),
		dispatch.WithHook(capability.NewContainmentGate(run.parts.sandbox)))
	r := resource.Resource{Kind: goal.Kind, Name: "probe"}

	// A term's check is model-adjacent work run on the operator's machine, so it is
	// dispatched semi-trusted and the containment gate refuses it where the host has
	// only a process jail (a GitHub Actions runner, which forbids unprivileged user
	// namespaces, is exactly that host). That is the honest answer and it is asserted
	// here rather than skipped: what must not happen is a run carrying on unaudited,
	// or, as it did, carrying on forever with nobody told.
	if sandbox.ContainmentOf(run.parts.sandbox) < sandbox.Required(sandbox.TrustSemi) {
		_, err := auditor.Audit(ctx, r, goal.Spec{}, goal.Status{},
			[]goal.Invariant{{ID: "held", Statement: "it holds", Check: "exit 0"}})
		if err == nil {
			t.Fatal("a host that cannot contain semi-trusted work ran the check anyway")
		}
		if fault.Classify(err) != fault.Terminal {
			t.Fatalf("a refused check does not stop the run: classified %q: %v", fault.Classify(err), err)
		}
		return
	}

	breaches, err := auditor.Audit(ctx, r, goal.Spec{}, goal.Status{},
		[]goal.Invariant{{ID: "held", Statement: "it holds", Check: "exit 0"}})
	if err != nil {
		t.Fatalf("a passing check could not be run in the sandbox: %v", err)
	}
	if len(breaches) != 0 {
		t.Fatalf("a check that exited 0 was read as a breach: %+v", breaches)
	}

	breaches, err = auditor.Audit(ctx, r, goal.Spec{}, goal.Status{},
		[]goal.Invariant{{ID: "broken", Statement: "it holds", Check: "exit 1"}})
	if err != nil {
		t.Fatalf("a failing check could not be run in the sandbox: %v", err)
	}
	if len(breaches) != 1 || breaches[0].ID != "broken" {
		t.Fatalf("a check that exited non-zero produced no breach: %+v", breaches)
	}
}

// A fan-out run can state terms too, and `flynn goal --fanout --goal-spec` is the
// combination an operator reaches for first: a run spending concurrent children on the
// objective is where what may not be traded away matters most, and the parent is the
// only place the whole run can be judged.
//
// The fan-out assembly wired no auditor until the terms had a surface, so such a goal
// stalled at admission with InvariantAuditorMissing. That is the engine refusing
// honestly, and it is still a run that never gets to do anything. The assertion is
// negative on purpose: a goal that stalls saying nobody can check its terms looks, to
// anything counting stopped runs, exactly like a goal stopped by a guard that worked.
func TestFanoutAssemblyCanRuleOnAStatedTerm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := memStore(t)
	rstore := store.Resources(mustRegistry(t))
	run, err := assembleFanoutMission(alwaysDone{}, harness.Plan{}, t.TempDir(), defaultSystemPrompt,
		rstore, store.Jobs(), store.Log(), nil, "", nil, sandbox.ResourceLimits{}, gateSetup{})
	if err != nil {
		t.Fatalf("assemble fan-out: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	done := make(chan struct{})
	go func() { _ = run.rt.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	spec := loadWrittenGoalSpec(t, `{
	  "objective": "deliver the work",
	  "invariants": [{"id": "clean-tree", "statement": "the tree stays clean", "check": "exit 0"}]
	}`)
	if _, err := run.rt.SubmitGoal(ctx, "termed", goal.Spec{
		Objective:     spec.Objective,
		StopCondition: stopCondition(spec.StopCondition),
		Grant:         []string{mission.ActionModelGenerate},
		Invariants:    spec.Invariants,
	}); err != nil {
		t.Fatalf("submit a fan-out goal that states its terms: %v", err)
	}

	// Adoption is the first reconcile, and the stall this guards against happens on that
	// same pass, so the term being adopted with the goal still running is the answer.
	for {
		r, err := rstore.Get(ctx, goal.Kind, resource.Scope{}, "termed")
		if err == nil {
			st, derr := goal.DecodeStatus(r)
			if derr == nil {
				if st.Unwired {
					t.Fatalf("a fan-out goal stating terms has nobody to check them: %s", st.Message)
				}
				if len(st.Invariants) == 1 && st.Invariants[0].ID == "clean-tree" {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("the fan-out goal never adopted its term")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A run stating no terms submits none, and says nothing about them: the default path
// for every run that passes no spec file, unchanged.
func TestARunWithNoSpecFileStatesNoTerms(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := memStore(t)
	rstore := store.Resources(mustRegistry(t))

	var out bytes.Buffer
	_, runID, _, err := drive(ctx, &out, llmtest.NewScripted(llmtest.SayText("done")), harness.Plan{},
		t.TempDir(), "do the work", defaultSystemPrompt,
		rstore, store.Jobs(), store.Log(), false, "", nil)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}

	r, err := rstore.Get(ctx, goal.Kind, resource.Scope{}, runID)
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	submitted, err := goal.DecodeSpec(r)
	if err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if len(submitted.Invariants) != 0 {
		t.Fatalf("a run that stated no terms carries some: %+v", submitted.Invariants)
	}
	if submitted.StopCondition != defaultStopCondition {
		t.Fatalf("stop condition = %q, want the default", submitted.StopCondition)
	}
	if strings.Contains(out.String(), "terms") {
		t.Fatalf("a run with no terms said something about them:\n%s", out.String())
	}
}
