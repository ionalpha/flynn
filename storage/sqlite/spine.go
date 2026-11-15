package sqlite

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

// eventLog is the [spine.Log] returned by [Store.Log], on the Store's
// connections, statements and clock.
type eventLog struct {
	p *Store
}

var _ spine.Log = (*eventLog)(nil)

// Append assigns Seq and an unset Time in one transaction, so concurrent
// appends never share a (stream, seq).
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

// appendTx builds the event with [spine.AppendInput.Materialize], which owns
// the defaulting, and stamps the assigned Seq.
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

// appendControlTx keeps a small lifecycle event inline whatever the blob
// threshold, so the record of archiving is never itself archived. Seq and
// defaulting match the ordinary path.
func appendControlTx(ctx context.Context, tx *sql.Tx, p *Store, in spine.AppendInput) (spine.Event, error) {
	e, raw, err := in.Materialize(p.clk, 0)
	if err != nil {
		return spine.Event{}, err
	}
	var seq int64
	if err := tx.StmtContext(ctx, p.stmts.eventInsert).QueryRowContext(ctx,
		in.Stream, in.Stream, sqlitex.FormatTime(e.Time.UTC()), in.Type, string(in.Actor),
		string(raw), // inline payload, no blob id - the empty payload_blob keeps the read inline
		in.TraceID, in.SpanID, in.CausationID, in.OriginInstanceID, e.SchemaVersion, in.Principal, "").
		Scan(&seq); err != nil {
		return spine.Event{}, err
	}
	e.Seq = seq
	return e, nil
}

// insertEventSQL assigns Seq in a subquery and returns it, so an append is
// one statement and the write lock is held for less time.
const insertEventSQL = `INSERT INTO events (stream, seq, time, type, actor, payload, trace_id, span_id, causation_id, origin_instance_id, schema_version, principal, payload_blob)
	VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE stream = ?), ?,?,?,?,?,?,?,?,?,?,?)
	RETURNING seq`

// insertEventTx is the command-path insert. A RawPayload is stored verbatim,
// so the path never re-encodes what the stamper serialized.
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

// insertEventRow does no defaulting, so Append and the command path share one
// INSERT and one Seq assignment.
func insertEventRow(ctx context.Context, tx *sql.Tx, p *Store, in spine.AppendInput, t time.Time, version int, raw json.RawMessage) (int64, error) {
	// Either storage reads back identical bytes, so the committed event is
	// unchanged.
	inline, blobID, err := resolvePayloadStorage(ctx, tx, p.blobThreshold, raw)
	if err != nil {
		return 0, err
	}
	var seq int64
	if err := tx.StmtContext(ctx, p.stmts.eventInsert).QueryRowContext(ctx,
		in.Stream, in.Stream, sqlitex.FormatTime(t.UTC()), in.Type, string(in.Actor), inline,
		in.TraceID, in.SpanID, in.CausationID, in.OriginInstanceID, version, in.Principal, blobID).Scan(&seq); err != nil {
		return 0, err
	}
	return seq, nil
}

// eventsReadSQL selects all three payload sources: payload_blob, inline
// payload, and b.body when the LEFT JOIN hits. A warm body misses the join and
// is read from warm by id.
const eventsReadSQL = `SELECT e.stream, e.seq, e.time, e.type, e.actor,
		e.payload_blob, e.payload, b.body,
		e.trace_id, e.span_id, e.causation_id, e.origin_instance_id, e.schema_version, e.principal
	FROM events e LEFT JOIN blobs b ON b.content_id = e.payload_blob
	WHERE e.stream = ? AND e.seq > ? ORDER BY e.seq LIMIT ?`

// Read maps Limit <= 0 to LIMIT -1 so one statement serves both.
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

// resolvePayload tries inline, hot, then warm. A body in neither is an error
// naming the content id, never an empty payload.
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

// SaveSnapshot replaces any snapshot at (stream, seq).
func (l *eventLog) SaveSnapshot(ctx context.Context, s spine.Snapshot) error {
	_, err := l.p.db.ExecContext(ctx,
		`INSERT INTO snapshots (stream, seq, payload) VALUES (?,?,?)
		 ON CONFLICT(stream, seq) DO UPDATE SET payload = excluded.payload`,
		s.Stream, s.Seq, s.Payload)
	return err
}

// LatestSnapshot treats upToSeq <= 0 as unbounded.
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
