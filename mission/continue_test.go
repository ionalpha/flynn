package mission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// resWithStatus builds a Goal resource carrying the suite's spec and a full status
// (not just the checkpoint), the shape a reopened turn is driven from.
func resWithStatus(t *testing.T, status goal.Status) resource.Resource {
	t.Helper()
	spec, err := json.Marshal(goal.Spec{Objective: "do the thing", StopCondition: "it is done"})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := status.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: spec, Status: enc}
}

// driveTurn runs the executor step by step from a starting status until Convergence
// reports done, returning the final checkpoint. It threads the persisted checkpoint
// between steps, exercising crash-resume the same way driveToDone does, but from an
// arbitrary opening status so a reopened conversation can be driven.
func driveTurn(t *testing.T, exec *Executor, status goal.Status, maxSteps int) json.RawMessage {
	t.Helper()
	spec := goal.Spec{Objective: "do the thing", StopCondition: "it is done"}
	cur := status
	for step := 1; step <= maxSteps; step++ {
		next, err := exec.Execute(context.Background(), resWithStatus(t, cur))
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		cur = goal.Status{Checkpoint: next}
		met, _, err := Convergence{}.Met(context.Background(), spec, cur)
		if err != nil {
			t.Fatal(err)
		}
		if met {
			return next
		}
	}
	t.Fatalf("turn did not converge within %d steps", maxSteps)
	return nil
}

// TestContinueConversationReopensWithNewTurn proves the multi-turn mechanism: after
// a goal converges, ContinueConversation appends the user's next line and reopens
// the goal so the next drive continues the same exchange. The reopened status is
// clean (not done, pending phase, fresh step budget, no in-flight step) and the
// model, when driven again, is handed the whole prior conversation rather than
// starting cold.
func TestContinueConversationReopensWithNewTurn(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.SayText("first answer"),
		llmtest.SayText("second answer"),
	)
	exec := NewExecutor(model, WithSystem("sys"))

	// Turn 1 converges.
	_, _, raw := driveToDone(t, exec, 5)

	// Reopen, with a settled status that also carries the cruft a finished or
	// interrupted turn leaves behind, to prove ContinueConversation clears it.
	settled := goal.Status{
		Checkpoint: raw,
		Phase:      goal.PhaseConverged,
		Steps:      3,
		Message:    "conversation reached a final turn",
		InFlight:   &goal.InFlight{JobID: "stale"},
	}
	reopened, err := ContinueConversation(settled, "second question")
	if err != nil {
		t.Fatalf("ContinueConversation: %v", err)
	}
	if reopened.Phase != goal.PhasePending {
		t.Fatalf("phase = %q, want Pending so the reconciler re-drives", reopened.Phase)
	}
	if reopened.Steps != 0 {
		t.Fatalf("steps = %d, want 0 (a fresh per-turn step budget)", reopened.Steps)
	}
	if reopened.InFlight != nil {
		t.Fatal("in-flight step not cleared; a reopened turn must dispatch a new step")
	}
	cp, err := decodeCheckpoint(reopened.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Done {
		t.Fatal("reopened checkpoint is still marked done")
	}
	if cp.Result != "" {
		t.Fatalf("reopened checkpoint kept the prior result %q", cp.Result)
	}
	last := cp.Messages[len(cp.Messages)-1]
	if last.Role != llm.RoleUser || last.TextContent() != "second question" {
		t.Fatalf("last message = %+v, want a user turn with the new line", last)
	}

	// Drive turn 2: it must converge to the second answer, and the model must have
	// seen the first turn in its history (continuity, not a cold restart).
	final := driveTurn(t, exec, reopened, 5)
	cp2, err := decodeCheckpoint(final)
	if err != nil {
		t.Fatal(err)
	}
	if !cp2.Done || cp2.Result != "second answer" {
		t.Fatalf("turn 2 final checkpoint = %+v, want done with \"second answer\"", cp2)
	}
	reqs := model.Requests()
	hist := reqs[len(reqs)-1].Messages
	var sawFirstAnswer, sawSecondQuestion bool
	for _, m := range hist {
		switch m.TextContent() {
		case "first answer":
			sawFirstAnswer = true
		case "second question":
			sawSecondQuestion = true
		}
	}
	if !sawFirstAnswer || !sawSecondQuestion {
		t.Fatalf("turn 2 lost continuity: sawFirstAnswer=%v sawSecondQuestion=%v\n%+v", sawFirstAnswer, sawSecondQuestion, hist)
	}
}

// TestContinueConversationDecodeError surfaces a corrupt checkpoint as a terminal
// fault rather than silently dropping the conversation.
func TestContinueConversationDecodeError(t *testing.T) {
	_, err := ContinueConversation(goal.Status{Checkpoint: json.RawMessage(`{not json`)}, "hi")
	if err == nil {
		t.Fatal("a corrupt checkpoint must error, not be silently reopened")
	}
}

// driveTurnWithRetry drives a turn the way the worker does: a transient step fault
// is retried from the last persisted checkpoint (no progress is made on a fault), so
// a flaky model or tool recovers without corrupting the conversation. It returns the
// converged checkpoint or fails after maxAttempts.
func driveTurnWithRetry(t *testing.T, exec goal.StepExecutor, status goal.Status, maxAttempts int) json.RawMessage {
	t.Helper()
	spec := goal.Spec{Objective: "do the thing", StopCondition: "it is done"}
	cur := status
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		next, err := exec.Execute(context.Background(), resWithStatus(t, cur))
		if err != nil {
			if fault.Classify(err) == fault.Transient {
				continue // retry from the same checkpoint, as the queue would
			}
			t.Fatalf("attempt %d: non-transient fault: %v", attempt, err)
		}
		cur = goal.Status{Checkpoint: next}
		met, _, err := Convergence{}.Met(context.Background(), spec, cur)
		if err != nil {
			t.Fatal(err)
		}
		if met {
			return next
		}
	}
	t.Fatalf("turn did not converge within %d attempts", maxAttempts)
	return nil
}

// TestMultiTurnSurvivesFlakyStepsChaos injects a deterministic run of transient step
// faults across a two-turn conversation and shows the continuation mechanism is
// robust to retries: each turn still converges to its answer, and crucially the
// reopened conversation accumulates exactly one assistant message per turn. A retried
// step makes no progress, so it must never duplicate a turn's answer or leave the
// history malformed for the next turn to build on.
func TestMultiTurnSurvivesFlakyStepsChaos(t *testing.T) {
	for _, failures := range []int{0, 1, 2, 4} {
		t.Run(fmt.Sprintf("failures_%d", failures), func(t *testing.T) {
			model := llmtest.NewScripted(
				llmtest.SayText("alpha"),
				llmtest.SayText("beta"),
			)
			// The fault lands before Execute delegates, so a failed attempt does not
			// consume a scripted turn: the model is still called exactly twice.
			faulty := testkit.FaultyExecutor(
				NewExecutor(model),
				testkit.FailFirst(failures, fault.New(fault.Transient, "flaky", "retry")),
			)

			raw := driveTurnWithRetry(t, faulty, goal.Status{}, failures+5)
			if cp, _ := decodeCheckpoint(raw); cp.Result != "alpha" {
				t.Fatalf("turn 1 result = %q, want \"alpha\"", cp.Result)
			}

			reopened, err := ContinueConversation(goal.Status{Checkpoint: raw}, "and then")
			if err != nil {
				t.Fatal(err)
			}
			final := driveTurnWithRetry(t, faulty, reopened, failures+5)

			cp, err := decodeCheckpoint(final)
			if err != nil {
				t.Fatal(err)
			}
			if !cp.Done || cp.Result != "beta" {
				t.Fatalf("turn 2 final = %+v, want done with \"beta\"", cp)
			}
			var answers []string
			for _, m := range cp.Messages {
				if m.Role == llm.RoleAssistant {
					answers = append(answers, m.TextContent())
				}
			}
			if len(answers) != 2 || answers[0] != "alpha" || answers[1] != "beta" {
				t.Fatalf("conversation answers = %v, want [alpha beta] (a retry corrupted history)", answers)
			}
			if model.Calls() != 2 {
				t.Fatalf("model called %d times, want 2 (retries must not re-call a completed turn)", model.Calls())
			}
		})
	}
}

// TestMultiTurnRetainsHistoryProperty is the conversational contract: for any number
// of follow-up turns, each continues the same goal, every turn converges, the
// conversation accumulates exactly one assistant answer per turn in order, and the
// model is handed the full prior history on every turn (it never restarts cold).
func TestMultiTurnRetainsHistoryProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nTurns := rapid.IntRange(1, 6).Draw(rt, "turns")
		answers := make([]string, nTurns)
		responses := make([]llm.Response, nTurns)
		for i := range answers {
			// Distinct answers so order and presence are checkable.
			answers[i] = fmt.Sprintf("answer-%d-%s", i, rapid.StringMatching(`[a-z]{2,5}`).Draw(rt, fmt.Sprintf("a%d", i)))
			responses[i] = llmtest.SayText(answers[i])
		}
		model := llmtest.NewScripted(responses...)
		exec := NewExecutor(model)

		var raw json.RawMessage
		for i := range nTurns {
			status := goal.Status{}
			if i > 0 {
				var err error
				status, err = ContinueConversation(goal.Status{Checkpoint: raw}, fmt.Sprintf("question-%d", i))
				if err != nil {
					t.Fatal(err)
				}
			}
			raw = driveTurn(t, exec, status, 4)
		}

		final, err := decodeCheckpoint(raw)
		if err != nil {
			t.Fatal(err)
		}
		// Exactly one assistant message per turn, in order.
		var got []string
		for _, m := range final.Messages {
			if m.Role == llm.RoleAssistant {
				got = append(got, m.TextContent())
			}
		}
		if len(got) != nTurns {
			t.Fatalf("assistant turns = %d, want %d", len(got), nTurns)
		}
		for i, a := range answers {
			if got[i] != a {
				t.Fatalf("answer %d = %q, want %q (history out of order)", i, got[i], a)
			}
		}
		// The model saw the whole history on the final turn: every prior answer is in
		// the last request it received.
		if nTurns > 1 {
			reqs := model.Requests()
			lastReq := reqs[len(reqs)-1]
			var blob strings.Builder
			for _, m := range lastReq.Messages {
				blob.WriteString(m.TextContent())
				blob.WriteByte('\n')
			}
			for i := range nTurns - 1 {
				if !strings.Contains(blob.String(), answers[i]) {
					t.Fatalf("final turn did not carry prior answer %q in its history", answers[i])
				}
			}
		}
	})
}
