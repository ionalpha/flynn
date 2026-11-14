package e2e

import (
	"strings"
	"testing"
	"time"
)

// planRequests returns the planning-phase requests the binary has made so far.
func (f *fakeOpenAI) planRequests() []oaiRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]oaiRequest(nil), f.plans...)
}

// correctionAfterRefusal starts a goal against a planner that always answers with plan, waits
// for the planner to be asked a second time, and returns that request's last message, which
// is the correction. The goal is then killed: a planner that will not change its check puts
// the goal on the retry ladder, and the scenario is over once the planner has been told why.
// It also fails if the build model was called, since no work may start before admission.
func correctionAfterRefusal(t *testing.T, plan string) string {
	t.Helper()
	fake := newFakeOpenAI(t, finalText("done")).planWith(oaiReply{Text: plan})
	in := newInstance(t).withModel(fake)

	proc := in.start("-no-learn", "goal", "state the answer")
	for polls := 0; len(fake.planRequests()) < 2; polls++ {
		if polls > 1200 { // 60s
			proc.kill(t)
			t.Fatalf("the planner was asked %d times in 60s, want a correction after the refusal\n%s%s", len(fake.planRequests()), proc.out, proc.errb)
		}
		time.Sleep(50 * time.Millisecond)
	}
	proc.kill(t)

	if n := fake.count(); n != 0 {
		t.Fatalf("the build model was called %d times for a goal that never passed admission", n)
	}
	second := fake.planRequests()[1]
	return second.Messages[len(second.Messages)-1].Content
}

// TestAGoalWhoseCheckAlreadyPassesIsRefusedBeforeAnyWork drives the whole binary with a
// planner that keeps writing a check which passes on the untouched tree. The check is run
// once in the sandbox before the build starts, refused, and sent back to the planner with
// the reason, so the planner is asked again; when it will not change the check the goal
// stops at planning and the build model is never called. Without admission the plan is
// accepted and the run builds against a check that cannot tell finished work from none.
func TestAGoalWhoseCheckAlreadyPassesIsRefusedBeforeAnyWork(t *testing.T) {
	msg := correctionAfterRefusal(t, `[{"item":"accomplish the objective","verify":"`+passingCheck+`"}]`)
	if !strings.Contains(msg, "already passes") || !strings.Contains(msg, passingCheck) {
		t.Fatalf("the correction did not say which check passes already and why:\n%s", msg)
	}
}

// TestAGoalWhoseCheckNamesAFileNobodyAskedForIsRefusedBeforeAnyWork is the other common
// planning mistake: a check on a path that does not exist and that the task never names. The
// text rules refuse it without running anything, and the correction names the path.
func TestAGoalWhoseCheckNamesAFileNobodyAskedForIsRefusedBeforeAnyWork(t *testing.T) {
	msg := correctionAfterRefusal(t, `[{"item":"accomplish the objective","verify":"type invented-report.txt"}]`)
	if !strings.Contains(msg, "invented-report.txt") {
		t.Fatalf("the correction did not name the file:\n%s", msg)
	}
}
