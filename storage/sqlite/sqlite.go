// Package sqlite is the agent's durable, single-file backend. One SQLite database
// (pure-Go modernc.org/sqlite, no cgo) backs every persistence domain: the state
// provider (sessions, skills, memory) and the event spine share one file and one
// connection, so all durable data lives in one place and a state mutation's event
// and its projection commit in one transaction.
//
// Writes go through the command path: every mutation is stamped once by the shared
// state.Stamper (IDs, HLC, versions, the sync envelope, CAS, tombstones), appended
// to the event spine, and projected into the tables, all inside a single
// transaction. The live path projects the typed record the Stamper returned (the
// same post-image the event payload carries, written verbatim from RawPayload);
// applyEvent performs the identical projection from the payload when Rebuild
// reprojects the log, so a rebuilt-from-log database is identical to a live one
// and the event log can never drift from the tables. Both paths share the same
// row-projection helpers, which keeps full event-sourcing reachable: state is a
// fold of the spine.
//
// A Store implements state.Provider directly and exposes the event log via Log().
// Both pass the shared conformance suites (statetest.RunSuite, spinetest.RunSuite),
// so this backend stays byte-for-byte interchangeable with the in-memory ones.
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

// WithInstanceID sets the origin/last-writer instance stamped onto records this
// backend creates (default "local"), so fleet/P2P merge can attribute writes.
func WithInstanceID(id string) Option {
	return func(s *Store) {
		if id != "" {
			s.instanceID = id
		}
	}
}

// WithClock sets the time source for record timestamps, event times, and the
// hybrid logical clock (default: clock.System). Tests and deterministic replay
// pass a clock.Manual.
func WithClock(c clock.Clock) Option {
	return func(s *Store) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithIDGenerator sets the source of record IDs (default: a generator on the
// store's clock with crypto/rand entropy). Supply a generator seeded with a
// deterministic clock and entropy so a re-run with the same seeds produces the
// exact same IDs, the basis of deterministic replay.
func WithIDGenerator(g *ids.Generator) Option {
	return func(s *Store) {
		if g != nil {
			s.gen = g
		}
	}
}

// WithSnapshotCodec makes the resource stream's snapshots verified: Snapshot
// seals the projection payload through the codec before saving it, and Rebuild
// opens (verifies) a stored snapshot through it before restoring - one that fails
// to open is skipped and the stream is folded from the start instead. With a
// codec set, an unsigned or tampered snapshot is never restored.
func WithSnapshotCodec(c spine.SnapshotCodec) Option {
	return func(s *Store) {
		s.snapCodec = c
	}
}

// WithSnapshotEvery makes the resource store checkpoint itself automatically:
// after every k successful mutations it writes a snapshot, so a rebuild folds at
// most k events past the last checkpoint instead of the whole stream. The
// snapshot is written after the mutation's transaction commits, off the hot write
// path, and best effort: a snapshot failure never fails the write (a missing
// snapshot is only slower, never wrong). Zero or negative disables automatic
// snapshots (the default).
func WithSnapshotEvery(k int) Option {
	return func(s *Store) {
		s.snapEvery = k
	}
}

// WithPayloadBlobThreshold sets the stored-byte length at or above which an event
// payload is externalized into the content-addressed blobs table instead of kept inline.
// Zero or negative keeps every payload inline. It overrides defaultPayloadBlobThreshold
// and lets a caller (or a test) force or forgo externalization.
func WithPayloadBlobThreshold(n int) Option {
	return func(s *Store) {
		s.blobThreshold = n
	}
}

// Store is the SQLite backend. It implements state.Provider (sessions, skills,
// memory) and exposes the event spine via Log(), all over one database and one
// connection so cross-domain work shares a single file and transaction.
type Store struct {
	db *sql.DB
	// readDB is the read-only connection pool for a file database (nil for
	// ":memory:", which lives inside its single write connection). Point reads
	// and sweeps run here so they never queue behind the write connection's
	// multi-statement transactions; WAL gives one writer plus N readers, and a
	// lone connection would forgo the N. Readers see only committed state.
	readDB     *sql.DB
	stmts      stmts
	clk        clock.Clock
	hlc        *hlc.Clock
	gen        *ids.Generator
	st         *state.Stamper
	instanceID string
	// jobsReady carries the jobs.Waker signal for the queue in Jobs(). It lives
	// on the Store because Jobs() builds a fresh facade per call: every facade
	// must share one channel or a worker would miss another facade's enqueues.
	jobsReady chan struct{}
	// snapCodec seals resource snapshots on write and verifies them on read (see
	// WithSnapshotCodec); snapEvery/snapPending drive the automatic snapshot
	// cadence (see WithSnapshotEvery). They live on the Store because Resources()
	// builds a fresh facade per call and every facade must share one counter.
	snapCodec   spine.SnapshotCodec
	snapEvery   int
	snapPending atomic.Int64
	// snapPendingState drives the state stream's snapshot cadence, kept separate from
	// snapPending so the resource and state streams checkpoint on their own counts.
	snapPendingState atomic.Int64
	// blobThreshold is the stored-byte length at or above which an event payload is
	// externalized into the content-addressed blobs table instead of kept inline (see
	// resolvePayloadStorage). It defaults to defaultPayloadBlobThreshold; zero or
	// negative keeps every payload inline (see WithPayloadBlobThreshold).
	blobThreshold int
	// warm is the compressed archive of sealed payload bodies (see warm.go). A read that
	// misses the hot blobs table rehydrates from it, and ArchiveSealedBlobs relocates
	// sealed bodies into it to bound the hot file. It is opened beside the hot database
	// and is nil only if opening it failed to be wired (never in normal operation).
	warm *warmStore
}

var _ state.Provider = (*Store)(nil)

// Open opens (creating if needed) a SQLite database at dsn and migrates it to the
// latest schema (the state tables and the event log). dsn is a file path, or
// ":memory:" for an ephemeral store.
//
// The connection pool is capped at a single connection: SQLite serialises writers
// anyway, and one connection keeps a ":memory:" database alive with a consistent
// view. Because every domain shares this one connection, a cross-domain write can
// be one transaction.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	db, err := sqlitex.Open(ctx, dsn, migrations)
	if err != nil {
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

// reads is the handle read-only statements run on: the read pool for a file
// database, the single write connection for ":memory:".
func (s *Store) reads() *sql.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

// stmts holds the hot statements, prepared once at Open. The pure-Go driver
// compiles a statement on every ExecContext/QueryContext call; the point reads
// and the event append below run on every control-loop turn, so each is prepared
// once here and reused (database/sql re-prepares a Stmt per pooled connection
// transparently, then caches it on that connection).
type stmts struct {
	// Write-connection statements, entered into each transaction with
	// tx.StmtContext (a Tx only accepts statements prepared on its own DB).
	eventInsert    *sql.Stmt // the folded assign-seq-and-insert
	sessionGetTx   *sql.Stmt // AppendTurn's session lookup
	turnsMaxSeq    *sql.Stmt // AppendTurn's per-session seq assignment
	turnInsert     *sql.Stmt
	sessionUpsert  *sql.Stmt
	resourceKeyTx  *sql.Stmt // the CAS lookup on every resource Put/Delete
	resourceUpsert *sql.Stmt // the projection write on every resource mutation
	// Read-pool statements.
	eventsRead   *sql.Stmt
	sessionGet   *sql.Stmt
	skillByID    *sql.Stmt
	skillBySlug  *sql.Stmt
	memoryRecall *sql.Stmt // the scoped, no-FTS recall shape (the agent-startup read)
}

// prepare compiles the hot statements. Read statements are prepared on reads()
// so they run on the pool; the transaction-scoped statements are prepared on the
// write connection and entered into each write transaction with tx.StmtContext.
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

// InstanceID is the origin/last-writer instance id this backend stamps onto the
// records it creates. It identifies this process on the fleet, so a running flynn
// can register and address its own Instance resource.
func (s *Store) InstanceID() string { return s.instanceID }

// Sessions returns the durable conversation store.
func (s *Store) Sessions() state.SessionStore { return &sessions{s} }

// Skills returns the scoped, FTS5-searchable skill store.
func (s *Store) Skills() state.SkillStore { return &skills{s} }

// Memory returns the durable memory store.
func (s *Store) Memory() state.MemoryStore { return &memory{s} }

// Log returns the durable event spine backed by the same database, so events and
// state share one file. The returned Log uses the Store's connections and clock
// and is valid until the Store is closed.
func (s *Store) Log() spine.Log { return &eventLog{p: s} }

// Close closes the underlying database (and the read pool of a file database),
// releasing both the state provider and the event log.
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

// commit runs the command path for one mutation: build stamps the record,
// produces the event to append (doing any tx-scoped lookup it needs for CAS),
// and returns the projection step; commit appends the event to the spine and
// runs the projection, both in one transaction. Append-and-project is atomic, so
// the log and the projection can never diverge. The projection writes the typed
// record the Stamper already returned (the same post-image the event payload
// carries), so the live path never decodes what it just encoded; applyEvent
// performs the identical projection from the payload during Rebuild.
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

// Rebuild reprojects the state tables from the event log: it folds the state
// stream through the same applyEvent the live write path uses, so the projection
// is reconciled to the log. It resumes from the stream's latest usable snapshot and
// folds only the events after it, so a rebuild stays bounded as the stream grows; a
// snapshot that cannot be verified (with a codec set) or decoded is skipped and the
// whole stream is folded instead - only slower, never wrong. It is idempotent (every
// record is applied by id), so running it repeatedly is safe.
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

// applyEvent projects one state event into the tables (and the FTS indexes),
// decoding the canonical post-image the Stamper wrote. It is the single source of
// the SQLite projection logic, shared by the live write path (commit) and
// reconstruction (Rebuild), so a rebuilt database is identical to a live one.
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
	default:
		return fmt.Errorf("sqlite: unknown state event %q", e.Type)
	}
}

// --- tx ----------------------------------------------------------------------

// tx runs fn inside a transaction (so a failed append+project leaves neither the
// event nor the projection changed). The shared engine owns the commit/rollback.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return sqlitex.Tx(ctx, s.db, fn)
}
