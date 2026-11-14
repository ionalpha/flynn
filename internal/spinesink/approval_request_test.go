package spinesink_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// TestRequestPayloadCarriesTheRequest: the request is the system's, names what would be
// authorized, and carries a preview built from the action, scope and host.
func TestRequestPayloadCarriesTheRequest(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewApproval(log, "run-1")
	expires := time.Date(2026, 11, 14, 9, 30, 0, 5, time.UTC)

	err := sink.Request(ctx, spinesink.Request{
		ID: "req-1", Action: "shell", Scope: state.Scope{Instance: "i", Project: "p"},
		Host: "box", Tainted: true, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := log.Read(ctx, spine.Query{Stream: "run-1"})
	if len(evs) != 1 || evs[0].Type != "approval.requested" || evs[0].Actor != spine.ActorSystem {
		t.Fatalf("events = %+v", evs)
	}
	p := evs[0].Payload
	if p["requestId"] != "req-1" || p["action"] != "shell" || p["scope"] != "i/p" || p["host"] != "box" || p["tainted"] != true {
		t.Fatalf("payload = %v", p)
	}
	if p["expiresAt"] != "2026-11-14T09:30:00.000000005Z" {
		t.Fatalf("expiresAt = %v", p["expiresAt"])
	}
	if p["preview"] != approval.Preview("shell", "i/p", "box") {
		t.Fatalf("preview = %v", p["preview"])
	}
}

// TestRequestIsInertAndBounded: text a model could influence reaches the stream with its
// control characters escaped, and a runaway identifier is cut.
func TestRequestIsInertAndBounded(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewApproval(log, "run-1")
	_ = sink.Request(ctx, spinesink.Request{
		ID: strings.Repeat("a", 5000), Action: "sh\x1b[31mell", Host: "ho\rst\u202e",
	})
	evs, _ := log.Read(ctx, spine.Query{Stream: "run-1"})
	p := evs[0].Payload
	for _, k := range []string{"requestId", "action", "host", "preview"} {
		s, _ := p[k].(string)
		if strings.ContainsAny(s, "\x1b\r\u202e") {
			t.Errorf("%s holds a raw control character: %q", k, s)
		}
	}
	if id, _ := p["requestId"].(string); len(id) > 128 {
		t.Errorf("requestId is %d bytes, want at most 128", len(id))
	}
}

// TestAnswerRecordsEveryReasonWithItsAuthor is the table the closed set demands: each
// reason yields its verdict and author, and a value outside the set writes nothing.
func TestAnswerRecordsEveryReasonWithItsAuthor(t *testing.T) {
	cases := []struct {
		reason  approval.Reason
		outcome string
		actor   spine.ActorType
	}{
		{approval.ReasonAllowed, "allow", spine.ActorHuman},
		{approval.ReasonDenied, "deny", spine.ActorHuman},
		{approval.ReasonExpired, "deny", spine.ActorSystem},
		{approval.ReasonCancelled, "deny", spine.ActorSystem},
		{approval.ReasonDisconnected, "deny", spine.ActorSystem},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			ctx := context.Background()
			log := spine.NewMemoryLog()
			sink := spinesink.NewApproval(log, "run-1")
			if err := sink.Answer(ctx, spinesink.Answer{RequestID: "r", Action: "shell", Reason: tc.reason, Client: "c1"}); err != nil {
				t.Fatal(err)
			}
			evs, _ := log.Read(ctx, spine.Query{Stream: "run-1"})
			if len(evs) != 1 || evs[0].Type != "approval.decision" {
				t.Fatalf("events = %+v", evs)
			}
			if evs[0].Actor != tc.actor {
				t.Fatalf("actor = %s, want %s", evs[0].Actor, tc.actor)
			}
			p := evs[0].Payload
			if p["outcome"] != tc.outcome || p["reason"] != string(tc.reason) || p["client"] != "c1" || p["requestId"] != "r" {
				t.Fatalf("payload = %v", p)
			}
		})
	}

	for _, bad := range []approval.Reason{"", "timeout", "Allowed", "allowed ", "approved", "denied\n"} {
		log := spine.NewMemoryLog()
		sink := spinesink.NewApproval(log, "run-1")
		if err := sink.Answer(context.Background(), spinesink.Answer{RequestID: "r", Reason: bad}); err == nil {
			t.Errorf("reason %q was accepted", bad)
		}
		if evs, _ := log.Read(context.Background(), spine.Query{Stream: "run-1"}); len(evs) != 0 {
			t.Errorf("reason %q wrote %d events", bad, len(evs))
		}
	}
}

// TestAnswerDoesNotRecordFeedbackText: the feedback goes to the model; the record says only
// that some was given.
func TestAnswerDoesNotRecordFeedbackText(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewApproval(log, "run-1")
	_ = sink.Answer(ctx, spinesink.Answer{RequestID: "r", Reason: approval.ReasonDenied, FeedbackPresent: true})
	evs, _ := log.Read(ctx, spine.Query{Stream: "run-1"})
	if evs[0].Payload["feedbackPresent"] != true {
		t.Fatalf("payload = %v", evs[0].Payload)
	}
}
