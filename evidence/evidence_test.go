package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// fakeSandbox runs no commands: it reports a scripted exit code and output, or refuses to
// run at all, which is the case that separates a check that failed from one that could not
// be run.
type fakeSandbox struct {
	sandbox.Sandbox
	exit    int
	output  string
	execErr error
	lines   []string
}

func (f *fakeSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	f.lines = append(f.lines, cmd.Line)
	if f.execErr != nil {
		return sandbox.ExecResult{}, f.execErr
	}
	return sandbox.ExecResult{Output: f.output, ExitCode: f.exit}, nil
}

// refusingAdmitter declines every action, standing in for a capability grant that does not
// carry the verify action or a containment gate on a host that cannot isolate the work.
type refusingAdmitter struct{}

func (refusingAdmitter) Admit(context.Context, dispatch.Action) error {
	return fault.New(fault.Forbidden, "not_granted", "the verification is not admitted here")
}

func goalRes(name string) resource.Resource {
	return resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: name}
}

func item(t *testing.T, text, verify string) goal.LedgerItem {
	t.Helper()
	ledger, err := goal.AppendItems(nil, goal.LedgerItem{Item: text, Verify: verify})
	if err != nil {
		t.Fatalf("build item: %v", err)
	}
	return ledger[0]
}

// TestRecordedVerdictReadsBackThroughTheGatesOwnDecoder: the producer and the gate share
// one derivation of the wire contract, so a verification the producer thinks it wrote is
// exactly the one the gate later reads.
func TestRecordedVerdictReadsBackThroughTheGatesOwnDecoder(t *testing.T) {
	log := spine.NewMemoryLog()
	ev := NewSpineEvidence(log)
	r := goalRes("run-1")
	ctx := context.Background()

	first, err := ev.Record(ctx, r, "aaaa", goal.ItemVerdict{Passed: true, Executed: true, ExitCode: 0, Output: "ok"})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	second, err := ev.Record(ctx, r, "bbbb", goal.ItemVerdict{Passed: false})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if first.Ref == second.Ref {
		t.Fatal("two appends shared one ref; a verification could then certify two items")
	}

	recorded, err := ev.Recorded(ctx, r)
	if err != nil {
		t.Fatalf("Recorded: %v", err)
	}
	if len(recorded) != 2 || recorded[0] != first || recorded[1] != second {
		t.Fatalf("read back %+v, want exactly what was written in order", recorded)
	}
	if recorded[0].Provenance != goal.ProvenanceExecuted || recorded[1].Provenance != goal.ProvenanceAsserted {
		t.Fatalf("provenance = %q/%q, want executed then asserted", recorded[0].Provenance, recorded[1].Provenance)
	}
}

// TestExecutedEvidenceCarriesWhatWasObserved: the executed case records the exit code and a
// hash of the output, so a later reader can tell a check that passed from one merely said
// to have passed, without the record growing by every test run's log.
func TestExecutedEvidenceCarriesWhatWasObserved(t *testing.T) {
	log := spine.NewMemoryLog()
	ctx := context.Background()
	if _, err := NewSpineEvidence(log).Record(ctx, goalRes("run-2"), "cccc",
		goal.ItemVerdict{Passed: false, Executed: true, ExitCode: 2, Output: "FAIL two tests"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	events, err := log.Read(ctx, spine.Query{Stream: "run-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	e := events[0]
	if e.Actor != spine.ActorSystem {
		t.Fatalf("actor = %q, want the runtime rather than the agent", e.Actor)
	}
	if got := e.Payload[chain.ItemOutputKey]; got != outputHash("FAIL two tests") {
		t.Fatalf("output hash = %v, want the hash of what was observed", got)
	}
	if e.Payload[chain.ItemExitKey] == nil {
		t.Fatal("an executed verdict recorded no exit code")
	}
	// The detail of an executed verdict is the check's own output, which is hashed rather
	// than stored. Copying it into the reason as well would put the output back on the
	// record by another name.
	if _, ok := e.Payload[chain.ItemReasonKey]; ok {
		t.Fatal("an executed verdict recorded a reason nothing ran")
	}
}

// TestUnexecutedEvidenceRecordsWhyNothingRan is the counterpart to the exit code. An item
// whose check never ran reports only that it could not be run, and that names no cause: a
// clause no host could execute and a sandbox that failed to start read identically to
// whoever is handed the stopped goal, and only one of those is worth trying to fix.
func TestUnexecutedEvidenceRecordsWhyNothingRan(t *testing.T) {
	log := spine.NewMemoryLog()
	ctx := context.Background()
	const why = "the check could not run: the sandbox refused to start"
	if _, err := NewSpineEvidence(log).Record(ctx, goalRes("run-9"), "eeee",
		goal.ItemVerdict{Detail: why}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	events, _ := log.Read(ctx, spine.Query{Stream: "run-9"})
	if got := events[0].Payload[chain.ItemReasonKey]; got != why {
		t.Fatalf("reason = %v, want %q", got, why)
	}
	// And it reads back through the gate's own decoder, which is what puts it in front of
	// whoever is handed the goal rather than only in the log.
	vs := goal.VerificationsFrom(events)
	if len(vs) != 1 || vs[0].Reason != why {
		t.Fatalf("decoded = %+v, want one verification carrying the reason", vs)
	}
}

// TestAssertedEvidenceDescribesNoExecution: a verdict nothing was run for has no exit code
// and no output to name, and recording zeros for them would be a claim about an execution
// that did not happen.
func TestAssertedEvidenceDescribesNoExecution(t *testing.T) {
	log := spine.NewMemoryLog()
	ctx := context.Background()
	if _, err := NewSpineEvidence(log).Record(ctx, goalRes("run-3"), "dddd",
		goal.ItemVerdict{Passed: true, ExitCode: 7, Output: "ignored"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	events, _ := log.Read(ctx, spine.Query{Stream: "run-3"})
	e := events[0]
	if e.Payload[chain.ItemProvenanceKey] != chain.ProvenanceAsserted {
		t.Fatalf("provenance = %v, want asserted", e.Payload[chain.ItemProvenanceKey])
	}
	if _, ok := e.Payload[chain.ItemExitKey]; ok {
		t.Fatal("an asserted verdict recorded an exit code for an execution that never happened")
	}
	if _, ok := e.Payload[chain.ItemOutputKey]; ok {
		t.Fatal("an asserted verdict recorded an output hash for an execution that never happened")
	}
}

// TestVerifierRunsTheClauseAndTreatsExitZeroAsProof.
func TestVerifierRunsTheClauseAndTreatsExitZeroAsProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exit   int
		passed bool
	}{
		{"exit 0 proves the item", 0, true},
		{"a non-zero exit does not", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &fakeSandbox{exit: tc.exit, output: "some output"}
			v, err := NewCommandVerifier(sb).VerifyItem(context.Background(), goalRes("r"), item(t, "do it", "go test ./..."))
			if err != nil {
				t.Fatalf("VerifyItem: %v", err)
			}
			if v.Passed != tc.passed || !v.Executed {
				t.Fatalf("verdict = %+v, want passed=%v executed", v, tc.passed)
			}
			if v.ExitCode != tc.exit {
				t.Fatalf("exit = %d, want %d", v.ExitCode, tc.exit)
			}
			if len(sb.lines) != 1 || sb.lines[0] != "go test ./..." {
				t.Fatalf("ran %v, want the item's own declared clause", sb.lines)
			}
			if !strings.Contains(v.Detail, "go test ./...") {
				t.Fatalf("detail = %q, want it to name the check that ran", v.Detail)
			}
		})
	}
}

// TestEveryWayOfNotRunningIsUnexecutedAndUnpassed: a clause no mechanism can run is a real
// and common outcome, so it surfaces as an honest unexecuted verdict the gate can rule on
// rather than as a failure that stalls the goal or, worse, as a pass.
func TestEveryWayOfNotRunningIsUnexecutedAndUnpassed(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		v    *CommandVerifier
		item goal.LedgerItem
		want string
	}{
		{
			name: "no clause to run",
			v:    NewCommandVerifier(&fakeSandbox{}),
			item: goal.LedgerItem{Item: "vague", Verify: "   "},
			want: "no check",
		},
		{
			name: "no sandbox to run it in",
			v:    NewCommandVerifier(nil),
			item: item(t, "do it", "true"),
			want: "no sandbox",
		},
		{
			name: "the sandbox could not start it",
			v:    NewCommandVerifier(&fakeSandbox{execErr: errors.New("no shell")}),
			item: item(t, "do it", "true"),
			want: "could not run",
		},
		{
			name: "admission refused it",
			v:    NewCommandVerifier(&fakeSandbox{}, dispatch.WithAdmitter(refusingAdmitter{})),
			item: item(t, "do it", "true"),
			want: "could not run",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := tc.v.VerifyItem(ctx, goalRes("r"), tc.item)
			if err != nil {
				t.Fatalf("VerifyItem returned a hard error for a check that merely could not run: %v", err)
			}
			if v.Passed || v.Executed {
				t.Fatalf("verdict = %+v, want neither passed nor executed", v)
			}
			if !strings.Contains(v.Detail, tc.want) {
				t.Fatalf("detail = %q, want it to mention %q", v.Detail, tc.want)
			}
		})
	}
}

// TestACancelledVerificationIsAHardError: a cancelled run is shutdown, not evidence that
// an item is unproven, so it is the one case that does not become a verdict.
func TestACancelledVerificationIsAHardError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sb := &fakeSandbox{execErr: errors.New("context cancelled")}
	if _, err := NewCommandVerifier(sb).VerifyItem(ctx, goalRes("r"), item(t, "do it", "true")); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
}

// TestVerificationIsAttributedToTheRunsScope: a verification is governed like any tool the
// agent invokes, attributed to the same scope and goal as the work that proposed it, so it
// is not a side channel around the waist.
func TestVerificationIsAttributedToTheRunsScope(t *testing.T) {
	var seen dispatch.Action
	admit := admitterFunc(func(_ context.Context, a dispatch.Action) error {
		seen = a
		return nil
	})
	r := goalRes("run-9")
	r.Scope = resource.Scope{Project: "proj"}
	if _, err := NewCommandVerifier(&fakeSandbox{}, dispatch.WithAdmitter(admit)).
		VerifyItem(context.Background(), r, item(t, "do it", "true")); err != nil {
		t.Fatalf("VerifyItem: %v", err)
	}
	if seen.Name != VerifyItemAction {
		t.Fatalf("action = %q, want %q", seen.Name, VerifyItemAction)
	}
	if seen.Goal != "run-9" {
		t.Fatalf("goal = %q, want the run it verifies", seen.Goal)
	}
	if seen.Scope != state.Scope(r.Scope) {
		t.Fatalf("scope = %+v, want the goal's own", seen.Scope)
	}
	if seen.Trust != sandbox.TrustSemi {
		t.Fatalf("trust = %v, want the same level the shell tool declares for model-authored commands", seen.Trust)
	}
}

type admitterFunc func(context.Context, dispatch.Action) error

func (f admitterFunc) Admit(ctx context.Context, a dispatch.Action) error { return f(ctx, a) }

// TestProvenanceIsTheVerdictsOwnExecutionAndNothingElse is the property the whole scheme
// rests on: across any verdict the producer is handed, the provenance that reaches the
// record is executed exactly when the verifier reported it ran, and asserted otherwise.
// There is no other input to it, so no payload a model can influence can move it.
func TestProvenanceIsTheVerdictsOwnExecutionAndNothingElse(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		v := goal.ItemVerdict{
			Passed:   rapid.Bool().Draw(rt, "passed"),
			Executed: rapid.Bool().Draw(rt, "executed"),
			ExitCode: rapid.IntRange(-1, 255).Draw(rt, "exit"),
			Output:   rapid.String().Draw(rt, "output"),
			Detail:   rapid.String().Draw(rt, "detail"),
		}
		itemID := rapid.StringMatching(`[a-f0-9]{16}`).Draw(rt, "item")

		log := spine.NewMemoryLog()
		ev := NewSpineEvidence(log)
		r := goalRes("prop")
		ctx := context.Background()

		written, err := ev.Record(ctx, r, itemID, v)
		if err != nil {
			rt.Fatalf("Record: %v", err)
		}
		want := goal.ProvenanceAsserted
		if v.Executed {
			want = goal.ProvenanceExecuted
		}
		if written.Provenance != want {
			rt.Fatalf("provenance = %q, want %q for executed=%v", written.Provenance, want, v.Executed)
		}
		if written.Passed != v.Passed || written.Item != itemID {
			rt.Fatalf("wrote %+v, want the verdict and item it was handed", written)
		}

		// What the gate reads back must be what the producer believes it wrote, or a
		// verification exists that the gate will never see.
		read, err := ev.Recorded(ctx, r)
		if err != nil {
			rt.Fatalf("Recorded: %v", err)
		}
		if len(read) != 1 || read[0] != written {
			rt.Fatalf("read back %+v, want %+v", read, written)
		}
	})
}

// TestDetailKeepsTheTailOfALongOutput: a failing command's diagnosis is almost always its
// last lines, so the bounded detail handed back to the agent keeps the end, not the start.
func TestDetailKeepsTheTailOfALongOutput(t *testing.T) {
	long := strings.Repeat("noise\n", 2000) + "the actual error"
	v, err := NewCommandVerifier(&fakeSandbox{exit: 1, output: long}).
		VerifyItem(context.Background(), goalRes("r"), item(t, "do it", "go build ./..."))
	if err != nil {
		t.Fatalf("VerifyItem: %v", err)
	}
	if !strings.Contains(v.Detail, "the actual error") {
		t.Fatal("the detail dropped the tail, which is where a failing command says why")
	}
	if len(v.Detail) > maxDetail+len("`go build ./...` exited 1\n…")+8 {
		t.Fatalf("detail is %d bytes, want it bounded near maxDetail", len(v.Detail))
	}
	if v.Output != long {
		t.Fatal("the verdict's own output was clipped; only the agent-facing detail is bounded")
	}
}

// TestAnUnreachableRecordIsTransient: a record that cannot be reached for a moment is not a
// run with no evidence, so both halves of the port classify the failure as retryable rather
// than letting it read as an unproven item.
func TestAnUnreachableRecordIsTransient(t *testing.T) {
	ev := NewSpineEvidence(brokenLog{})
	ctx := context.Background()
	if _, err := ev.Record(ctx, goalRes("r"), "aaaa", goal.ItemVerdict{}); fault.Classify(err) != fault.Transient {
		t.Fatalf("Record error class = %v, want transient", fault.Classify(err))
	}
	if _, err := ev.Recorded(ctx, goalRes("r")); fault.Classify(err) != fault.Transient {
		t.Fatalf("Recorded error class = %v, want transient", fault.Classify(err))
	}
}

// brokenLog is a spine.Log whose every operation fails, standing in for a record that is
// briefly unreachable.
type brokenLog struct{ spine.Log }

func (brokenLog) Append(context.Context, spine.AppendInput) (spine.Event, error) {
	return spine.Event{}, errors.New("the log is unreachable")
}

func (brokenLog) Read(context.Context, spine.Query) ([]spine.Event, error) {
	return nil, errors.New("the log is unreachable")
}
