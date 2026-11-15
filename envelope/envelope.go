// Package envelope defines the sync envelope: the five fields every replicated
// record carries (SyncVersion, OriginInstanceID, UpdatedHLC, LastWriterID, Deleted)
// and the stamping rules over them. Fleet merge is correct only if every writer
// applies the same rules: the last-writer-wins key is (UpdatedHLC, LastWriterID),
// optimistic concurrency is a compare-and-set on SyncVersion, and a delete is a
// tombstone that still syncs. The rules live here once, so the state and resource
// stampers cannot drift apart.
//
// The ops touch only the envelope and take the writer and HLC time as arguments, so
// a stamp produces the same envelope whichever store applies it.
package envelope

import (
	"encoding/json"

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/spine"
)

// Envelope is the sync and concurrency metadata of every replicated record. It has
// no JSON tags because records serialize with Go field names and embedding inlines
// them, so embedding it changes no wire format.
type Envelope struct {
	// SyncVersion is bumped on every write and is 1 on create. A write carrying the
	// version it read fails if the stored version moved; zero writes unconditionally.
	// It is local-only; the HLC orders writes across instances.
	SyncVersion int64
	// OriginInstanceID is the instance that created the record, kept across updates
	// so a merge can attribute provenance.
	OriginInstanceID string
	// UpdatedHLC is the hybrid-logical-clock time of the last write, the first half of
	// the last-writer-wins key.
	UpdatedHLC hlc.Time
	// LastWriterID is the instance that made the last write.
	LastWriterID string
	// Deleted marks a tombstone. It keeps its envelope and syncs, so a stale replica
	// cannot resurrect the record. Reads filter tombstones out.
	Deleted bool
}

// CAS reports whether a write expecting version expected may replace stored. Zero
// writes unconditionally; otherwise it must equal the stored SyncVersion. With
// stored nil only an unconditional write passes, because a non-zero expectation
// names a record that does not exist. Callers map false to their ErrConflict.
func CAS(expected int64, stored *Envelope) bool {
	if stored == nil {
		return expected == 0
	}
	return expected == 0 || expected == stored.SyncVersion
}

// StampCreate initializes e for a first write: version 1, live, the writer as last
// writer, and the writer as origin unless already set, so a synced create keeps its
// origin.
func StampCreate(e *Envelope, writerID string, now hlc.Time) {
	e.SyncVersion = 1
	if e.OriginInstanceID == "" {
		e.OriginInstanceID = writerID
	}
	e.LastWriterID = writerID
	e.UpdatedHLC = now
	e.Deleted = false
}

// StampUpdate stamps e as the successor of prev: origin carried forward and the
// version bumped from the stored one, never from what the caller sent. The caller
// checks CAS first. Deleted is kept as the caller set it, so an update can
// resurrect a tombstone.
func StampUpdate(e *Envelope, prev Envelope, writerID string, now hlc.Time) {
	e.OriginInstanceID = prev.OriginInstanceID
	e.SyncVersion = prev.SyncVersion + 1
	e.LastWriterID = writerID
	e.UpdatedHLC = now
}

// StampBump advances a live record the caller holds in place, with no CAS and no
// field carry-over.
func StampBump(e *Envelope, writerID string, now hlc.Time) {
	e.SyncVersion++
	e.LastWriterID = writerID
	e.UpdatedHLC = now
}

// StampTombstone marks e deleted and bumps it, so the tombstone's fresh
// (UpdatedHLC, LastWriterID) wins over any stale live copy.
func StampTombstone(e *Envelope, writerID string, now hlc.Time) {
	e.Deleted = true
	StampBump(e, writerID, now)
}

// EventInput builds the spine append for a stamped mutation, with the payload
// carried verbatim and the writer as origin. It is the one definition of the
// record-to-event shape, so the state and resource streams cannot diverge. The
// payload arrives encoded because each package owns its layout.
func EventInput(stream, typ string, actor spine.ActorType, originInstanceID string, payload json.RawMessage) spine.AppendInput {
	return spine.AppendInput{
		Stream:           stream,
		Type:             typ,
		Actor:            actor,
		RawPayload:       payload,
		OriginInstanceID: originInstanceID,
	}
}
