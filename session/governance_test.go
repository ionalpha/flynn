package session

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/spine"
)

// TestTypeStringsMatchProducers pins the projected types to the strings their
// producers actually write, so the projection cannot silently drift from the dispatch
// waist or the sealer. The dispatch types are checked against the dispatch package
// itself; the record type is checked against the value the sealer stores.
func TestTypeStringsMatchProducers(t *testing.T) {
	if typeDispatchStart != dispatch.EventStart {
		t.Errorf("typeDispatchStart = %q, want %q", typeDispatchStart, dispatch.EventStart)
	}
	if typeDispatchEnd != dispatch.EventEnd {
		t.Errorf("typeDispatchEnd = %q, want %q", typeDispatchEnd, dispatch.EventEnd)
	}
	if typeDispatchRejected != dispatch.EventRejected {
		t.Errorf("typeDispatchRejected = %q, want %q", typeDispatchRejected, dispatch.EventRejected)
	}
	// The sealer (cmd/flynn) stores its signed record under this type on the run's
	// stream; that package is not importable here, so the literal is pinned directly.
	if typeRecordSealed != "spine.record" {
		t.Errorf("typeRecordSealed = %q, want %q", typeRecordSealed, "spine.record")
	}
}

// govPayload mirrors the shape spinesink writes for a dispatch event, so the decode
// test exercises the real payload keys rather than a hand-picked subset.
func govPayload(action string, call int64, trust, errClass string) map[string]any {
	p := map[string]any{"action": action, "call": call}
	if trust != "" {
		p["trust"] = trust
	}
	if errClass != "" {
		p["error_class"] = errClass
	}
	return p
}

func TestDecodeGovernanceEvents(t *testing.T) {
	cases := []struct {
		name       string
		se         spine.Event
		wantKind   Kind
		wantAction string
		wantCall   int64
		wantTrust  string
		wantFault  string
	}{
		{
			name:       "admitted",
			se:         spine.Event{Type: typeDispatchStart, Payload: govPayload("bash", 7, "agent", "")},
			wantKind:   KindActionAdmitted,
			wantAction: "bash", wantCall: 7, wantTrust: "agent",
		},
		{
			name:       "completed clean",
			se:         spine.Event{Type: typeDispatchEnd, Payload: govPayload("bash", 7, "agent", "")},
			wantKind:   KindActionCompleted,
			wantAction: "bash", wantCall: 7, wantTrust: "agent",
		},
		{
			name:       "completed with fault",
			se:         spine.Event{Type: typeDispatchEnd, Payload: govPayload("bash", 7, "agent", "transient")},
			wantKind:   KindActionCompleted,
			wantAction: "bash", wantCall: 7, wantTrust: "agent", wantFault: "transient",
		},
		{
			name:       "rejected",
			se:         spine.Event{Type: typeDispatchRejected, Payload: govPayload("write", 9, "model", "capability_denied")},
			wantKind:   KindActionRejected,
			wantAction: "write", wantCall: 9, wantTrust: "model", wantFault: "capability_denied",
		},
		{
			name:     "record sealed",
			se:       spine.Event{Type: typeRecordSealed, Payload: map[string]any{"record": "..."}},
			wantKind: KindRecordSealed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := fromSpine(tc.se)
			if e.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", e.Kind, tc.wantKind)
			}
			if e.Action != tc.wantAction {
				t.Errorf("Action = %q, want %q", e.Action, tc.wantAction)
			}
			if e.Call != tc.wantCall {
				t.Errorf("Call = %d, want %d", e.Call, tc.wantCall)
			}
			if e.Trust != tc.wantTrust {
				t.Errorf("Trust = %q, want %q", e.Trust, tc.wantTrust)
			}
			if e.Fault != tc.wantFault {
				t.Errorf("Fault = %q, want %q", e.Fault, tc.wantFault)
			}
		})
	}
}

// TestGovernanceCorrelationIDSurvivesJSONShape proves the correlation id decodes
// whether the log handed it back as an int64 (the in-memory log shares the map) or a
// float64 (a durable log re-encodes the payload as JSON), so pairing an admission with
// its outcome works on either backend.
func TestGovernanceCorrelationIDSurvivesJSONShape(t *testing.T) {
	asInt := fromSpine(spine.Event{Type: typeDispatchStart, Payload: map[string]any{"action": "a", "call": int64(42)}})
	asFloat := fromSpine(spine.Event{Type: typeDispatchStart, Payload: map[string]any{"action": "a", "call": float64(42)}})
	if asInt.Call != 42 || asFloat.Call != 42 {
		t.Fatalf("Call int=%d float=%d, want both 42", asInt.Call, asFloat.Call)
	}
}

func TestAsInt64(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{int64(5), 5},
		{float64(5), 5},
		{int(5), 5},
		{"5", 0},
		{nil, 0},
	}
	for _, tc := range cases {
		if got := asInt64(tc.in); got != tc.want {
			t.Errorf("asInt64(%#v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestHistoryProjectsGovernanceEvents drives the real read path: raw spine events in
// the shapes the waist and the sealer write, read back through History, arrive as
// typed session events interleaved in order with the conversation.
func TestHistoryProjectsGovernanceEvents(t *testing.T) {
	log := spine.NewMemoryLog()
	ctx := context.Background()
	const stream = "run-gov"
	appends := []spine.AppendInput{
		{Stream: stream, Type: string(KindSessionStarted), Payload: map[string]any{payloadKey: `{"kind":"session.started","text":"do the thing"}`}},
		{Stream: stream, Type: typeDispatchStart, Payload: govPayload("bash", 1, "agent", "")},
		{Stream: stream, Type: typeDispatchEnd, Payload: govPayload("bash", 1, "agent", "")},
		{Stream: stream, Type: typeDispatchRejected, Payload: govPayload("write_file", 2, "model", "capability_denied")},
		{Stream: stream, Type: typeRecordSealed, Payload: map[string]any{"record": "x"}},
	}
	for _, a := range appends {
		a.Time = time.Unix(0, 0)
		if _, err := log.Append(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := History(ctx, log, stream)
	if err != nil {
		t.Fatal(err)
	}
	want := []Kind{KindSessionStarted, KindActionAdmitted, KindActionCompleted, KindActionRejected, KindRecordSealed}
	if got := kindsOf(evs); len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i, k := range want {
		if evs[i].Kind != k {
			t.Errorf("event %d kind = %q, want %q", i, evs[i].Kind, k)
		}
	}
	if evs[3].Fault != "capability_denied" {
		t.Errorf("rejected fault = %q, want capability_denied", evs[3].Fault)
	}
}
