package sqlite

// The warm tier holds payload bodies whose every referencing event is sealed
// under a checkpoint, zstd-compressed in a separate SQLite file, so the hot
// file tracks the working set. A hot miss falls back to warm.
//
// Relocation copies to warm, then deletes from hot, idempotently by content
// id: a crash between leaves the body in both, never neither, and a concurrent
// reader always finds it. Bodies are addressed by the SHA-256 the envelope
// commits to, so moving one changes nothing the Merkle tree signed, and a lost
// warm body is detectable by its id.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/internal/sqlitex"
	"github.com/ionalpha/flynn/spine"
)

// warmStore has its own file so it can sit on cheaper storage. Losing it costs
// replay of archived history, never verifiability of the log.
type warmStore struct {
	db *sql.DB
}

// warmDSNSuffix names the warm file beside the hot one: "flynn.db.warm".
const warmDSNSuffix = ".warm"

// warmSchema: `size` is the uncompressed length, so accounting and the length
// check need no decompress; `packed` is the compressed length; `refs` carries
// the hot refcount so later tiers keep content-addressed sharing.
const warmSchema = `CREATE TABLE IF NOT EXISTS warm_blobs (
  content_id TEXT    NOT NULL PRIMARY KEY,
  body       BLOB    NOT NULL,
  size       INTEGER NOT NULL,
  packed     INTEGER NOT NULL,
  refs       INTEGER NOT NULL DEFAULT 0
)`

// warmDSN maps ":memory:" to a separate ":memory:" database and a path to
// path plus warmDSNSuffix.
func warmDSN(hotDSN string) string {
	if hotDSN == ":memory:" {
		return ":memory:"
	}
	return hotDSN + warmDSNSuffix
}

// openWarm uses one connection: warm traffic is a slow sweep and rare
// fallback reads, and one connection keeps ":memory:" alive.
func openWarm(ctx context.Context, hotDSN string) (*warmStore, error) {
	dsn := warmDSN(hotDSN)
	db, err := sql.Open("sqlite", dsn+warmPragmas+sqlitex.WriteTxLock)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open warm store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, warmSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: init warm schema: %w", err)
	}
	return &warmStore{db: db}, nil
}

// warmPragmas mirrors the hot store's write side, minus read-pool tuning.
const warmPragmas = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"

// put is idempotent, so a resumed sweep converges. It cannot join the hot
// transaction, which is why relocation copies before deleting.
func (w *warmStore) put(ctx context.Context, contentID string, packed []byte, size, refs int64) error {
	_, err := w.db.ExecContext(ctx,
		`INSERT INTO warm_blobs (content_id, body, size, packed, refs) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(content_id) DO UPDATE SET body = excluded.body, size = excluded.size,
		     packed = excluded.packed, refs = excluded.refs`,
		contentID, packed, size, int64(len(packed)), refs)
	if err != nil {
		return fmt.Errorf("sqlite: warm put %s: %w", contentID, err)
	}
	return nil
}

// get returns (nil, false, nil) for a missing id, distinct from the error a
// corrupt record returns.
func (w *warmStore) get(ctx context.Context, contentID string) ([]byte, bool, error) {
	var packed []byte
	err := w.db.QueryRowContext(ctx, `SELECT body FROM warm_blobs WHERE content_id = ?`, contentID).Scan(&packed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: warm get %s: %w", contentID, err)
	}
	raw, err := decompressBody(packed)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// has lets relocation confirm a warm copy before deleting from hot.
func (w *warmStore) has(ctx context.Context, contentID string) (bool, error) {
	var one int
	err := w.db.QueryRowContext(ctx, `SELECT 1 FROM warm_blobs WHERE content_id = ?`, contentID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: warm has %s: %w", contentID, err)
	}
	return true, nil
}

func (w *warmStore) close() error { return w.db.Close() }

// WarmBlobStats reports archived bodies and their uncompressed and compressed
// bytes, the ratio the size gate reads.
func (s *Store) WarmBlobStats(ctx context.Context) (count, rawBytes, packedBytes int64, err error) {
	if s.warm == nil {
		return 0, 0, 0, nil
	}
	err = s.warm.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0), COALESCE(SUM(packed), 0) FROM warm_blobs`).
		Scan(&count, &rawBytes, &packedBytes)
	return count, rawBytes, packedBytes, err
}

// sealedBlobsSelectSQL selects bodies with no reference above its stream's
// latest checkpoint. A stream with no checkpoint seals nothing, and a body
// shared across streams waits until sealed in all.
//
// The `payload_blob != ”` term is redundant with the join (a content id is never empty)
// but explicit: it lets SQLite prove the query implies the partial index's predicate, so
// the reference probe seeks idx_events_payload_blob instead of scanning the whole log per
// blob (asserted by TestSealedBlobsIndex).
const sealedBlobsSelectSQL = `SELECT b.content_id, b.body, b.size, b.refs
	FROM blobs b
	WHERE NOT EXISTS (
		SELECT 1 FROM events e
		WHERE e.payload_blob = b.content_id AND e.payload_blob != ''
		  AND e.seq > COALESCE((SELECT MAX(size) FROM checkpoints c WHERE c.stream = e.stream), 0)
	)`

type sealedBlob struct {
	id   string
	raw  []byte
	size int64
	refs int64
}

// selectSealedBlobs materializes its result so the sweep holds no read cursor
// while it mutates the hot store.
func (s *Store) selectSealedBlobs(ctx context.Context) ([]sealedBlob, error) {
	rows, err := s.reads().QueryContext(ctx, sealedBlobsSelectSQL)
	if err != nil {
		return nil, fmt.Errorf("sqlite: select sealed blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var eligible []sealedBlob
	for rows.Next() {
		var b sealedBlob
		if err := rows.Scan(&b.id, &b.raw, &b.size, &b.refs); err != nil {
			return nil, fmt.Errorf("sqlite: scan sealed blob: %w", err)
		}
		eligible = append(eligible, b)
	}
	return eligible, rows.Err()
}

// ArchiveSealedBlobs moves sealed bodies to warm and returns the count. Each
// body is copied and confirmed first; then one transaction deletes them from
// hot and appends a RetentionArchived event with a manifest digest, so no
// body moves unrecorded. A crash between phases leaves both copies and no
// event, and a retry finishes. A sweep that moves nothing records nothing.
func (s *Store) ArchiveSealedBlobs(ctx context.Context) (moved int, err error) {
	if s.warm == nil {
		return 0, nil
	}
	eligible, err := s.selectSealedBlobs(ctx)
	if err != nil {
		return 0, err
	}

	// Phase one: a body warm reports absent after put stays hot.
	var (
		confirmed           []sealedBlob
		hotBytes, warmBytes int64
	)
	for _, b := range eligible {
		packed := compressBody(b.raw)
		if err := s.warm.put(ctx, b.id, packed, b.size, b.refs); err != nil {
			return 0, err
		}
		present, err := s.warm.has(ctx, b.id)
		if err != nil {
			return 0, err
		}
		if !present {
			continue
		}
		confirmed = append(confirmed, b)
		hotBytes += b.size
		warmBytes += int64(len(packed))
	}
	if len(confirmed) == 0 {
		return 0, nil
	}

	// Phase two: delete and record in one transaction.
	ids := make([]string, len(confirmed))
	for i, b := range confirmed {
		ids[i] = b.id
	}
	err = sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE content_id = ?`, id); err != nil {
				return fmt.Errorf("sqlite: drop archived blob %s: %w", id, err)
			}
		}
		_, err := appendControlTx(ctx, tx, s, retentionArchivedEvent(ids, hotBytes, warmBytes))
		return err
	})
	if err != nil {
		return 0, err
	}
	return len(confirmed), nil
}

// retentionArchivedEvent's payload is small and always inline, never
// externalized into the tier it records.
func retentionArchivedEvent(ids []string, hotBytes, warmBytes int64) spine.AppendInput {
	return spine.AppendInput{
		Stream: chain.RetentionStream,
		Type:   chain.RetentionArchived,
		Actor:  spine.ActorSystem,
		Payload: map[string]any{
			chain.RetentionKeyAction:    chain.RetentionActionArchive,
			chain.RetentionKeyTier:      chain.RetentionTierWarm,
			chain.RetentionKeyMoved:     int64(len(ids)),
			chain.RetentionKeyHotBytes:  hotBytes,
			chain.RetentionKeyWarmBytes: warmBytes,
			chain.RetentionKeyManifest:  chain.RetentionManifest(ids),
		},
	}
}
