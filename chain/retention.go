package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/ionalpha/flynn/spine"
)

// The retention vocabulary: lifecycle events the storage tier emits when it moves
// sealed history between tiers. A body is addressed by its own hash and the
// envelope commits to that hash, so relocation changes nothing the tree signed.
// Each relocation is itself an event on a reserved stream, so thinning the hot
// tier is part of the verifiable record. These strings are the wire contract; an
// integration test asserts the producer agrees with them.
const (
	// RetentionStream carries tier-lifecycle events. It is an ordinary spine stream
	// named so it never collides with a run or resource stream.
	RetentionStream = "flynn.retention"

	// RetentionArchived records sealed payload bodies moved from the hot tier to a
	// colder one: the action, destination tier, body and byte counts, and a
	// [RetentionManifest] digest of exactly which bodies moved.
	RetentionArchived = "retention.archived"
)

// Retention payload keys. RetentionKeyManifest commits to the exact content ids
// moved, so a record cannot claim one set while having moved another.
const (
	RetentionKeyAction    = "action"
	RetentionKeyTier      = "tier"
	RetentionKeyMoved     = "moved"
	RetentionKeyHotBytes  = "hot_bytes"
	RetentionKeyWarmBytes = "warm_bytes"
	RetentionKeyManifest  = "manifest"
)

// The values RetentionKeyAction and RetentionKeyTier take. Archive is the
// hot-to-warm relocation.
const (
	RetentionActionArchive = "archive"
	RetentionTierWarm      = "warm"
)

// RetentionManifest is the SHA-256 over the content ids sorted and
// newline-delimited, so it does not depend on visit order. Producer and verifier
// share this one function. An empty set yields the digest of the empty string,
// which no stored event carries because a no-op archival records nothing.
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

// RetentionRecord is the decoded [RetentionArchived] payload. Moved counts bodies
// relocated; HotBytes is the uncompressed bytes reclaimed; WarmBytes is the
// compressed bytes the warm tier gained; Manifest is the [RetentionManifest] of
// the moved set.
type RetentionRecord struct {
	Action    string
	Tier      string
	Moved     int64
	HotBytes  int64
	WarmBytes int64
	Manifest  string
}

// DecodeRetention returns ok=false for any other event type. Integers are read
// across the integer and float shapes a CBOR or JSON round trip produces.
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

// stringField returns "" when the field is absent or not a string.
func stringField(payload map[string]any, key string) string {
	if s, ok := payload[key].(string); ok {
		return s
	}
	return ""
}
