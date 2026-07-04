package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/ionalpha/flynn/spine"
)

// The retention vocabulary: the lifecycle events the storage tier emits when it relocates
// sealed history between tiers. Tiering moves where a payload body is stored, never what
// the log recorded - a body is addressed by its own hash and the envelope commits to that
// hash, so relocation changes nothing the tree signed. To keep the move from being a
// silent mutation, every relocation is itself an event on a reserved stream: the action
// that thins the hot tier is part of the same append-only, verifiable record it operates
// on. These string values are the wire contract a producer writes and a reader parses; an
// integration test asserts the producer agrees with them, so the two cannot drift.
const (
	// RetentionStream is the reserved stream carrying tier-lifecycle events. It is an
	// ordinary spine stream - ordered, checkpointable, verifiable like any other - named
	// so it never collides with a run or resource stream.
	RetentionStream = "flynn.retention"

	// RetentionArchived records that a set of sealed payload bodies was relocated from the
	// hot tier to a colder one. Its payload names the action, the destination tier, the
	// counts of bodies and bytes, and a manifest digest committing to exactly which bodies
	// moved (see RetentionManifest).
	RetentionArchived = "retention.archived"
)

// Retention payload keys. The value at RetentionKeyManifest is the checksum that makes the
// event tamper-evident about its own effect: it commits to the precise set of content ids
// the action moved, so a record cannot claim to have archived one set while having moved
// another.
const (
	RetentionKeyAction    = "action"
	RetentionKeyTier      = "tier"
	RetentionKeyMoved     = "moved"
	RetentionKeyHotBytes  = "hot_bytes"
	RetentionKeyWarmBytes = "warm_bytes"
	RetentionKeyManifest  = "manifest"
)

// The enumerable values RetentionKeyAction and RetentionKeyTier take. Archive is the
// hot-to-warm relocation; further actions (export to cold, verify) extend this vocabulary
// as those tiers land.
const (
	RetentionActionArchive = "archive"
	RetentionTierWarm      = "warm"
)

// RetentionManifest is the digest committing to the exact set of content ids a retention
// action moved: the SHA-256 over the ids sorted and newline-delimited. Producer and
// verifier both compute the manifest through this one function, so the digest a record
// carries cannot silently drift from the bodies it claims to cover, and it is independent
// of the order the mover happened to visit the bodies. An empty set yields the digest of
// the empty string, which never appears in a stored event because a no-op archival records
// nothing.
func RetentionManifest(contentIDs []string) string {
	ids := append([]string(nil), contentIDs...)
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RetentionRecord is the decoded content of a RetentionArchived event: what a reader
// learns about one tiering action without re-parsing raw payload keys. Moved is the number
// of bodies relocated; HotBytes is the original (uncompressed) hot bytes reclaimed;
// WarmBytes is the compressed bytes the warm tier gained; Manifest is the RetentionManifest
// digest of the moved set.
type RetentionRecord struct {
	Action    string
	Tier      string
	Moved     int64
	HotBytes  int64
	WarmBytes int64
	Manifest  string
}

// DecodeRetention reads a RetentionArchived event's payload into a RetentionRecord,
// returning ok=false for any other event type. Integer fields are read tolerantly across
// the integer and float shapes a CBOR or JSON round trip can produce (see intField), so a
// record decodes whether its events came straight from the canonical bytes or back through
// a store that serialized them as JSON.
func DecodeRetention(e spine.Event) (RetentionRecord, bool) {
	if e.Type != RetentionArchived {
		return RetentionRecord{}, false
	}
	rec := RetentionRecord{
		Action:   stringField(e.Payload, RetentionKeyAction),
		Tier:     stringField(e.Payload, RetentionKeyTier),
		Manifest: stringField(e.Payload, RetentionKeyManifest),
	}
	rec.Moved, _ = intField(e.Payload, RetentionKeyMoved)
	rec.HotBytes, _ = intField(e.Payload, RetentionKeyHotBytes)
	rec.WarmBytes, _ = intField(e.Payload, RetentionKeyWarmBytes)
	return rec, true
}

// stringField reads a string-valued payload field, returning "" when it is absent or not a
// string.
func stringField(payload map[string]any, key string) string {
	if s, ok := payload[key].(string); ok {
		return s
	}
	return ""
}
