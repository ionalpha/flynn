package sqlite

// A large payload is stored once in `blobs` under its SHA-256, and the event
// row keeps the id. Readers get identical bytes either way, so the event the
// Merkle tree commits to is unchanged. Identical bodies share a row, and the
// warm tier relocates bodies by id.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// defaultPayloadBlobThreshold sits well above control-plane event sizes, so
// only large bodies such as tool output move out.
const defaultPayloadBlobThreshold = 4096

// resolvePayloadStorage returns ("", id) for an externalized body, bumping
// the refcount on a duplicate, or (raw, "") inline. It runs in the caller's
// transaction so blob and event commit together.
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

// PayloadBlobStats reports distinct bodies, their bytes and event references;
// distinct over referenced is the dedup ratio.
func (s *Store) PayloadBlobStats(ctx context.Context) (distinct, bytes, refs int64, err error) {
	err = s.reads().QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0), COALESCE(SUM(refs), 0) FROM blobs`).
		Scan(&distinct, &bytes, &refs)
	return distinct, bytes, refs, err
}
