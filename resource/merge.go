package resource

import (
	"fmt"

	"github.com/ionalpha/flynn/spine"
)

// MergeOutcome reports what a Merge did to the local record.
type MergeOutcome int

const (
	// MergeApplied means the remote record won and replaced or first inserted the
	// local one.
	MergeApplied MergeOutcome = iota
	// MergeIgnored means the local record won and the remote was discarded.
	MergeIgnored
	// MergeUnchanged means the remote write was already present, an idempotent no-op.
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

// MergeResult is what a merge did and the resource now in the record's slot.
type MergeResult struct {
	Outcome  MergeOutcome
	Resource Resource
}

// Resolve picks the winner between an incoming replicated record and the local one
// with the same ID. It is deterministic and commutative, so every instance converges
// whatever the arrival order. The rules, in order:
//
//  1. The same write (equal UpdatedHLC and LastWriterID) is a no-op.
//  2. A human-authored write outranks an automated one regardless of clocks, so a
//     later agent write never overwrites human intent.
//  3. Last-writer-wins by UpdatedHLC, LastWriterID breaking an exact tie.
//
// Tombstones carry the same envelope, so a later delete removes and a later write
// after a delete resurrects.
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

// provenanceRank ranks a human write above agent and system writes, which tie, so
// among automation last-writer-wins alone decides.
func provenanceRank(a spine.ActorType) int {
	if a == spine.ActorHuman {
		return 1
	}
	return 0
}

// ValidateForMerge checks that a replicated record carries the full envelope Merge
// needs. No Stamper assigns identity on the merge path, so this keeps a half-built
// local resource out of it. Backends call it before resolving.
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

// MergeEvent builds the spine event for a merged r. Its payload is the winning
// post-image verbatim and it keeps r's origin and writer actor, so folding the stream
// reproduces the merge on any backend.
func MergeEvent(r Resource) (spine.AppendInput, error) {
	p, err := encodePayload(r)
	if err != nil {
		return spine.AppendInput{}, err
	}
	return spine.AppendInput{
		Stream:           ResourceStream,
		Type:             EvMerged,
		Actor:            writerActor(r.WriterActor),
		RawPayload:       p,
		OriginInstanceID: r.OriginInstanceID,
	}, nil
}
