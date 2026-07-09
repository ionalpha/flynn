package chain

import (
	"fmt"

	"github.com/ionalpha/flynn/spine"
)

// The attested-event vocabulary a record carries alongside its provenance
// declaration. An external harness reports its own episode as a stream of lines; the
// run enforces none of them (the effects it enforces cross the dispatch waist and are
// recorded there, at TierEnforced). Repeating the harness's account verbatim is what
// lets a reader tell the two apart: an enforced effect is something the run admitted
// and ran, an attested event is something the harness said it did. Without the raw
// lines the record could only say how many claims the harness made, not what they were,
// which is an audit trail of a number. These string values are the wire contract a
// producer writes and this package reads, so they must not change.
const (
	// AttestedRecorded is one event the external harness reported about its own episode,
	// kept verbatim and marked as the harness's claim rather than the run's observation.
	AttestedRecorded = "provenance.attested"

	// AttestedKindKey is what the harness's line projected to (text, progress, usage,
	// error, done, bridge_call, native_command).
	AttestedKindKey = "kind"
	// AttestedTierKey is the provenance tier the event was projected at. It is
	// TierAttested for every event on this stream; recording it keeps a reader from
	// having to know that, and leaves room for a harness that can be observed more
	// strongly.
	AttestedTierKey = "tier"
	// AttestedRawKey is the harness's original output line, verbatim, up to
	// AttestedRawLimit bytes.
	AttestedRawKey = "raw"
	// AttestedDigestKey is the SHA-256 of the WHOLE raw line, hex encoded, whether or not
	// the line was truncated. A verifier holding the harness's own log can therefore match
	// a truncated record against the full line it came from.
	AttestedDigestKey = "raw_sha256"
	// AttestedBytesKey is the length of the whole raw line in bytes, before truncation.
	AttestedBytesKey = "raw_bytes"
	// AttestedTruncatedKey is true when AttestedRawKey holds a prefix rather than the
	// whole line. Absent means the raw line is complete.
	AttestedTruncatedKey = "truncated"

	// AttestedRawLimit is how many bytes of a harness line the record inlines. A line can
	// carry a whole tool result echoed back (the episode reader raises its own ceiling to
	// megabytes), and a record is read into memory to verify it, so the raw account is
	// bounded and the digest carries the rest of the line's identity.
	AttestedRawLimit = 4096
)

// AttestedEvent is one event an external harness reported about its own episode, as
// the record keeps it. The run did not observe the work behind it; the record repeats
// the claim, marked as a claim.
type AttestedEvent struct {
	// Kind is what the harness's line projected to (text, progress, bridge_call, ...).
	Kind string
	// Tier is the provenance tier the event carries, always TierAttested today.
	Tier string
	// Raw is the harness's original line, verbatim, truncated to AttestedRawLimit bytes.
	Raw string
	// Digest is the hex SHA-256 of the whole line before truncation.
	Digest string
	// Bytes is the length of the whole line before truncation.
	Bytes int
	// Truncated reports whether Raw is a prefix of the line rather than all of it.
	Truncated bool
}

// AttestedEventsOf returns the harness's attested events recorded on a run's stream, in
// stream order. A native run records none. The events must already be authentic and in
// order (verify the record first), the same precondition the governance and ground-truth
// verifiers assume.
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

// VerifyAttestation checks that a run's provenance declaration agrees with the attested
// events recorded next to it: the declaration's count is what verify prints, and the
// events are what a reader inspects, so a record whose count exceeds its events is
// claiming an account it does not carry (lines dropped after the fact), and one whose
// events exceed its count is carrying claims the declaration never owned. A native run
// declares no provenance and records no attested events, and passes.
//
// It assumes the record already verified: authenticity is what makes the counts worth
// comparing.
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
