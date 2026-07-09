package chain

import (
	"testing"

	"github.com/ionalpha/flynn/spine"
)

// attestedEvent builds one recorded harness event on a stream.
func attestedEvent(kind, raw string) spine.Event {
	return spine.Event{Type: AttestedRecorded, Payload: map[string]any{
		AttestedKindKey:   kind,
		AttestedTierKey:   TierAttested,
		AttestedRawKey:    raw,
		AttestedDigestKey: "deadbeef",
		AttestedBytesKey:  len(raw),
	}}
}

// declaration builds a provenance declaration claiming attested harness events.
func declaration(attested int) spine.Event {
	return spine.Event{Type: ProvenanceDeclared, Payload: map[string]any{
		ProvenanceHarnessKey:  "codex",
		ProvenanceAttestedKey: attested,
	}}
}

// TestAttestedEventsOf reads the harness's own account off a stream, in stream order,
// ignoring the events the run enforced at the waist.
func TestAttestedEventsOf(t *testing.T) {
	events := []spine.Event{
		{Type: "dispatch.start"},
		attestedEvent("text", `{"msg":"first"}`),
		{Type: "dispatch.end"},
		attestedEvent("bridge_call", `{"tool":"read_file"}`),
		declaration(2),
	}
	got := AttestedEventsOf(events)
	if len(got) != 2 {
		t.Fatalf("read %d attested event(s), want 2", len(got))
	}
	if got[0].Kind != "text" || got[0].Raw != `{"msg":"first"}` {
		t.Errorf("first event = %+v, want the harness's text line verbatim", got[0])
	}
	if got[1].Kind != "bridge_call" {
		t.Errorf("second event kind = %q, want bridge_call", got[1].Kind)
	}
	if got[0].Tier != TierAttested {
		t.Errorf("tier = %q, want %q", got[0].Tier, TierAttested)
	}
	if got[0].Bytes != len(`{"msg":"first"}`) {
		t.Errorf("bytes = %d, want the whole line's length", got[0].Bytes)
	}
	if got[0].Truncated {
		t.Error("a short line must not be marked truncated")
	}
}

// TestAttestedEventsOfNative proves a native run carries no attested events: it observed
// everything it did, so there is no second account to keep.
func TestAttestedEventsOfNative(t *testing.T) {
	if got := AttestedEventsOf([]spine.Event{{Type: "dispatch.start"}, {Type: OutcomeRecorded}}); got != nil {
		t.Fatalf("a native stream carried %d attested event(s)", len(got))
	}
}

// TestAttestedEventsOfAcrossEncodings proves a truncated line's flags and length survive
// the two encodings one record is read back from: the durable store's JSON, where every
// number is a float64, and the sealed record's canonical CBOR, where it is a uint64.
func TestAttestedEventsOfAcrossEncodings(t *testing.T) {
	cases := map[string]any{
		"in memory":                              4096,
		"through JSON (durable store)":           float64(4096),
		"through canonical CBOR (sealed record)": uint64(4096),
	}
	for name, size := range cases {
		t.Run(name, func(t *testing.T) {
			e := spine.Event{Type: AttestedRecorded, Payload: map[string]any{
				AttestedKindKey:      "text",
				AttestedBytesKey:     size,
				AttestedTruncatedKey: true,
			}}
			got := AttestedEventsOf([]spine.Event{e})
			if len(got) != 1 {
				t.Fatalf("read %d attested event(s), want 1", len(got))
			}
			if got[0].Bytes != 4096 {
				t.Errorf("bytes = %d, want 4096", got[0].Bytes)
			}
			if !got[0].Truncated {
				t.Error("a truncated line must read back as truncated")
			}
		})
	}
}

// TestVerifyAttestation proves the invariant the declaration's headline number rests on:
// the count it publishes is the number of harness events the record actually carries. A
// record that claims more is quoting an account it cannot show, and one that carries more
// is holding claims the declaration never owned.
func TestVerifyAttestation(t *testing.T) {
	cases := []struct {
		name    string
		events  []spine.Event
		wantErr bool
	}{
		{
			name:   "native run declares and records nothing",
			events: []spine.Event{{Type: "dispatch.start"}},
		},
		{
			name:   "declaration agrees with the events",
			events: []spine.Event{attestedEvent("text", "a"), attestedEvent("done", "b"), declaration(2)},
		},
		{
			name:    "declaration claims more than the record carries",
			events:  []spine.Event{attestedEvent("text", "a"), declaration(4)},
			wantErr: true,
		},
		{
			name:    "record carries more than the declaration claims",
			events:  []spine.Event{attestedEvent("text", "a"), attestedEvent("text", "b"), declaration(1)},
			wantErr: true,
		},
		{
			name:    "attested events with no declaration",
			events:  []spine.Event{attestedEvent("text", "a")},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyAttestation(tc.events)
			if tc.wantErr && err == nil {
				t.Fatal("a record whose account does not match its declaration passed")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a consistent record failed: %v", err)
			}
		})
	}
}
