// Package sqlite is the durable spine.Log: an append-only event store backed by
// pure-Go modernc.org/sqlite (no cgo), so the agent stays a single static
// binary. It passes the same spinetest.RunSuite conformance suite as the
// in-memory log, so the two are byte-for-byte interchangeable.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/migrate"
	"github.com/ionalpha/flynn/spine"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Option configures the Log.
type Option func(*Log)

// WithClock sets the time source used to stamp events whose Time is unset
// (default: clock.System). Tests and replay pass a clock.Manual.
func WithClock(c clock.Clock) Option { return func(l *Log) { l.clk = c } }

// Log is the SQLite-backed spine.Log.
type Log struct {
	db  *sql.DB
	clk clock.Clock
}

var _ spine.Log = (*Log)(nil)

// Open opens (creating if needed) a SQLite database at dsn and migrates it to
// the latest schema. dsn is a file path, or ":memory:" for an ephemeral store.
//
// The pool is capped at one connection: SQLite serialises writers anyway, and a
// single connection keeps a ":memory:" database alive with a consistent view.
func Open(ctx context.Context, dsn string, opts ...Option) (*Log, error) {
	conn := dsn + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("spine/sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(1)

	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate.Run(ctx, db, sub); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("spine/sqlite: migrate: %w", err)
	}

	l := &Log{db: db, clk: clock.System{}}
	for _, o := range opts {
		o(l)
	}
	return l, nil
}

// Close closes the underlying database.
func (l *Log) Close() error { return l.db.Close() }

// Append implements spine.Log. It assigns the next per-stream Seq and stamps an
// unset Time from the clock, inside one transaction so a concurrent append can
// never claim the same (stream, seq).
func (l *Log) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	t := in.Time
	if t.IsZero() {
		t = l.clk.Now()
	}
	t = t.UTC()

	payload, err := json.Marshal(in.Payload)
	if err != nil {
		return spine.Event{}, fmt.Errorf("spine/sqlite: marshal payload: %w", err)
	}

	var e spine.Event
	err = l.tx(ctx, func(tx *sql.Tx) error {
		var maxSeq int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE stream = ?`, in.Stream).Scan(&maxSeq); err != nil {
			return err
		}
		seq := maxSeq + 1
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (stream, seq, time, type, actor, payload, trace_id, span_id, causation_id, origin_instance_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			in.Stream, seq, t.Format(time.RFC3339Nano), in.Type, string(in.Actor), string(payload),
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
func (l *Log) Read(ctx context.Context, q spine.Query) ([]spine.Event, error) {
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
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("spine/sqlite: parse time %q: %w", ts, err)
		}
		e.Time = parsed.UTC()
		e.Actor = spine.ActorType(actor)
		if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
			return nil, fmt.Errorf("spine/sqlite: unmarshal payload: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (l *Log) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Commit()
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
