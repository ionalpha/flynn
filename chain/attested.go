package chain

import (
	"fmt"

	"github.com/ionalpha/flynn/spine"
)

// The attested-event vocabulary a record carries beside its provenance
// declaration. The run enforces none of an external harness's reported lines; the
// effects it enforces are recorded at TierEnforced. Repeating the harness's
// account verbatim lets a reader tell an enforced effect from a claim, and see
// what the claims were, not only how many. These strings are the wire contract
// between producer and this package and must not change.
const (
	// AttestedRecorded is one event the harness reported about itself, kept
	// verbatim as the harness's claim, not the run's observation.
	AttestedRecorded = "provenance.attested"

	// AttestedKindKey is what the line projected to (text, progress, usage, error,
	// done, bridge_call, native_command).
	AttestedKindKey = "kind"
	// AttestedTierKey is the tier the event was projected at, TierAttested today.
	// Recording it leaves room for a harness that can be observed more strongly.
	AttestedTierKey = "tier"
	// AttestedRawKey is the original line, up to AttestedRawLimit bytes.
	AttestedRawKey = "raw"
	// AttestedDigestKey is the hex SHA-256 of the whole line, truncated or not, so
	// a verifier holding the harness's log can match a truncated record to it.
	AttestedDigestKey = "raw_sha256"
	// AttestedBytesKey is the line length in bytes before truncation.
	AttestedBytesKey = "raw_bytes"
	// AttestedTruncatedKey is true when AttestedRawKey holds a prefix; absent means
	// complete.
	AttestedTruncatedKey = "truncated"

	// AttestedRawLimit is how many bytes of a line the record inlines. A line can
	// echo a whole tool result and a record is read into memory to verify, so the
	// digest carries the rest of the line's identity.
	AttestedRawLimit = 4096
)

// AttestedEvent is one event an external harness reported about itself. The run
// did not observe the work behind it.
type AttestedEvent struct {
	Kind      string
	Tier      string
	Raw       string
	Digest    string
	Bytes     int
	Truncated bool
}

// AttestedEventsOf returns the attested events in stream order; a native run has
// none. The events must already be verified and in order, as the governance and
// ground-truth verifiers assume.
func AttestedEventsOf(events []spine.Event) []AttestedEvent {
	var out []AttestedEvent
	for _, e := range events {
		if e.Type != AttestedRecorded {
			continue
		}
		a := AttestedEvent{Bytes: payloadInt(e.Payload[AttestedBytesKey])}
		a.Kind, _ = e.Payload[AttestedKindKey].(string)
		a.Tier, _ = e.Payload[AttestedTierKey].(string)
		a.Raw, _ = e.Payload[AttestedRawKey].(string)
		a.Digest, _ = e.Payload[AttestedDigestKey].(string)
		a.Truncated, _ = e.Payload[AttestedTruncatedKey].(bool)
		out = append(out, a)
	}
	return out
}

// VerifyAttestation checks that the provenance declaration's count equals the
// attested events recorded. A count above the events means lines were dropped
// after the fact; events above the count are claims the declaration never owned.
// A native run declares nothing and passes. The record must already be verified.
func VerifyAttestation(events []spine.Event) error {
	recorded := len(AttestedEventsOf(events))
	p, ok := ProvenanceOf(events)
	if !ok {
		if recorded > 0 {
			return fmt.Errorf("%d attested event(s) recorded with no provenance declaration", recorded)
		}
		return nil
	}
	if p.AttestedEvents != recorded {
		return fmt.Errorf("provenance declares %d attested event(s), record carries %d",
			p.AttestedEvents, recorded)
	}
	return nil
}
