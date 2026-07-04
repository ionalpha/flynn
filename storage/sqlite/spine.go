package sqlite

// This file holds the SQLite-backed spine.Log, the append-only event store. It
// shares the Store's database and connection (see Store.Log), so events live in
// the same file as the state projections. It passes spinetest.RunSuite, matching
// the in-memory log byte for byte.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/internal/sqlitex"
	"github.com/ionalpha/flynn/spine"
)

// eventLog is the SQLite-backed spine.Log. It is returned by Store.Log and uses
// the Store's connections (writes on the write connection, reads on the pool),
// prepared statements, and clock.
type eventLog struct {
	p *Store
}

var _ spine.Log = (*eventLog)(nil)

// Append implements spine.Log. It assigns the next per-stream Seq and stamps an
// unset Time from the clock, inside one transaction so a concurrent append can
// never claim the same (stream, seq).
func (l *eventLog) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	var e spine.Event
	err := sqlitex.Tx(ctx, l.p.db, func(tx *sql.Tx) error {
		var err error
		e, err = appendTx(ctx, tx, l.p, in)
		return err
	})
	if err != nil {
		return spine.Event{}, err
	}
	return e, nil
}

// appendTx appends one event inside an existing transaction (the public Append
// wraps it in a transaction of its own) and returns the stored event. It builds
// the event through spine.AppendInput.Materialize - the one place the event shape
// and its defaulting live - and stamps the database-assigned Seq onto it.
func appendTx(ctx context.Context, tx *sql.Tx, p *Store, in spine.AppendInput) (spine.Event, error) {
	e, raw, err := in.Materialize(p.clk, 0)
	if err != nil {
		return spine.Event{}, err
	}
	seq, err := insertEventRow(ctx, tx, p, in, e.Time, e.SchemaVersion, raw)
	if err != nil {
		return spine.Event{}, err
	}
	e.Seq = seq
	return e, nil
}

// insertEventSQL is the append statement, prepared once at Open (stmts.eventInsert).
// Seq assignment is folded in: the scalar subquery seeks the (stream, seq) primary
// key for the current maximum and RETURNING hands the assigned value back, so an
// append is one statement instead of a SELECT-then-INSERT pair. That drops a round
// trip per event and shortens the window the write transaction holds the database
// exclusively.
const insertEventSQL = `INSERT INTO events (stream, seq, time, type, actor, payload, trace_id, span_id, causation_id, origin_instance_id, schema_version, principal, payload_blob)
	VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE stream = ?), ?,?,?,?,?,?,?,?,?,?,?)
	RETURNING seq`

// insertEventTx writes one event row for the command path inside an existing
// transaction, assigning the next per-stream Seq. It resolves the stored payload
// without decoding it - a RawPayload is stored verbatim (it is already the JSON the
// payload column holds), a decoded Payload is marshalled - so the durable command
// path never re-encodes or decodes what its stamper serialized. The command path
// (commit) projects from the typed record it already holds, so it discards the
// returned Seq and never builds a decoded Event.
func insertEventTx(ctx context.Context, tx *sql.Tx, p *Store, in spine.AppendInput) (int64, error) {
	t := in.Time
	if t.IsZero() {
		t = p.clk.Now()
	}
	version := in.SchemaVersion
	if version == 0 {
		version = spine.DefaultSchemaVersion
	}
	var raw json.RawMessage
	switch {
	case in.Payload != nil && len(in.RawPayload) > 0:
		return 0, spine.ErrPayloadConflict
	case len(in.RawPayload) > 0:
		raw = in.RawPayload
	default:
		b, err := json.Marshal(in.Payload)
		if err != nil {
			return 0, fmt.Errorf("sqlite: marshal event payload: %w", err)
		}
		raw = b
	}
	return insertEventRow(ctx, tx, p, in, t, version, raw)
}

// insertEventRow writes one already-resolved event row inside a transaction and
// returns the database-assigned per-stream Seq. It does no defaulting or payload
// resolution - callers pass the stamped Time, SchemaVersion, and stored payload
// bytes - so the Append path (via Materialize) and the command path (via
// insertEventTx) share one INSERT and one Seq assignment.
func insertEventRow(ctx context.Context, tx *sql.Tx, p *Store, in spine.AppendInput, t time.Time, version int, raw json.RawMessage) (int64, error) {
	// A large body is externalized into blobs and the inline column is left empty; a
	// small one stays inline with no blob id. Either way the read path rehydrates the
	// exact same bytes, so the canonical event the tree commits to is unchanged.
	inline, blobID, err := resolvePayloadStorage(ctx, tx, p.blobThreshold, raw)
	if err != nil {
		return 0, err
	}
	var seq int64
	// StmtContext binds the Open-time prepared statement to this transaction's
	// connection; on the single write connection that is a cached re-bind, not a
	// recompile.
	if err := tx.StmtContext(ctx, p.stmts.eventInsert).QueryRowContext(ctx,
		in.Stream, in.Stream, sqlitex.FormatTime(t.UTC()), in.Type, string(in.Actor), inline,
		in.TraceID, in.SpanID, in.CausationID, in.OriginInstanceID, version, in.Principal, blobID).Scan(&seq); err != nil {
		return 0, err
	}
	return seq, nil
}

// eventsReadSQL reads a stream's events in Seq order, projecting the three columns the
// payload can come from so the read loop can resolve it across tiers: payload_blob names
// the externalized body (empty for an inline payload), payload is the inline bytes, and
// b.body is the hot blob when the LEFT JOIN hits. An inline row has an empty payload_blob,
// so the join misses and the loop uses payload; a hot-blob row joins and uses b.body; a
// row whose body has been relocated to the warm tier misses the join (b.body is NULL) and
// the loop rehydrates it from the warm store by its content id. Columns are listed
// explicitly (not SELECT *) so the physical row layout stays out of the scan.
const eventsReadSQL = `SELECT e.stream, e.seq, e.time, e.type, e.actor,
		e.payload_blob, e.payload, b.body,
		e.trace_id, e.span_id, e.causation_id, e.origin_instance_id, e.schema_version, e.principal
	FROM events e LEFT JOIN blobs b ON b.content_id = e.payload_blob
	WHERE e.stream = ? AND e.seq > ? ORDER BY e.seq LIMIT ?`

// Read implements spine.Log: events on a stream in Seq order, AfterSeq exclusive,
// Limit capping (<= 0 means no limit). It runs on the read pool via the prepared
// statement; a non-positive Limit becomes SQLite's LIMIT -1 (no limit), so both
// shapes share one statement.
func (l *eventLog) Read(ctx context.Context, q spine.Query) ([]spine.Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = -1
	}
	rows, err := l.p.stmts.eventsRead.QueryContext(ctx, q.Stream, q.AfterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]spine.Event, 0)
	for rows.Next() {
		var (
			e       spine.Event
			ts      string
			actor   string
			blobID  string
			inline  string
			hotBody []byte // NULL (nil) when the LEFT JOIN misses: inline row, or body tiered to warm
		)
		if err := rows.Scan(&e.Stream, &e.Seq, &ts, &e.Type, &actor, &blobID, &inline, &hotBody,
			&e.TraceID, &e.SpanID, &e.CausationID, &e.OriginInstanceID, &e.SchemaVersion, &e.Principal); err != nil {
			return nil, err
		}
		e.Time = sqlitex.ParseTime(ts)
		e.Actor = spine.ActorType(actor)
		payload, err := l.resolvePayload(ctx, blobID, inline, hotBody)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &e.Payload); err != nil {
			return nil, fmt.Errorf("sqlite: unmarshal event payload: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// resolvePayload returns an event's payload bytes across the storage tiers. An empty
// blobID means the payload was kept inline (use it). A non-empty blobID with a hot body
// (the LEFT JOIN hit) uses that verbatim. A non-empty blobID whose hot body is absent
// means the body was relocated to the warm tier, so it is rehydrated (and decompressed)
// from the warm store by its content id. A body that is in neither tier is a missing
// archive segment: it returns an error naming the content id, so a read over relocated
// history fails loudly at exactly the absent body rather than yielding an empty payload.
func (l *eventLog) resolvePayload(ctx context.Context, blobID, inline string, hotBody []byte) ([]byte, error) {
	if blobID == "" {
		return []byte(inline), nil
	}
	if hotBody != nil {
		return hotBody, nil
	}
	if l.p.warm != nil {
		raw, ok, err := l.p.warm.get(ctx, blobID)
		if err != nil {
			return nil, err
		}
		if ok {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("sqlite: payload body %s is in neither the hot nor the warm tier", blobID)
}

// SaveSnapshot implements spine.Log: it stores a stream checkpoint, replacing any
// snapshot already at (stream, seq).
func (l *eventLog) SaveSnapshot(ctx context.Context, s spine.Snapshot) error {
	_, err := l.p.db.ExecContext(ctx,
		`INSERT INTO snapshots (stream, seq, payload) VALUES (?,?,?)
		 ON CONFLICT(stream, seq) DO UPDATE SET payload = excluded.payload`,
		s.Stream, s.Seq, s.Payload)
	return err
}

// LatestSnapshot implements spine.Log: the newest snapshot for stream at or before
// upToSeq (any seq when upToSeq <= 0), and false when none exists.
func (l *eventLog) LatestSnapshot(ctx context.Context, stream string, upToSeq int64) (spine.Snapshot, bool, error) {
	s := spine.Snapshot{Stream: stream}
	err := l.p.reads().QueryRowContext(ctx,
		`SELECT seq, payload FROM snapshots WHERE stream = ? AND (? <= 0 OR seq <= ?) ORDER BY seq DESC LIMIT 1`,
		stream, upToSeq, upToSeq).Scan(&s.Seq, &s.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return spine.Snapshot{}, false, nil
	}
	if err != nil {
		return spine.Snapshot{}, false, err
	}
	return s, true, nil
}
