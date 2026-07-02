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
// wraps it in a transaction of its own) and returns the stored event, decoding a
// RawPayload so the returned payload shape matches a Payload append.
func appendTx(ctx context.Context, tx *sql.Tx, p *Store, in spine.AppendInput) (spine.Event, error) {
	payload, err := in.DecodedPayload()
	if err != nil {
		return spine.Event{}, err
	}
	seq, t, version, err := insertEventTx(ctx, tx, p, in)
	if err != nil {
		return spine.Event{}, err
	}
	return spine.Event{
		Stream: in.Stream, Seq: seq, Time: t, Type: in.Type, Actor: in.Actor,
		Payload:       clonePayload(payload),
		SchemaVersion: version,
		TraceID:       in.TraceID,
		SpanID:        in.SpanID,
		CausationID:   in.CausationID, OriginInstanceID: in.OriginInstanceID,
		Principal: in.Principal,
	}, nil
}

// insertEventSQL is the append statement, prepared once at Open (stmts.eventInsert).
// Seq assignment is folded in: the scalar subquery seeks the (stream, seq) primary
// key for the current maximum and RETURNING hands the assigned value back, so an
// append is one statement instead of a SELECT-then-INSERT pair. That drops a round
// trip per event and shortens the window the write transaction holds the database
// exclusively.
const insertEventSQL = `INSERT INTO events (stream, seq, time, type, actor, payload, trace_id, span_id, causation_id, origin_instance_id, schema_version, principal)
	VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE stream = ?), ?,?,?,?,?,?,?,?,?,?)
	RETURNING seq`

// insertEventTx writes one event row inside an existing transaction, assigning
// the next per-stream Seq and stamping an unset Time from the clock. A RawPayload is
// stored verbatim (it is already the JSON the payload column holds), so the
// durable command path never re-encodes what its stamper serialized; a decoded
// Payload is marshalled here. The command path (commit) calls this directly and
// projects from the typed record it already holds, so it never pays for the
// decoded event appendTx builds.
func insertEventTx(ctx context.Context, tx *sql.Tx, p *Store, in spine.AppendInput) (seq int64, t time.Time, version int, err error) {
	t = in.Time
	if t.IsZero() {
		t = p.clk.Now()
	}
	t = t.UTC()

	var payload []byte
	switch {
	case in.Payload != nil && len(in.RawPayload) > 0:
		return 0, time.Time{}, 0, spine.ErrPayloadConflict
	case len(in.RawPayload) > 0:
		payload = in.RawPayload
	default:
		payload, err = json.Marshal(in.Payload)
		if err != nil {
			return 0, time.Time{}, 0, fmt.Errorf("sqlite: marshal event payload: %w", err)
		}
	}

	version = in.SchemaVersion
	if version == 0 {
		version = spine.DefaultSchemaVersion
	}

	// StmtContext binds the Open-time prepared statement to this transaction's
	// connection; on the single write connection that is a cached re-bind, not a
	// recompile.
	if err := tx.StmtContext(ctx, p.stmts.eventInsert).QueryRowContext(ctx,
		in.Stream, in.Stream, sqlitex.FormatTime(t), in.Type, string(in.Actor), string(payload),
		in.TraceID, in.SpanID, in.CausationID, in.OriginInstanceID, version, in.Principal).Scan(&seq); err != nil {
		return 0, time.Time{}, 0, err
	}
	return seq, t, version, nil
}

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
			payload string
		)
		if err := rows.Scan(&e.Stream, &e.Seq, &ts, &e.Type, &actor, &payload,
			&e.TraceID, &e.SpanID, &e.CausationID, &e.OriginInstanceID, &e.SchemaVersion, &e.Principal); err != nil {
			return nil, err
		}
		e.Time = sqlitex.ParseTime(ts)
		e.Actor = spine.ActorType(actor)
		if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
			return nil, fmt.Errorf("sqlite: unmarshal event payload: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
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

// clonePayload shallow-copies a payload map so the returned event is decoupled
// from the caller's map (the log is immutable).
func clonePayload(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	c := make(map[string]any, len(p))
	for k, v := range p {
		c[k] = v
	}
	return c
}
