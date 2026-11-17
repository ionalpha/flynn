// Package sqlite is the durable single-file backend (pure-Go, no cgo). The
// state provider and the event spine share one file and one connection, so a
// mutation's event and its projection commit in one transaction.
//
// Each mutation is stamped by [state.Stamper], appended to the spine and
// projected, in one transaction. The live path projects the record the Stamper
// returned; Rebuild projects the same post-image from the payload through
// shared row helpers, so a rebuilt database equals a live one and state stays
// a fold of the spine. The shared conformance suites keep this backend
// interchangeable with the in-memory ones.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sync/atomic"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/sqlitex"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Option configures the Store.
type Option func(*Store)

// WithInstanceID sets the instance stamped on new records (default "local"),
// so a fleet merge can attribute writes.
func WithInstanceID(id string) Option {
	return func(s *Store) {
		if id != "" {
			s.instanceID = id
		}
	}
}

// WithClock sets the time source for timestamps and the HLC (default
// clock.System).
func WithClock(c clock.Clock) Option {
	return func(s *Store) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithIDGenerator sets the ID source (default: the store's clock with
// crypto/rand entropy). Seeded clock and entropy make IDs replayable.
func WithIDGenerator(g *ids.Generator) Option {
	return func(s *Store) {
		if g != nil {
			s.gen = g
		}
	}
}

// WithSnapshotCodec seals resource snapshots on save and verifies them before
// restore. One that fails to open is skipped and the stream folded from the
// start, so a tampered snapshot is never restored.
func WithSnapshotCodec(c spine.SnapshotCodec) Option {
	return func(s *Store) {
		s.snapCodec = c
	}
}

// WithSnapshotEvery snapshots after every k mutations, so a rebuild folds at
// most k events. It runs after commit and best effort: a failure never fails
// the write. k <= 0 disables it (the default).
func WithSnapshotEvery(k int) Option {
	return func(s *Store) {
		s.snapEvery = k
	}
}

// WithPayloadBlobThreshold sets the stored size at or above which a payload
// moves to the content-addressed blobs table. n <= 0 keeps all inline.
func WithPayloadBlobThreshold(n int) Option {
	return func(s *Store) {
		s.blobThreshold = n
	}
}

// Store is the SQLite [state.Provider], with the event spine at Log.
type Store struct {
	db *sql.DB
	// readDB is the read pool for a file database, nil for ":memory:". Reads
	// run here so they never queue behind write transactions; WAL allows N
	// readers beside the one writer.
	readDB     *sql.DB
	stmts      stmts
	clk        clock.Clock
	hlc        *hlc.Clock
	gen        *ids.Generator
	st         *state.Stamper
	instanceID string
	// jobsReady is on the Store because Jobs builds a facade per call, and all
	// must share one channel or a worker misses another's enqueues.
	jobsReady chan struct{}
	// Snapshot settings are on the Store because Resources builds a facade per
	// call, and all must share one counter.
	snapCodec   spine.SnapshotCodec
	snapEvery   int
	snapPending atomic.Int64
	// snapPendingState counts the state stream apart from resources.
	snapPendingState atomic.Int64
	blobThreshold    int
	// warm archives sealed payload bodies; a hot-table miss reads from it.
	warm *warmStore
}

var _ state.Provider = (*Store)(nil)

// Open opens or creates the database at dsn (a path, or ":memory:") and
// migrates it. Writes use one connection: SQLite serialises writers anyway,
// one connection keeps ":memory:" alive, and a cross-domain write can be one
// transaction.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	db, err := sqlitex.Open(ctx, dsn, migrations)
	if err != nil {
		return nil, err
	}
	if err := refuseNewerSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	readDB, err := sqlitex.OpenReadPool(dsn, 0)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	warm, err := openWarm(ctx, dsn)
	if err != nil {
		_ = db.Close()
		if readDB != nil {
			_ = readDB.Close()
		}
		return nil, err
	}
	s := &Store{db: db, readDB: readDB, warm: warm, clk: clock.System{}, instanceID: "local", jobsReady: make(chan struct{}, 1), blobThreshold: defaultPayloadBlobThreshold}
	for _, o := range opts {
		o(s)
	}
	if s.gen == nil {
		s.gen = ids.NewGenerator(ids.WithClock(s.clk))
	}
	s.hlc = hlc.NewClock(hlc.WithPhysical(s.clk))
	s.st = state.NewStamper(s.instanceID, s.clk, s.hlc, s.gen)
	if err := s.prepare(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) reads() *sql.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

// stmts holds statements prepared once at Open, because the pure-Go driver
// compiles on every call and these run on every control-loop turn.
type stmts struct {
	// Write-connection statements, entered into each Tx with StmtContext.
	eventInsert    *sql.Stmt // the folded assign-seq-and-insert
	sessionGetTx   *sql.Stmt // AppendTurn's session lookup
	turnsMaxSeq    *sql.Stmt // AppendTurn's per-session seq assignment
	turnInsert     *sql.Stmt
	sessionUpsert  *sql.Stmt
	resourceKeyTx  *sql.Stmt // the CAS lookup on every resource Put/Delete
	resourceUpsert *sql.Stmt // the projection write on every resource mutation
	eventsRead     *sql.Stmt
	sessionGet     *sql.Stmt
	skillByID      *sql.Stmt
	skillBySlug    *sql.Stmt
	memoryRecall   *sql.Stmt // the scoped, no-FTS recall shape (the agent-startup read)
}

func (s *Store) prepare(ctx context.Context) error {
	var err error
	prep := func(dst **sql.Stmt, db *sql.DB, q string) {
		if err != nil {
			return
		}
		*dst, err = db.PrepareContext(ctx, q)
	}
	prep(&s.stmts.eventInsert, s.db, insertEventSQL)
	prep(&s.stmts.sessionGetTx, s.db, sessionGetLiveSQL)
	prep(&s.stmts.turnsMaxSeq, s.db,
		`SELECT COALESCE(MAX(seq), 0) FROM turns WHERE session_id = ?`)
	prep(&s.stmts.turnInsert, s.db, insertTurnSQL)
	prep(&s.stmts.sessionUpsert, s.db, upsertSessionSQL)
	prep(&s.stmts.resourceKeyTx, s.db, resourceByKeySQL)
	prep(&s.stmts.resourceUpsert, s.db, upsertResourceSQL)
	prep(&s.stmts.eventsRead, s.reads(), eventsReadSQL)
	prep(&s.stmts.sessionGet, s.reads(), sessionGetLiveSQL)
	prep(&s.stmts.skillByID, s.reads(),
		`SELECT `+skillCols+` FROM skills WHERE id = ? AND deleted = 0`)
	prep(&s.stmts.skillBySlug, s.reads(),
		`SELECT `+skillCols+` FROM skills WHERE slug = ? AND deleted = 0 ORDER BY created_at, id LIMIT 1`)
	prep(&s.stmts.memoryRecall, s.reads(),
		`SELECT `+memoryCols+` FROM memory_items m
		 WHERE `+memoryLiveSQL+` AND m.scope_instance = ? AND m.scope_project = ? AND m.scope_workspace = ?
		 ORDER BY m.created_at DESC, m.id DESC LIMIT ?`)
	if err != nil {
		return fmt.Errorf("sqlite: prepare: %w", err)
	}
	return nil
}

// Name identifies the backend ("sqlite").
func (s *Store) Name() string { return "sqlite" }

// InstanceID is the id stamped on records this process creates.
func (s *Store) InstanceID() string { return s.instanceID }

// Sessions returns the durable conversation store.
func (s *Store) Sessions() state.SessionStore { return &sessions{s} }

// Skills returns the FTS5-searchable skill store.
func (s *Store) Skills() state.SkillStore { return &skills{s} }

// Memory returns the durable memory store.
func (s *Store) Memory() state.MemoryStore { return &memory{s} }

// Log returns the event spine in the same database, valid until Close.
func (s *Store) Log() spine.Log { return &eventLog{p: s} }

// Close closes both connections.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.readDB != nil {
		if rerr := s.readDB.Close(); err == nil {
			err = rerr
		}
	}
	if s.warm != nil {
		if rerr := s.warm.close(); err == nil {
			err = rerr
		}
	}
	return err
}

// commit appends build's event and runs its projection in one transaction, so
// the log and tables cannot diverge. build may read inside the transaction for
// CAS. The projection uses the stamped record, never decoding what it just
// encoded.
func (s *Store) commit(ctx context.Context, build func(tx *sql.Tx) (spine.AppendInput, func(tx *sql.Tx) error, error)) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		in, project, err := build(tx)
		if err != nil {
			return err
		}
		if _, err := insertEventTx(ctx, tx, s, in); err != nil {
			return err
		}
		return project(tx)
	})
	if err == nil {
		s.maybeSnapshotState(ctx)
	}
	return err
}

// Rebuild reprojects the tables from the log, starting at the latest usable
// snapshot. A snapshot that fails to verify or decode is skipped and the whole
// stream folded. Records apply by id, so it is idempotent.
func (s *Store) Rebuild(ctx context.Context) error {
	restored, afterSeq := s.stateSnapshotForRebuild(ctx)
	events, err := s.Log().Read(ctx, spine.Query{Stream: state.StateStream, AfterSeq: afterSeq})
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.restoreStateSnapshot(ctx, tx, restored); err != nil {
			return err
		}
		for _, e := range events {
			if err := s.applyEvent(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// applyEvent projects one event's post-image into the tables and FTS indexes
// during Rebuild.
func (s *Store) applyEvent(ctx context.Context, tx *sql.Tx, e spine.Event) error {
	switch e.Type {
	case state.EvSessionCreated, state.EvSessionDeleted:
		ses, err := state.DecodeSession(e.Payload)
		if err != nil {
			return err
		}
		return upsertSessionRow(ctx, tx, s, ses)
	case state.EvTurnAppended:
		t, err := state.DecodeTurn(e.Payload)
		if err != nil {
			return err
		}
		ses, err := state.DecodeSession(e.Payload)
		if err != nil {
			return err
		}
		if err := insertTurnRow(ctx, tx, s, t); err != nil {
			return err
		}
		return upsertSessionRow(ctx, tx, s, ses)
	case state.EvSkillUpserted, state.EvSkillDeleted:
		sk, err := state.DecodeSkill(e.Payload)
		if err != nil {
			return err
		}
		return projectSkill(ctx, tx, sk)
	case state.EvMemoryWritten, state.EvMemoryDeleted:
		it, err := state.DecodeMemoryItem(e.Payload)
		if err != nil {
			return err
		}
		return projectMemory(ctx, tx, it)
	case state.EvMemoryPushed, state.EvMemoryUsed:
		rows, err := state.DecodeMemoryUsage(e.Payload)
		if err != nil {
			return err
		}
		return projectMemoryUsage(ctx, tx, rows)
	case state.EvMemoryPromoted:
		p, err := state.DecodeMemoryPromotion(e.Payload)
		if err != nil {
			return err
		}
		return projectMemoryPromotion(ctx, tx, p)
	case state.EvModelUsageRecorded:
		rs, err := state.DecodeModelUsageRecords(e.Payload)
		if err != nil {
			return err
		}
		return projectModelUsage(ctx, tx, rs)
	default:
		return fmt.Errorf("sqlite: unknown state event %q", e.Type)
	}
}

// tx runs fn in one transaction, so a failure changes neither log nor tables.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return sqlitex.Tx(ctx, s.db, fn)
}
