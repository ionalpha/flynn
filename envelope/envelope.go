// Package envelope is the one definition of the sync envelope: the five fields
// every replicated record carries (SyncVersion, OriginInstanceID, UpdatedHLC,
// LastWriterID, Deleted) and the stamping rules over them. Fleet-merge
// correctness rests on every writer applying identical rules - the LWW key is
// (UpdatedHLC, LastWriterID), optimistic concurrency is a compare-and-set on
// SyncVersion, and a delete is a tombstone that still syncs - so the rules live
// here exactly once and the state and resource stampers are thin per-type
// wrappers that cannot drift from each other.
//
// The ops are pure: they touch only the envelope, take the writer identity and
// HLC time as arguments, and do no persistence, so the same stamp produces the
// same envelope regardless of which store applies it.
package envelope

import (
	"encoding/json"

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/spine"
)

// Envelope is the sync/concurrency metadata carried by every replicated record.
// It is embedded (state records embed it directly; the resource envelope wraps
// it with lifecycle and provenance fields), and it has no JSON tags on purpose:
// records serialize with Go field names, and embedding inlines them, so adopting
// this struct changes no wire format and replays no differently.
type Envelope struct {
	// SyncVersion is bumped on every write; 1 on create. It powers local
	// optimistic concurrency: pass the version you read and the write fails
	// if the stored version has moved (a zero SyncVersion writes
	// unconditionally). It is local-only; cross-instance ordering is the HLC.
	SyncVersion int64
	// OriginInstanceID is the instance that first created the record; preserved
	// across updates so fleet/P2P merge can attribute provenance.
	OriginInstanceID string
	// UpdatedHLC is the hybrid-logical-clock time of the last write. It orders
	// writes across instances for last-writer-wins merge (the LWW key is
	// (UpdatedHLC, LastWriterID)), where SyncVersion (local-only) cannot.
	UpdatedHLC hlc.Time
	// LastWriterID is the instance that performed the last write (distinct from
	// OriginInstanceID, the creator).
	LastWriterID string
	// Deleted marks a tombstone: a soft delete that still carries its envelope
	// so it propagates in sync, preventing a stale replica from resurrecting the
	// record. Reads filter tombstones out.
	Deleted bool
}

// CAS reports whether a write carrying the expected version may replace the
// stored envelope: a zero expected version writes unconditionally, otherwise it
// must equal the stored SyncVersion. stored is nil when no record exists yet, in
// which case only an unconditional write is allowed (a non-zero expectation
// names a record that is not there - a conflict, not a create). Callers map a
// false result to their package's ErrConflict.
func CAS(expected int64, stored *Envelope) bool {
	if stored == nil {
		return expected == 0
	}
	return expected == 0 || expected == stored.SyncVersion
}

// StampCreate initializes e for a record's first write: version 1, live, the
// writer as last writer, and the writer as origin unless the caller already
// attributed the record to another instance (a synced create keeps its origin).
func StampCreate(e *Envelope, writerID string, now hlc.Time) {
	e.SyncVersion = 1
	if e.OriginInstanceID == "" {
		e.OriginInstanceID = writerID
	}
	e.LastWriterID = writerID
	e.UpdatedHLC = now
	e.Deleted = false
}

// StampUpdate stamps e as the successor of prev: origin carried forward, the
// version bumped from the stored one (never from what the caller sent), and the
// writer recorded. The caller checks CAS first; StampUpdate itself is
// unconditional. Deleted is taken from e as the caller set it, so an update can
// resurrect a tombstone.
func StampUpdate(e *Envelope, prev Envelope, writerID string, now hlc.Time) {
	e.OriginInstanceID = prev.OriginInstanceID
	e.SyncVersion = prev.SyncVersion + 1
	e.LastWriterID = writerID
	e.UpdatedHLC = now
}

// StampBump advances e in place for an unconditional mutation of a live record
// the caller already holds (a session whose turn count moved, a counter): the
// version bumps and the writer is recorded, with no CAS and no field carry-over.
func StampBump(e *Envelope, writerID string, now hlc.Time) {
	e.SyncVersion++
	e.LastWriterID = writerID
	e.UpdatedHLC = now
}

// StampTombstone marks e deleted in place and bumps it, so the tombstone
// carries a fresh (UpdatedHLC, LastWriterID) and propagates in sync ahead of
// any stale live copy.
func StampTombstone(e *Envelope, writerID string, now hlc.Time) {
	e.Deleted = true
	StampBump(e, writerID, now)
}

// EventInput builds the spine append for a stamped mutation: the given stream
// and type, the pre-encoded payload carried verbatim, and the writing instance
// as origin. It is the one place the record-to-event shape is defined, so the
// state and resource streams cannot diverge on it. The payload arrives encoded
// because each package owns its payload layout (and its allocation profile).
func EventInput(stream, typ string, actor spine.ActorType, originInstanceID string, payload json.RawMessage) spine.AppendInput {
	return spine.AppendInput{
		Stream:           stream,
		Type:             typ,
		Actor:            actor,
		RawPayload:       payload,
		OriginInstanceID: originInstanceID,
	}
}
