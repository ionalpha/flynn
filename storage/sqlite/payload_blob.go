package sqlite

// Content-addressed payload bodies. A large event payload is written once into the
// `blobs` table under the SHA-256 of its stored bytes and the event row keeps only that
// content id in `payload_blob`; a small payload stays inline in `events.payload` and its
// `payload_blob` is empty. The read path (eventsReadSQL) rehydrates the inline or blob
// body in one query, so a reader sees the exact same payload bytes either way and the
// canonical event the Merkle tree commits to is unchanged - externalizing a body moves
// where it is stored, never what was recorded.
//
// Because a body is keyed by its own hash, two events carrying identical bodies share one
// row: the append-only log's hot footprint grows with distinct bodies, not with their
// repetition. This is the separation the warm/cold storage tiers build on - the bodies
// are the bulk of the bytes and become an independently relocatable set addressed by
// content id, while the event rows, tiles, and checkpoints stay small and hot.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// defaultPayloadBlobThreshold is the stored-byte length at or above which a payload is
// externalized into `blobs` instead of kept inline. It sits well above the size of a
// control-plane event (a heartbeat, a status change) so the common small events stay
// inline and only the large bodies - tool outputs, model replies - are separated out.
const defaultPayloadBlobThreshold = 4096

// resolvePayloadStorage decides how one event's stored payload bytes are held. When
// externalization is enabled (threshold > 0) and raw is at least threshold bytes, it
// writes raw into `blobs` keyed by its SHA-256 (incrementing the reference count when the
// body is already present, so identical bodies dedupe) and returns an empty inline column
// with that content id. Otherwise the body stays inline: it returns raw as the column
// value and an empty content id. It runs inside the caller's write transaction so the
// blob row and the event row that references it commit together.
func resolvePayloadStorage(ctx context.Context, tx *sql.Tx, threshold int, raw []byte) (inline string, blobID string, err error) {
	if threshold <= 0 || len(raw) < threshold {
		return string(raw), "", nil
	}
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO blobs (content_id, body, size, refs) VALUES (?, ?, ?, 1)
		 ON CONFLICT(content_id) DO UPDATE SET refs = refs + 1`,
		id, string(raw), len(raw)); err != nil {
		return "", "", fmt.Errorf("sqlite: store payload blob: %w", err)
	}
	return "", id, nil
}

// PayloadBlobStats reports the content-addressed payload store's accounting: the number
// of distinct bodies held, their total stored bytes, and the total number of event
// references to them. Distinct versus referenced is the deduplication ratio - identical
// bodies are stored once but referenced by every event that carries them - and the byte
// total is the externalized share a warm or cold tier can relocate independently of the
// event rows. It reads committed state on the read pool.
func (s *Store) PayloadBlobStats(ctx context.Context) (distinct, bytes, refs int64, err error) {
	err = s.reads().QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0), COALESCE(SUM(refs), 0) FROM blobs`).
		Scan(&distinct, &bytes, &refs)
	return distinct, bytes, refs, err
}
