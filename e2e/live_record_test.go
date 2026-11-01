package e2e

import "testing"

// The live lane trusts its record decoder to read what a model did. These tests run it
// against a scripted model, so a change to the record's shape fails here, in the default
// run, instead of surfacing as a live scenario that sees nothing.

// TestRecordDecoderReadsToolCallsRefusalsAndPosture drives one run in which the model writes
// a file that needs approval (refused) and then runs a shell command, and checks that the
// decoder reports the calls, their results, the refusal, the sandbox posture and the
// run's convergence.
func TestRecordDecoderReadsToolCallsRefusalsAndPosture(t *testing.T) {
	fake := newFakeOpenAIQueue(
		t,
		toolCall("w1", "write", `{"path":"a.txt","content":"x"}`),
		toolCall("b1", "bash", `{"command":"echo decoded-marker"}`),
		finalText("Done."),
	)
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "-require-approval", "write", "goal", "write a.txt then echo")
	rec := in.sealedRun(res)

	names := callNames(rec)
	if len(names) != 2 || names[0] != "write" || names[1] != "bash" {
		t.Fatalf("calls = %v, want [write bash]", names)
	}
	if got := rec.calls("bash")[0].command(); got != "echo decoded-marker" {
		t.Errorf("the bash call's command = %q", got)
	}
	if !ranCommand(rec, "decoded-marker") {
		t.Error("ranCommand did not find the command the model ran")
	}
	if !containsString(rec.rejections(), "approval_required") {
		t.Errorf("rejections = %v, want approval_required", rec.rejections())
	}
	if _, err := in.workfile("a.txt"); err == nil {
		t.Error("the refused write created its file")
	}
	if !rec.converged() {
		t.Error("the run did not record convergence")
	}
	if p := rec.posture(t); p["network"] == nil || p["credentials"] == nil {
		t.Errorf("posture = %v, want network and credentials stated", p)
	}
	if out, isErr := rec.result(rec.calls("write")[0].id); !isErr || out == "" {
		t.Errorf("the refused write's result = %q (error %v), want the refusal reported as an error", out, isErr)
	}
}
