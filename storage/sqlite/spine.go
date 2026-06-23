package sqlite

// This file holds the SQLite-backed spine.Log, the append-only event store. It
// shares the Store's database and connection (see Store.Log), so events live in
// the same file as the state projections. It passes spinetest.RunSuite, matching
// the in-memory log byte for byte.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/sqlitex"
	"github.com/ionalpha/flynn/spine"
)

// eventLog is the SQLite-backed spine.Log. It is returned by Store.Log and uses
// the Store's shared connection and clock.
type eventLog struct {
	db  *sql.DB
	clk clock.Clock
}

var _ spine.Log = (*eventLog)(nil)

// Append implements spine.Log. It assigns the next per-stream Seq and stamps an
// unset Time from the clock, inside one transaction so a concurrent append can
// never claim the same (stream, seq).
func (l *eventLog) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	t := in.Time
	if t.IsZero() {
		t = l.clk.Now()
	}
	t = t.UTC()

	payload, err := json.Marshal(in.Payload)
	if err != nil {
		return spine.Event{}, fmt.Errorf("sqlite: marshal event payload: %w", err)
	}

	var e spine.Event
	err = sqlitex.Tx(ctx, l.db, func(tx *sql.Tx) error {
		var maxSeq int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE stream = ?`, in.Stream).Scan(&maxSeq); err != nil {
			return err
		}
		seq := maxSeq + 1
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (stream, seq, time, type, actor, payload, trace_id, span_id, causation_id, origin_instance_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			in.Stream, seq, sqlitex.FormatTime(t), in.Type, string(in.Actor), string(payload),
			in.TraceID, in.SpanID, in.CausationID, in.OriginInstanceID); err != nil {
			return err
		}
		e = spine.Event{
			Stream: in.Stream, Seq: seq, Time: t, Type: in.Type, Actor: in.Actor,
			Payload:     clonePayload(in.Payload),
			TraceID:     in.TraceID,
			SpanID:      in.SpanID,
			CausationID: in.CausationID, OriginInstanceID: in.OriginInstanceID,
		}
		return nil
	})
	if err != nil {
		return spine.Event{}, err
	}
	return e, nil
}

// Read implements spine.Log: events on a stream in Seq order, AfterSeq exclusive,
// Limit capping (<= 0 means no limit).
func (l *eventLog) Read(ctx context.Context, q spine.Query) ([]spine.Event, error) {
	query := `SELECT * FROM events WHERE stream = ? AND seq > ? ORDER BY seq`
	args := []any{q.Stream, q.AfterSeq}
	if q.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, q.Limit)
	}
	rows, err := l.db.QueryContext(ctx, query, args...)
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
			&e.TraceID, &e.SpanID, &e.CausationID, &e.OriginInstanceID); err != nil {
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
