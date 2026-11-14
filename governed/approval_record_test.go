package governed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/spine"
)

// funcPrompter adapts a function to [mission.ApprovalPrompter].
type funcPrompter func(ctx context.Context, req mission.ApprovalRequest) (mission.ApprovalDecision, error)

func (f funcPrompter) Prompt(ctx context.Context, req mission.ApprovalRequest) (mission.ApprovalDecision, error) {
	return f(ctx, req)
}

// flakyLog accepts okAppends appends and then refuses every one, standing in for a record
// that stops being writable.
type flakyLog struct {
	spine.Log
	okAppends int
}

func (l *flakyLog) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	if l.okAppends <= 0 {
		return spine.Event{}, errors.New("disk full")
	}
	l.okAppends--
	return l.Log.Append(ctx, in)
}

func flakyStack(t *testing.T, okAppends int) *approvalStack {
	t.Helper()
	stack, err := newApprovalStack([]string{"shell"}, &flakyLog{Log: spine.NewMemoryLog(), okAppends: okAppends}, "run-1")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return stack
}

func recordedStack(t *testing.T) (*approvalStack, spine.Log) {
	t.Helper()
	log := spine.NewMemoryLog()
	stack, err := newApprovalStack([]string{"shell"}, log, "run-1")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return stack, log
}

func streamOf(t *testing.T, log spine.Log) []spine.Event {
	t.Helper()
	evs, err := log.Read(context.Background(), spine.Query{Stream: "run-1"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out []spine.Event
	for _, e := range evs {
		// The gate also writes its own verdicts to the stream; only the request and the
		// resolutions that carry an outcome belong to the recorder.
		if e.Type == "approval.requested" || (e.Type == "approval.decision" && e.Payload["outcome"] != nil) {
			out = append(out, e)
		}
	}
	return out
}

// TestRecordedPromptWritesOneResolutionPerReason is the table for the recorder: whichever
// way the prompt ends, the request is on the stream first and exactly one resolution
// follows, with the closed-set reason that path means and the author that reason implies.
func TestRecordedPromptWritesOneResolutionPerReason(t *testing.T) {
	cases := []struct {
		name      string
		inner     func(ctx context.Context, cancel context.CancelFunc) (mission.ApprovalDecision, error)
		grace     time.Duration
		wantAllow bool
		wantErr   bool
		reason    string
		actor     spine.ActorType
	}{
		{"allowed", func(context.Context, context.CancelFunc) (mission.ApprovalDecision, error) {
			return mission.ApprovalDecision{Allow: true, Scope: "ls *"}, nil
		}, time.Minute, true, false, "allowed", spine.ActorHuman},
		{"denied", func(context.Context, context.CancelFunc) (mission.ApprovalDecision, error) {
			return mission.ApprovalDecision{Feedback: "no"}, nil
		}, time.Minute, false, false, "denied", spine.ActorHuman},
		{"expired", func(ctx context.Context, _ context.CancelFunc) (mission.ApprovalDecision, error) {
			<-ctx.Done()
			return mission.ApprovalDecision{}, ctx.Err()
		}, 20 * time.Millisecond, false, true, "expired", spine.ActorSystem},
		{"cancelled", func(ctx context.Context, cancel context.CancelFunc) (mission.ApprovalDecision, error) {
			cancel()
			return mission.ApprovalDecision{}, ctx.Err()
		}, time.Minute, false, true, "cancelled", spine.ActorSystem},
		{"disconnected", func(context.Context, context.CancelFunc) (mission.ApprovalDecision, error) {
			return mission.ApprovalDecision{}, approval.ErrDisconnected
		}, time.Minute, false, true, "disconnected", spine.ActorSystem},
		{"unexplained failure is disconnected", func(context.Context, context.CancelFunc) (mission.ApprovalDecision, error) {
			return mission.ApprovalDecision{}, errors.New("pipe closed")
		}, time.Minute, false, true, "disconnected", spine.ActorSystem},
		{"an allow that raced the deadline is not honoured", func(ctx context.Context, _ context.CancelFunc) (mission.ApprovalDecision, error) {
			<-ctx.Done()
			return mission.ApprovalDecision{Allow: true}, nil
		}, 20 * time.Millisecond, false, true, "expired", spine.ActorSystem},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stack, log := recordedStack(t)
			ctx, cancel := context.WithTimeout(context.Background(), tc.grace)
			defer cancel()
			var openedAfter int
			p := stack.recording(funcPrompter(func(ctx context.Context, req mission.ApprovalRequest) (mission.ApprovalDecision, error) {
				openedAfter = len(streamOf(t, log))
				return tc.inner(ctx, cancel)
			}))

			dec, err := p.Prompt(ctx, mission.ApprovalRequest{Action: "shell", Host: "box", Grace: tc.grace})
			if (err != nil) != tc.wantErr || dec.Allow != tc.wantAllow {
				t.Fatalf("allow = %v, err = %v; want allow %v, err %v", dec.Allow, err, tc.wantAllow, tc.wantErr)
			}
			if openedAfter != 1 {
				t.Fatalf("%d records existed when the prompt opened, want the request alone", openedAfter)
			}
			evs := streamOf(t, log)
			if len(evs) != 2 || evs[0].Type != "approval.requested" || evs[1].Type != "approval.decision" {
				t.Fatalf("stream = %+v, want the request then one resolution", evs)
			}
			if evs[0].Payload["requestId"] != evs[1].Payload["requestId"] || evs[0].Payload["requestId"] == "" {
				t.Fatalf("the resolution does not name its request: %v / %v", evs[0].Payload, evs[1].Payload)
			}
			if got := evs[1].Payload["reason"]; got != tc.reason {
				t.Fatalf("reason = %v, want %s", got, tc.reason)
			}
			if evs[1].Actor != tc.actor {
				t.Fatalf("actor = %s, want %s", evs[1].Actor, tc.actor)
			}
		})
	}
}

// TestRecordedPromptFailsClosedWhenTheRecordCannotBeWritten: a request that cannot be
// recorded is never put to the person, so an allow cannot exist without a question behind
// it.
func TestRecordedPromptFailsClosedWhenTheRecordCannotBeWritten(t *testing.T) {
	stack := flakyStack(t, 0)
	asked := false
	p := stack.recording(funcPrompter(func(context.Context, mission.ApprovalRequest) (mission.ApprovalDecision, error) {
		asked = true
		return mission.ApprovalDecision{Allow: true}, nil
	}))
	dec, err := p.Prompt(context.Background(), mission.ApprovalRequest{Action: "shell", Grace: time.Minute})
	if err == nil || dec.Allow {
		t.Fatalf("allow = %v, err = %v; want a refusal", dec.Allow, err)
	}
	if asked {
		t.Fatal("the person was asked a question the record could not hold")
	}
}

// TestRecordedAllowWithoutAResolutionIsNotAnAllow: the request was written and the person
// said yes, but the answer cannot be recorded. Returning the allow would leave a sealed
// history with a question and no answer next to an action that ran, so it declines.
func TestRecordedAllowWithoutAResolutionIsNotAnAllow(t *testing.T) {
	stack := flakyStack(t, 1)
	p := stack.recording(funcPrompter(func(context.Context, mission.ApprovalRequest) (mission.ApprovalDecision, error) {
		return mission.ApprovalDecision{Allow: true}, nil
	}))
	dec, err := p.Prompt(context.Background(), mission.ApprovalRequest{Action: "shell", Grace: time.Minute})
	if err == nil || dec.Allow {
		t.Fatalf("allow = %v, err = %v; want a refusal", dec.Allow, err)
	}

	// A denial that cannot be recorded is still a denial.
	stack = flakyStack(t, 1)
	p = stack.recording(funcPrompter(func(context.Context, mission.ApprovalRequest) (mission.ApprovalDecision, error) {
		return mission.ApprovalDecision{}, nil
	}))
	dec, err = p.Prompt(context.Background(), mission.ApprovalRequest{Action: "shell", Grace: time.Minute})
	if err != nil || dec.Allow {
		t.Fatalf("allow = %v, err = %v; want a plain denial", dec.Allow, err)
	}
}

// TestRecordingKeepsNoPrompterNil: no prompter is the non-interactive answer, and a
// recorder around nothing would make it look interactive.
func TestRecordingKeepsNoPrompterNil(t *testing.T) {
	stack, _ := recordedStack(t)
	if got := stack.recording(nil); got != nil {
		t.Fatalf("a nil prompter was wrapped: %T", got)
	}
	var none *approvalStack
	inner := funcPrompter(func(context.Context, mission.ApprovalRequest) (mission.ApprovalDecision, error) {
		return mission.ApprovalDecision{}, nil
	})
	if got := none.recording(inner); got == nil {
		t.Fatal("a nil stack dropped the prompter")
	}
}
