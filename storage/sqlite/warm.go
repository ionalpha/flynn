package sqlite

// The warm tier. A payload body starts life in the hot `blobs` table (content-addressed,
// deduped, verbatim). Once every event that references it has been sealed under a
// checkpoint - so the body belongs to closed, provable history and will not be appended
// to again - ArchiveSealedBlobs relocates it into the warm store: a separate SQLite file
// holding the same content-addressed bodies zstd-compressed. The hot file then keeps only
// the bodies of unsealed (recent) events, so its footprint tracks the working set rather
// than all history, while the full record stays reachable: a read that misses the hot
// `blobs` table falls back to the warm store and decompresses (see spine.go Read).
//
// Relocation is copy-then-delete, never move-in-place, and both halves are idempotent by
// content id. A body is written to warm first and only deleted from hot once it is present
// in warm, so a crash between the two leaves the body in BOTH stores (harmless - the hot
// read wins) and never in neither. A reader concurrent with archival always resolves the
// body: from hot until the delete commits, from warm after. Because the body is addressed
// by its own SHA-256 and the event envelope commits to that id, moving where it is stored
// changes nothing the Merkle tree signed - a warm body rehydrates to the exact bytes the
// chain already proved, and a warm store that has lost or corrupted a body is DETECTABLE
// (the id names what is missing), never a silent gap.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/internal/sqlitex"
	"github.com/ionalpha/flynn/spine"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

// warmStore is the compressed archive of sealed payload bodies, backed by its own SQLite
// file so it can live on cheaper or loss-tolerant storage than the hot database and its
// bytes never inflate the hot file. It holds a single content-addressed table; a lost warm
// file costs replayability of archived history, never the verifiability of the log, so it
// runs with the same crash-safe-but-relaxed durability as the hot store.
type warmStore struct {
	db *sql.DB
}

// warmDSNSuffix names the warm file beside the hot one (e.g. "flynn.db" -> "flynn.db.warm").
// A ":memory:" hot store gets a distinct ":memory:" warm store, which persists for the
// process on its single connection just as the hot in-memory store does.
const warmDSNSuffix = ".warm"

// warmSchema is the warm store's one table. `body` holds the zstd-compressed bytes;
// `size` is the ORIGINAL byte length (what the hot `blobs.size` held before relocation),
// kept so warm accounting and the rehydrated-length check need no decompress; `packed` is
// the compressed length, for the compression-ratio report. `refs` carries the hot refcount
// across so a later cold tier or GC keeps the same content-addressed sharing invariant.
const warmSchema = `CREATE TABLE IF NOT EXISTS warm_blobs (
  content_id TEXT    NOT NULL PRIMARY KEY,
  body       BLOB    NOT NULL,
  size       INTEGER NOT NULL,
  packed     INTEGER NOT NULL,
  refs       INTEGER NOT NULL DEFAULT 0
)`

// warmDSN derives the warm store's dsn from the hot store's. A ":memory:" hot store maps to
// ":memory:" (a separate in-process database), any file path to that path plus the suffix.
func warmDSN(hotDSN string) string {
	if hotDSN == ":memory:" {
		return ":memory:"
	}
	return hotDSN + warmDSNSuffix
}

// openWarm opens (creating if needed) the warm store beside the hot database and ensures
// its table exists. It uses the same relaxed-but-crash-safe WAL pragmas as the hot store
// and a single connection: warm writes are the low-rate archival sweep, warm reads are the
// rare replay-of-archived-history fallback, so one connection is ample and keeps a
// ":memory:" warm store alive with a consistent view.
func openWarm(ctx context.Context, hotDSN string) (*warmStore, error) {
	dsn := warmDSN(hotDSN)
	db, err := sql.Open("sqlite", dsn+warmPragmas)
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

// warmPragmas mirrors the hot store's write-side configuration (WAL, relaxed fsync, wait on
// a lock); the warm store declares no foreign keys and needs no read pool, so it omits
// query_only and the pool-only tuning.
const warmPragmas = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"

// put writes one compressed body under its content id, idempotently: re-archiving a body
// already present overwrites it with the same bytes and refreshes its refcount, so a
// retried or crash-resumed sweep converges rather than erroring. It runs on the warm store
// alone (a different database than the hot transaction), which is why relocation copies
// here first and only then deletes from hot.
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

// get returns the decompressed body for a content id and whether it was present. A missing
// id is (nil, false, nil) so the caller can distinguish "not in warm" (try elsewhere, or
// report which id is absent) from a decode failure (a corrupt warm record, returned as an
// error rather than wrong bytes).
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

// has reports whether a content id is present in the warm store, without decompressing.
// Relocation uses it to delete a body from hot only once its warm copy is durable.
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

// close closes the warm store's database.
func (w *warmStore) close() error { return w.db.Close() }

// WarmBlobStats reports the warm store's accounting: the number of bodies archived, their
// original (uncompressed) total bytes, and their compressed total bytes. The ratio of the
// two is the warm-tier compression report the size gate reads; distinct-versus-referenced
// dedup is unchanged from the hot tier and reported there.
func (s *Store) WarmBlobStats(ctx context.Context) (count, rawBytes, packedBytes int64, err error) {
	if s.warm == nil {
		return 0, 0, 0, nil
	}
	err = s.warm.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0), COALESCE(SUM(packed), 0) FROM warm_blobs`).
		Scan(&count, &rawBytes, &packedBytes)
	return count, rawBytes, packedBytes, err
}

// sealedBlobsSelectSQL selects the hot bodies eligible to relocate to warm: every event
// that references the body sits at or below its stream's highest checkpoint size, so the
// body belongs entirely to sealed history and no future append can add a reference to it.
// A stream with no checkpoint has sealed size 0, so its bodies are never eligible - the
// tier only ever moves what is provably closed. The NOT EXISTS names an unsealed reference
// as the disqualifier, so a body shared across streams waits until it is sealed in all of
// them.
const sealedBlobsSelectSQL = `SELECT b.content_id, b.body, b.size, b.refs
	FROM blobs b
	WHERE NOT EXISTS (
		SELECT 1 FROM events e
		WHERE e.payload_blob = b.content_id
		  AND e.seq > COALESCE((SELECT MAX(size) FROM checkpoints c WHERE c.stream = e.stream), 0)
	)`

// sealedBlob is one hot body eligible to relocate: its content id, raw bytes, original
// (uncompressed) size, and hot reference count.
type sealedBlob struct {
	id   string
	raw  []byte
	size int64
	refs int64
}

// selectSealedBlobs reads the hot bodies whose every referencing event is sealed (see
// sealedBlobsSelectSQL). It runs on the read pool and materializes the result before the
// write phase, so the archival sweep holds no read cursor while it mutates the hot store.
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

// ArchiveSealedBlobs relocates every hot payload body whose referencing events are all
// sealed into the warm store, compressed, records the relocation as one governed retention
// event, and returns how many bodies moved. It is the hot->warm tiering step: after it
// runs, the hot `blobs` table holds only bodies still referenced by unsealed (recent)
// events, so the hot file's body footprint tracks the working set instead of all history.
//
// It runs in two phases so the mutation is atomic with its own audit record. First every
// eligible body is copied to warm (idempotent by content id) and confirmed durable there.
// Then, in a single write transaction, the confirmed bodies are deleted from hot and a
// RetentionArchived event is appended to the reserved retention stream naming the count,
// the bytes reclaimed and gained, and a manifest digest committing to exactly which bodies
// moved. Because the delete and the event commit together, a reader never sees bodies
// relocated with no event explaining where they went - tiering is never a silent mutation.
//
// The phase split preserves the crash guarantees of a copy-then-delete move. A crash after
// phase one leaves the copied bodies in both stores (harmless - the hot read wins) and no
// event; the retried sweep re-copies (idempotent), re-confirms, and completes phase two. A
// crash inside phase two rolls the transaction back atomically, so the bodies stay hot and
// no partial record is written. A body is addressed by its SHA-256 and the envelope commits
// to that id, so the warm copy rehydrates to the identical bytes and no checkpoint, proof,
// or event over the run streams changes. Freed hot pages return to SQLite's free list for
// reuse by later appends (bounding steady-state growth); reclaiming them to the OS is a
// separate compaction step. A no-op sweep (nothing sealed, or no warm tier) moves nothing
// and records nothing - only a real relocation is worth an event.
func (s *Store) ArchiveSealedBlobs(ctx context.Context) (moved int, err error) {
	if s.warm == nil {
		return 0, nil
	}
	eligible, err := s.selectSealedBlobs(ctx)
	if err != nil {
		return 0, err
	}

	// Phase one: copy each eligible body to warm and confirm it is durable there before it
	// becomes a candidate for deletion from hot. A body warm reports absent after a put is
	// left hot rather than deleted into a gap.
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

	// Phase two: delete the confirmed bodies from hot and record the tiering action, both
	// in one transaction so the mutation and its audit record commit together.
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

// retentionArchivedEvent builds the governed event recording one hot->warm relocation: the
// action, the destination tier, the body and byte counts, and the manifest digest over the
// moved content ids. Its payload is small and self-describing, so it is appended inline
// (see appendControlTx) and never itself externalized into the tier it governs.
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
