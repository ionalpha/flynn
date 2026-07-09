package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/spine"
)

// TestAppendProvenanceRoundTrip proves the emit side writes exactly what the verify
// side reads: appendProvenance records a declaration onto a run's stream that
// chain.ProvenanceOf recovers from the same stream, so `flynn spine verify` surfaces an
// external run's tier mix from its sealed record.
func TestAppendProvenanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const stream = "run/provenance"
	declared := externalProvenance{
		harness:    "codex",
		attested:   3,
		nativeRate: 0.25,
		drift:      map[string]int{"no-native-tools": 2},
	}
	if err := appendProvenance(ctx, store.Log(), stream, declared); err != nil {
		t.Fatalf("appendProvenance: %v", err)
	}

	events, err := store.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	p, ok := chain.ProvenanceOf(events)
	if !ok {
		t.Fatal("appended declaration was not readable back from the stream")
	}
	if p.Harness != "codex" || p.Effects != chain.TierEnforced || p.Reasoning != chain.TierUnobserved || p.Replayable {
		t.Fatalf("round-tripped provenance wrong: %+v", p)
	}
	// Through the real store, so the tally, the rate, and the drift map are read back off
	// persisted bytes rather than the in-memory values the producer wrote. Each arrives
	// through JSON, where an int becomes a float64 and a map becomes map[string]any.
	if p.AttestedEvents != 3 {
		t.Fatalf("attested events = %d, want 3", p.AttestedEvents)
	}
	if p.NativeToolRate != 0.25 {
		t.Fatalf("native tool rate = %v, want 0.25", p.NativeToolRate)
	}
	if p.Drift["no-native-tools"] != 2 {
		t.Fatalf("drift = %v, want no-native-tools x2", p.Drift)
	}
}

// TestAppendProvenanceOmitsEmptyDrift proves a run whose harness honored the contract
// records no drift key at all, rather than an empty map a reader might puzzle over.
func TestAppendProvenanceOmitsEmptyDrift(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const stream = "run/clean"
	if err := appendProvenance(ctx, store.Log(), stream, externalProvenance{harness: "codex"}); err != nil {
		t.Fatalf("appendProvenance: %v", err)
	}
	events, err := store.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := chain.ProvenanceOf(events)
	if !ok {
		t.Fatal("declaration not readable")
	}
	if len(p.Drift) != 0 {
		t.Errorf("a compliant run must record no drift, got %v", p.Drift)
	}
	if _, present := events[0].Payload[chain.ProvenanceDriftKey]; present {
		t.Error("the drift key must be absent, not an empty map")
	}
}

// TestAttestedSinkRoundTrip proves the emit side writes what the verify side reads: an
// event recorded through the sink comes back off the store as the harness's line, at the
// attested tier, digested whole.
func TestAttestedSinkRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const stream = "run/attested"
	const line = `{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}`
	sink := &attestedSink{log: store.Log(), stream: stream}
	if err := sink.Record(ctx, externagent.Event{
		Kind: externagent.EventText,
		Tier: externagent.TierAttested,
		Raw:  json.RawMessage(line),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	events, err := store.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	got := chain.AttestedEventsOf(events)
	if len(got) != 1 {
		t.Fatalf("read %d attested event(s), want 1", len(got))
	}
	if got[0].Kind != "text" || got[0].Tier != chain.TierAttested {
		t.Errorf("event = %+v, want a text event at the attested tier", got[0])
	}
	if got[0].Raw != line {
		t.Errorf("raw = %s, want the harness's line verbatim", got[0].Raw)
	}
	sum := sha256.Sum256([]byte(line))
	if got[0].Digest != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %s, want the SHA-256 of the line", got[0].Digest)
	}
	if got[0].Bytes != len(line) || got[0].Truncated {
		t.Errorf("a short line was recorded as %d byte(s), truncated=%v", got[0].Bytes, got[0].Truncated)
	}
}

// TestAttestedSinkTruncatesLargeLines proves a harness cannot inflate the record by
// echoing a large tool result into its stream: the line is inlined up to the limit and
// digested whole, so the record stays bounded and a verifier holding the harness's own
// log can still match the truncated line against the one it came from.
func TestAttestedSinkTruncatesLargeLines(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const stream = "run/big"
	line := `{"text":"` + strings.Repeat("x", 2*chain.AttestedRawLimit) + `"}`
	sink := &attestedSink{log: store.Log(), stream: stream}
	if err := sink.Record(ctx, externagent.Event{Kind: externagent.EventText, Tier: externagent.TierAttested, Raw: json.RawMessage(line)}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	events, err := store.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	got := chain.AttestedEventsOf(events)[0]
	if len(got.Raw) != chain.AttestedRawLimit {
		t.Errorf("inlined %d byte(s), want the %d-byte limit", len(got.Raw), chain.AttestedRawLimit)
	}
	if !got.Truncated {
		t.Error("a truncated line must say so")
	}
	if got.Bytes != len(line) {
		t.Errorf("recorded length = %d, want the whole line's %d", got.Bytes, len(line))
	}
	sum := sha256.Sum256([]byte(line))
	if got.Digest != hex.EncodeToString(sum[:]) {
		t.Error("the digest must cover the whole line, not the truncated prefix")
	}
}

// TestAttestedDeclarationMatchesRecordedEvents is the invariant the sealed record's
// headline number rests on, checked across the two sides that produce it: the tally the
// declaration publishes, and the sink that writes the events. A reader who trusts "N
// events attested" must be able to read N events.
func TestAttestedDeclarationMatchesRecordedEvents(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const stream = "run/invariant"
	ea, err := newExternalAgent("codex", t.TempDir())
	if err != nil {
		t.Fatalf("newExternalAgent: %v", err)
	}
	recordAttestedEvents(ea, store.Log(), stream)

	sink := &attestedSink{log: store.Log(), stream: stream}
	lines := []string{`{"type":"thread.started"}`, `{"type":"item.completed"}`, `{"type":"turn.completed"}`}
	for _, line := range lines {
		if rerr := sink.Record(ctx,
			externagent.Event{Kind: externagent.EventProgress, Tier: externagent.TierAttested, Raw: json.RawMessage(line)}); rerr != nil {
			t.Fatalf("record: %v", rerr)
		}
	}
	// Nothing was lost on the way to the record, so the count the declaration is about to
	// publish is the count the record carries.
	if lost, lerr := unrecordedAttested(ea); lost != 0 {
		t.Fatalf("unrecorded = %d (%v)", lost, lerr)
	}
	// The declaration publishes the tally the host observed; here it is the lines recorded.
	if err := appendProvenance(ctx, store.Log(), stream, externalProvenance{harness: "codex", attested: len(lines)}); err != nil {
		t.Fatalf("appendProvenance: %v", err)
	}

	events, err := store.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.VerifyAttestation(events); err != nil {
		t.Fatalf("a run's declaration disagreed with the account it recorded: %v", err)
	}
}
