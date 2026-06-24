package resource

import (
	"fmt"

	"github.com/ionalpha/flynn/spine"
)

// MergeOutcome reports what a Merge did to the local record.
type MergeOutcome int

const (
	// MergeApplied means the remote record won and replaced (or first-inserted) the
	// local one.
	MergeApplied MergeOutcome = iota
	// MergeIgnored means the local record won and the remote was discarded as older
	// or lower-precedence. The local state is unchanged.
	MergeIgnored
	// MergeUnchanged means the remote was already present (same write), so the apply
	// was an idempotent no-op.
	MergeUnchanged
)

func (o MergeOutcome) String() string {
	switch o {
	case MergeApplied:
		return "applied"
	case MergeIgnored:
		return "ignored"
	case MergeUnchanged:
		return "unchanged"
	default:
		return "unknown"
	}
}

// MergeResult is the outcome of applying a replicated resource: what happened and
// the resource that now holds the record's slot (the winner, or the unchanged
// local record).
type MergeResult struct {
	Outcome  MergeOutcome
	Resource Resource
}

// Resolve decides a cross-instance conflict between an incoming replicated record
// and the current local one for the same ID, returning the winner and whether it
// is the incoming record. It is the pure heart of Merge, deterministic and
// commutative so every instance converges on the same result no matter the order
// records arrive. The rules, in order:
//
//  1. Idempotence: the same write (equal UpdatedHLC and LastWriterID) is a no-op,
//     so re-delivering a record never changes anything.
//  2. Provenance precedence: a human-authored write outranks an automated one
//     regardless of clocks, because human intent should not be silently overwritten
//     by a later agent or system write.
//  3. Last-writer-wins by UpdatedHLC, with LastWriterID as the deterministic
//     tiebreak when two writes share an exact HLC.
//
// Tombstones carry the same envelope as live writes, so a delete and a write are
// ordered by the very same key: a higher-HLC delete removes, and a higher-HLC
// write after a delete intentionally resurrects.
func Resolve(incoming, current Resource) (winner Resource, takeIncoming bool) {
	if incoming.UpdatedHLC == current.UpdatedHLC && incoming.LastWriterID == current.LastWriterID {
		return current, false // same write already applied
	}
	if pi, pc := provenanceRank(incoming.WriterActor), provenanceRank(current.WriterActor); pi != pc {
		if pi > pc {
			return incoming, true
		}
		return current, false
	}
	if cmp := incoming.UpdatedHLC.Compare(current.UpdatedHLC); cmp != 0 {
		if cmp > 0 {
			return incoming, true
		}
		return current, false
	}
	if incoming.LastWriterID > current.LastWriterID {
		return incoming, true
	}
	return current, false
}

// provenanceRank scores authorship for merge precedence. Only the human-versus-
// automation distinction matters: a human write outranks an agent or system write.
// Agent and system are equal (both automation), so among them last-writer-wins
// alone decides.
func provenanceRank(a spine.ActorType) int {
	if a == spine.ActorHuman {
		return 1
	}
	return 0
}

// ValidateForMerge checks that a replicated record carries the full envelope Merge
// relies on. A record produced by a real write always has these; rejecting a record
// without them guards against feeding a half-built local resource into the merge
// path (where no Stamper assigns identity). Backends call it before resolving.
func ValidateForMerge(r Resource) error {
	switch {
	case r.ID == "":
		return fmt.Errorf("%w: merge requires a stable ID", ErrInvalid)
	case r.APIVersion == "" || r.Kind == "" || r.Name == "":
		return fmt.Errorf("%w: merge requires APIVersion, Kind and Name", ErrInvalid)
	case r.UpdatedHLC.IsZero():
		return fmt.Errorf("%w: merge requires an UpdatedHLC", ErrInvalid)
	case r.OriginInstanceID == "" || r.LastWriterID == "":
		return fmt.Errorf("%w: merge requires OriginInstanceID and LastWriterID", ErrInvalid)
	}
	return nil
}

// MergeEvent builds the spine event recording that r was applied by cross-instance
// merge. Its payload is the winning post-image verbatim, so folding the stream
// reproduces the merged state on any backend, and it preserves r's provenance
// (origin instance and writer actor) rather than restamping it locally.
func MergeEvent(r Resource) (spine.AppendInput, error) {
	p, err := encodeResource(r)
	if err != nil {
		return spine.AppendInput{}, err
	}
	return spine.AppendInput{
		Stream:           ResourceStream,
		Type:             EvMerged,
		Actor:            writerActor(r.WriterActor),
		Payload:          map[string]any{payloadKey: p},
		OriginInstanceID: r.OriginInstanceID,
	}, nil
}
