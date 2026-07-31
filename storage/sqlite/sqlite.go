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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

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
	default:
		return fmt.Errorf("sqlite: unknown state event %q", e.Type)
	}
}

// --- shared helpers ---------------------------------------------------------

func formatTime(t time.Time) string { return sqlitex.FormatTime(t) }

func parseTime(s string) time.Time { return sqlitex.ParseTime(s) }

// hlcTime reconstructs an hlc.Time from its stored columns. The counter column
// only ever holds a uint16 written by this package; the mask makes that explicit
// (and satisfies the integer-overflow checker).
func hlcTime(wall, counter int64) hlc.Time {
	return hlc.Time{Wall: wall, Counter: uint16(counter & 0xffff)}
}

// ftsPhrase wraps a user query as a single FTS5 phrase so arbitrary input is
// matched literally and can never be misread as FTS5 query syntax. Internal
// double quotes are doubled per the FTS5 string-literal rules.
func ftsPhrase(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// --- sessions ---------------------------------------------------------------

type sessions struct{ p *Store }

const sessionCols = `id, title, model, created_at, updated_at,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted`

// sessionGetLiveSQL is the live-session point read, prepared twice at Open: on
// the read pool (Sessions().Get) and on the write connection (the tx-scoped
// lookup in the turn/delete command paths).
const sessionGetLiveSQL = `SELECT ` + sessionCols + ` FROM sessions WHERE id = ? AND deleted = 0`

func scanSession(sc interface{ Scan(...any) error }) (state.Session, error) {
	var (
		s                state.Session
		created, updated string
		wall, counter    int64
		deleted          int
	)
	if err := sc.Scan(&s.ID, &s.Title, &s.Model, &created, &updated,
		&s.SyncVersion, &s.OriginInstanceID, &wall, &counter, &s.LastWriterID, &deleted); err != nil {
		return state.Session{}, err
	}
	s.CreatedAt, s.UpdatedAt = parseTime(created), parseTime(updated)
	s.UpdatedHLC = hlcTime(wall, counter)
	s.Deleted = deleted != 0
	return s, nil
}

// upsertSessionRow writes the session post-image in place. It is an ON CONFLICT
// upsert rather than INSERT OR REPLACE: REPLACE would delete the existing row,
// which the turns foreign key forbids once a session has turns. DO UPDATE mutates
// the row in place, so the projection stays consistent and Rebuild is idempotent.
const upsertSessionSQL = `INSERT INTO sessions (` + sessionCols + `) VALUES (?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET
		title=excluded.title, model=excluded.model,
		created_at=excluded.created_at, updated_at=excluded.updated_at,
		sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
		updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
		last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`

func upsertSessionRow(ctx context.Context, tx *sql.Tx, p *Store, ses state.Session) error {
	_, err := tx.StmtContext(ctx, p.stmts.sessionUpsert).ExecContext(ctx,
		ses.ID, ses.Title, ses.Model, formatTime(ses.CreatedAt), formatTime(ses.UpdatedAt),
		ses.SyncVersion, ses.OriginInstanceID, ses.UpdatedHLC.Wall, int64(ses.UpdatedHLC.Counter), ses.LastWriterID, boolToInt(ses.Deleted))
	return err
}

const insertTurnSQL = `INSERT INTO turns (id, session_id, seq, role, content, created_at,
		sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET
		session_id=excluded.session_id, seq=excluded.seq, role=excluded.role, content=excluded.content,
		created_at=excluded.created_at, sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
		updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
		last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`

// insertTurnRow writes a turn post-image. Turns are append-only, but an upsert by
// id keeps Rebuild idempotent (replaying the same event is a no-op write).
func insertTurnRow(ctx context.Context, tx *sql.Tx, p *Store, t state.Turn) error {
	_, err := tx.StmtContext(ctx, p.stmts.turnInsert).ExecContext(ctx,
		t.ID, t.SessionID, t.Seq, t.Role, t.Content, formatTime(t.CreatedAt),
		t.SyncVersion, t.OriginInstanceID, t.UpdatedHLC.Wall, int64(t.UpdatedHLC.Counter), t.LastWriterID, boolToInt(t.Deleted))
	return err
}

func (s *sessions) Create(ctx context.Context, ses state.Session) (state.Session, error) {
	var rec state.Session
	err := s.p.commit(ctx, func(*sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		r, ev, err := s.p.st.CreateSession(ses)
		rec = r
		return ev, func(tx *sql.Tx) error { return upsertSessionRow(ctx, tx, s.p, r) }, err
	})
	if err != nil {
		return state.Session{}, fmt.Errorf("sqlite: create session: %w", err)
	}
	return rec, nil
}

func (s *sessions) Get(ctx context.Context, id string) (state.Session, error) {
	row := s.p.stmts.sessionGet.QueryRowContext(ctx, id)
	ses, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Session{}, state.ErrNotFound
	}
	return ses, err
}

func (s *sessions) List(ctx context.Context) ([]state.Session, error) {
	rows, err := s.p.reads().QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE deleted = 0 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]state.Session, 0)
	for rows.Next() {
		ses, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ses)
	}
	return out, rows.Err()
}

func (s *sessions) AppendTurn(ctx context.Context, t state.Turn) (state.Turn, error) {
	var rec state.Turn
	err := s.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		ses, err := getSessionTx(ctx, tx, s.p, t.SessionID)
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		var maxSeq int64
		if err := tx.StmtContext(ctx, s.p.stmts.turnsMaxSeq).QueryRowContext(ctx, t.SessionID).Scan(&maxSeq); err != nil {
			return spine.AppendInput{}, nil, err
		}
		r, bumped, ev, err := s.p.st.AppendTurn(ses, t, maxSeq+1)
		rec = r
		return ev, func(tx *sql.Tx) error {
			if err := insertTurnRow(ctx, tx, s.p, r); err != nil {
				return err
			}
			return upsertSessionRow(ctx, tx, s.p, bumped)
		}, err
	})
	return rec, err
}

func (s *sessions) Turns(ctx context.Context, sessionID string) ([]state.Turn, error) {
	rows, err := s.p.reads().QueryContext(ctx,
		`SELECT id, session_id, seq, role, content, created_at,
			sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted
		 FROM turns WHERE session_id = ? AND deleted = 0 ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]state.Turn, 0)
	for rows.Next() {
		var (
			t             state.Turn
			created       string
			wall, counter int64
			deleted       int
		)
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Seq, &t.Role, &t.Content, &created,
			&t.SyncVersion, &t.OriginInstanceID, &wall, &counter, &t.LastWriterID, &deleted); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTime(created)
		t.UpdatedHLC = hlcTime(wall, counter)
		t.Deleted = deleted != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *sessions) Delete(ctx context.Context, id string) error {
	return s.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		ses, err := getSessionTx(ctx, tx, s.p, id)
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		r, ev, err := s.p.st.DeleteSession(ses)
		return ev, func(tx *sql.Tx) error { return upsertSessionRow(ctx, tx, s.p, r) }, err
	})
}

// getSessionTx loads a live session within tx (for the envelope it bumps), or
// returns ErrNotFound if it is missing or tombstoned.
func getSessionTx(ctx context.Context, tx *sql.Tx, p *Store, id string) (state.Session, error) {
	row := tx.StmtContext(ctx, p.stmts.sessionGetTx).QueryRowContext(ctx, id)
	ses, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Session{}, state.ErrNotFound
	}
	return ses, err
}

// --- skills -----------------------------------------------------------------

type skills struct{ p *Store }

// skillCols matches the skills table column order.
const skillCols = `id, slug, name, body, tags, uses, wins, check_cmd, scope_instance, scope_project, scope_workspace,
	version, created_at, updated_at,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted`

func scanSkill(sc interface{ Scan(...any) error }) (state.Skill, error) {
	var (
		s                state.Skill
		tags             string
		created, updated string
		wall, counter    int64
		deleted          int
	)
	if err := sc.Scan(&s.ID, &s.Slug, &s.Name, &s.Body, &tags, &s.Uses, &s.Wins, &s.Check,
		&s.Scope.Instance, &s.Scope.Project, &s.Scope.Workspace,
		&s.Version, &created, &updated,
		&s.SyncVersion, &s.OriginInstanceID, &wall, &counter, &s.LastWriterID, &deleted); err != nil {
		return state.Skill{}, err
	}
	s.CreatedAt, s.UpdatedAt = parseTime(created), parseTime(updated)
	s.UpdatedHLC = hlcTime(wall, counter)
	s.Deleted = deleted != 0
	if tags != "" && tags != "[]" {
		_ = json.Unmarshal([]byte(tags), &s.Tags)
	}
	return s, nil
}

func upsertSkillRow(ctx context.Context, tx *sql.Tx, sk state.Skill) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO skills (`+skillCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
			slug=excluded.slug, name=excluded.name, body=excluded.body, tags=excluded.tags,
			uses=excluded.uses, wins=excluded.wins, check_cmd=excluded.check_cmd,
			scope_instance=excluded.scope_instance, scope_project=excluded.scope_project, scope_workspace=excluded.scope_workspace,
			version=excluded.version, created_at=excluded.created_at, updated_at=excluded.updated_at,
			sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`,
		sk.ID, sk.Slug, sk.Name, sk.Body, marshalTags(sk.Tags), sk.Uses, sk.Wins, sk.Check,
		sk.Scope.Instance, sk.Scope.Project, sk.Scope.Workspace,
		sk.Version, formatTime(sk.CreatedAt), formatTime(sk.UpdatedAt),
		sk.SyncVersion, sk.OriginInstanceID, sk.UpdatedHLC.Wall, int64(sk.UpdatedHLC.Counter), sk.LastWriterID, boolToInt(sk.Deleted))
	return err
}

func (s *skills) Upsert(ctx context.Context, sk state.Skill) (state.Skill, error) {
	var rec state.Skill
	err := s.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		existing, err := getSkillBySlugTx(ctx, tx, sk.Scope, sk.Slug)
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		r, ev, err := s.p.st.UpsertSkill(existing, sk)
		rec = r
		return ev, func(tx *sql.Tx) error { return projectSkill(ctx, tx, r) }, err
	})
	if err != nil {
		return state.Skill{}, err
	}
	return rec, nil
}

func (s *skills) Get(ctx context.Context, idOrSlug string) (state.Skill, error) {
	row := s.p.stmts.skillByID.QueryRowContext(ctx, idOrSlug)
	sk, err := scanSkill(row)
	if err == nil {
		return sk, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Skill{}, err
	}
	row = s.p.stmts.skillBySlug.QueryRowContext(ctx, idOrSlug)
	sk, err = scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Skill{}, state.ErrNotFound
	}
	return sk, err
}

func (s *skills) List(ctx context.Context, scope state.Scope) ([]state.Skill, error) {
	rows, err := s.p.reads().QueryContext(ctx,
		`SELECT `+skillCols+` FROM skills WHERE scope_instance = ? AND scope_project = ? AND scope_workspace = ? AND deleted = 0 ORDER BY slug`,
		scope.Instance, scope.Project, scope.Workspace)
	if err != nil {
		return nil, err
	}
	return collectSkills(rows)
}

func (s *skills) Search(ctx context.Context, query string, limit int) ([]state.Skill, error) {
	q := strings.TrimSpace(query)
	var (
		rows *sql.Rows
		err  error
	)
	if q == "" {
		// An empty query matches everything, ordered by slug (FTS5 rejects an
		// empty MATCH), capped at limit.
		sqlStr := `SELECT ` + skillCols + ` FROM skills WHERE deleted = 0 ORDER BY slug`
		if limit > 0 {
			sqlStr += ` LIMIT ?`
			rows, err = s.p.reads().QueryContext(ctx, sqlStr, limit)
		} else {
			rows, err = s.p.reads().QueryContext(ctx, sqlStr)
		}
	} else {
		sqlStr := `SELECT s.id, s.slug, s.name, s.body, s.tags, s.uses, s.wins, s.check_cmd, s.scope_instance, s.scope_project, s.scope_workspace,
			s.version, s.created_at, s.updated_at,
			s.sync_version, s.origin_instance_id, s.updated_hlc_wall, s.updated_hlc_counter, s.last_writer_id, s.deleted
			FROM skills s JOIN skills_fts f ON f.skill_id = s.id
			WHERE f.skills_fts MATCH ? AND s.deleted = 0 ORDER BY s.slug`
		if limit > 0 {
			sqlStr += ` LIMIT ?`
			rows, err = s.p.reads().QueryContext(ctx, sqlStr, ftsPhrase(q), limit)
		} else {
			rows, err = s.p.reads().QueryContext(ctx, sqlStr, ftsPhrase(q))
		}
	}
	if err != nil {
		return nil, err
	}
	return collectSkills(rows)
}

func (s *skills) Delete(ctx context.Context, idOrSlug string) error {
	return s.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		existing, err := getLiveSkillTx(ctx, tx, idOrSlug)
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		r, ev, err := s.p.st.DeleteSkill(existing)
		return ev, func(tx *sql.Tx) error { return projectSkill(ctx, tx, r) }, err
	})
}

func collectSkills(rows *sql.Rows) ([]state.Skill, error) {
	defer func() { _ = rows.Close() }()
	out := make([]state.Skill, 0)
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// getSkillBySlugTx loads the stored skill for (scope, slug) within tx, tombstones
// included so an upsert over a tombstone can resurrect it (the row holds the
// slot). It returns nil when no row exists.
func getSkillBySlugTx(ctx context.Context, tx *sql.Tx, scope state.Scope, slug string) (*state.Skill, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+skillCols+` FROM skills
		 WHERE scope_instance = ? AND scope_project = ? AND scope_workspace = ? AND slug = ?`,
		scope.Instance, scope.Project, scope.Workspace, slug)
	sk, err := scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sk, nil
}

// getLiveSkillTx loads a live skill by id or slug within tx, or returns
// ErrNotFound.
func getLiveSkillTx(ctx context.Context, tx *sql.Tx, idOrSlug string) (state.Skill, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+skillCols+` FROM skills WHERE id = ? AND deleted = 0`, idOrSlug)
	sk, err := scanSkill(row)
	if err == nil {
		return sk, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Skill{}, err
	}
	row = tx.QueryRowContext(ctx, `SELECT `+skillCols+` FROM skills WHERE slug = ? AND deleted = 0 ORDER BY created_at, id LIMIT 1`, idOrSlug)
	sk, err = scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Skill{}, state.ErrNotFound
	}
	return sk, err
}

// projectSkill writes a skill post-image: the row and its FTS index together.
// Shared by the live command path and applyEvent (Rebuild), so both project
// identically.
func projectSkill(ctx context.Context, tx *sql.Tx, sk state.Skill) error {
	if err := upsertSkillRow(ctx, tx, sk); err != nil {
		return err
	}
	return reindexSkill(ctx, tx, sk)
}

// reindexSkill rewrites a skill's FTS row so search reflects the latest content,
// and holds an entry only while the skill is live.
func reindexSkill(ctx context.Context, tx *sql.Tx, sk state.Skill) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM skills_fts WHERE skill_id = ?`, sk.ID); err != nil {
		return err
	}
	if sk.Deleted {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO skills_fts (skill_id, name, body, tags) VALUES (?,?,?,?)`,
		sk.ID, sk.Name, sk.Body, strings.Join(sk.Tags, " "))
	return err
}

// --- memory -----------------------------------------------------------------

type memory struct{ p *Store }

const memoryCols = `id, kind, content, scope_instance, scope_project, scope_workspace, sources, created_at, expires_at,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted`

// memoryColsQualified is memoryCols against the `m` alias, for the recall query,
// which joins the FTS table and so cannot use bare column names. Recall appends
// a sixteenth expression, the relevance score, after these fifteen.
const memoryColsQualified = `m.id, m.kind, m.content, m.scope_instance, m.scope_project, m.scope_workspace, m.sources, m.created_at, m.expires_at,
	m.sync_version, m.origin_instance_id, m.updated_hlc_wall, m.updated_hlc_counter, m.last_writer_id, m.deleted`

// memoryLiveSQL is the predicate for a row a recall may return: not tombstoned,
// and not past its expiry as of the bound instant. Both recall shapes use it, so
// the prepared single-scope statement and the built query cannot disagree on what
// "live" means. It binds one parameter, the read time in unix nanoseconds.
const memoryLiveSQL = `m.deleted = 0 AND (m.expires_at = 0 OR m.expires_at > ?)`

// encodeSources renders provenance for storage as a JSON array. Empty provenance
// is the empty string rather than "[]" so an item that never had a source keeps a
// cheap, obviously-empty column value.
//
// It returns no error because it cannot fail: encoding a []string is total, so
// dropping the error is honest, where a defensive error return would add a branch
// no test could ever reach and no caller could ever handle.
func encodeSources(sources []string) string {
	if len(sources) == 0 {
		return ""
	}
	b, _ := json.Marshal(sources)
	return string(b)
}

// decodeSources reads back what encodeSources wrote.
func decodeSources(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("sqlite: decode memory sources: %w", err)
	}
	return out, nil
}

// expiryNanos renders an expiry for storage: unix nanoseconds, with 0 reserved for
// "never", which is what the zero time means on the item.
func expiryNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// expiryTime is expiryNanos in reverse.
func expiryTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// scanMemoryRow scans the fifteen memory columns, and the trailing relevance score
// too when score is non-nil (the recall shapes select it as a sixteenth
// expression). One scanner so the column list and its readers cannot drift apart.
func scanMemoryRow(sc interface{ Scan(...any) error }, score *float64) (state.MemoryItem, error) {
	var (
		m             state.MemoryItem
		sources       string
		created       string
		expires       int64
		wall, counter int64
		deleted       int
	)
	dst := []any{
		&m.ID, &m.Kind, &m.Content,
		&m.Scope.Instance, &m.Scope.Project, &m.Scope.Workspace, &sources, &created, &expires,
		&m.SyncVersion, &m.OriginInstanceID, &wall, &counter, &m.LastWriterID, &deleted,
	}
	if score != nil {
		dst = append(dst, score)
	}
	if err := sc.Scan(dst...); err != nil {
		return state.MemoryItem{}, err
	}
	decoded, err := decodeSources(sources)
	if err != nil {
		return state.MemoryItem{}, err
	}
	m.Sources = decoded
	m.CreatedAt = parseTime(created)
	m.ExpiresAt = expiryTime(expires)
	m.UpdatedHLC = hlcTime(wall, counter)
	m.Deleted = deleted != 0
	if score != nil {
		m.Score = *score
	}
	return m, nil
}

func scanMemory(sc interface{ Scan(...any) error }) (state.MemoryItem, error) {
	return scanMemoryRow(sc, nil)
}

// scanScoredMemory scans a recall row: the item's columns plus the trailing
// relevance score.
func scanScoredMemory(sc interface{ Scan(...any) error }) (state.MemoryItem, error) {
	var score float64
	return scanMemoryRow(sc, &score)
}

func upsertMemoryRow(ctx context.Context, tx *sql.Tx, it state.MemoryItem) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memory_items (`+memoryCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
			kind=excluded.kind, content=excluded.content,
			scope_instance=excluded.scope_instance, scope_project=excluded.scope_project, scope_workspace=excluded.scope_workspace,
			sources=excluded.sources, created_at=excluded.created_at, expires_at=excluded.expires_at,
			sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`,
		it.ID, it.Kind, it.Content, it.Scope.Instance, it.Scope.Project, it.Scope.Workspace, encodeSources(it.Sources),
		formatTime(it.CreatedAt), expiryNanos(it.ExpiresAt),
		it.SyncVersion, it.OriginInstanceID, it.UpdatedHLC.Wall, int64(it.UpdatedHLC.Counter), it.LastWriterID, boolToInt(it.Deleted))
	return err
}

// projectMemory writes a memory-item post-image: the row and its FTS index
// together. Shared by the live command path and applyEvent (Rebuild), so both
// project identically.
func projectMemory(ctx context.Context, tx *sql.Tx, it state.MemoryItem) error {
	if err := upsertMemoryRow(ctx, tx, it); err != nil {
		return err
	}
	return reindexMemory(ctx, tx, it)
}

// reindexMemory keeps the memory FTS index holding an entry only while the item
// is live, so a tombstone drops out of recall.
func reindexMemory(ctx context.Context, tx *sql.Tx, it state.MemoryItem) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE item_id = ?`, it.ID); err != nil {
		return err
	}
	if it.Deleted {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_fts (item_id, content) VALUES (?, ?)`, it.ID, it.Content)
	return err
}

func (m *memory) Write(ctx context.Context, it state.MemoryItem) (state.MemoryItem, error) {
	var rec state.MemoryItem
	err := m.p.commit(ctx, func(*sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		r, ev, err := m.p.st.WriteMemory(it)
		rec = r
		return ev, func(tx *sql.Tx) error { return projectMemory(ctx, tx, r) }, err
	})
	if err != nil {
		return state.MemoryItem{}, err
	}
	return rec, nil
}

func (m *memory) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	query := strings.TrimSpace(q.Query)
	chain := q.ScopeChain()
	// One clock reading bound into both recall shapes and into the Go post-filter,
	// so a single call cannot judge two rows against two different instants. This is
	// the read time itself, not an expiry, so it goes through UnixNano directly:
	// expiryNanos reserves 0 for "never", which is a meaningless answer for a clock.
	now := m.p.clk.Now()
	liveAt := now.UnixNano()

	// The single-scope, no-FTS shape is the agent-startup read; it runs on the
	// prepared statement (a non-positive Limit becomes SQLite's LIMIT -1, no
	// limit, so both limit shapes share it). A widened read cannot: the prepared
	// statement binds exactly one scope triple, so it falls through to the built
	// query below, which is the only shape that can express a variable-length
	// resolution chain.
	// Anything the query language cannot answer correctly is applied after the
	// rows come back, and forces the cap to be applied there too - a SQL LIMIT
	// would otherwise truncate rows that the Go stage was going to drop anyway,
	// returning fewer results than asked for.
	postFilter := !q.Since.IsZero() || !q.Until.IsZero() || q.MinScore > 0

	if query == "" && len(chain) == 1 && len(q.Kinds) == 0 && !postFilter {
		limit := q.Limit
		if limit <= 0 {
			limit = -1
		}
		rows, err := m.p.stmts.memoryRecall.QueryContext(ctx, liveAt, q.Scope.Instance, q.Scope.Project, q.Scope.Workspace, limit)
		if err != nil {
			return nil, err
		}
		items, err := collectMemory(rows)
		if err != nil {
			return nil, err
		}
		// No query, so nothing was graded and every row is an equally good match.
		for i := range items {
			items[i].Score = 1
		}
		return items, nil
	}

	var sb strings.Builder
	args := make([]any, 0, 8)
	if query == "" {
		// Nothing to rank against, so every row scores 1: state.MemoryItem.Score
		// reserves 1 for "matched, no opinion on how well".
		sb.WriteString(`SELECT ` + memoryColsQualified + `, 1.0
			FROM memory_items m WHERE ` + memoryLiveSQL)
		args = append(args, liveAt)
	} else {
		// FTS5 computes bm25 for the MATCH regardless; the contract used to discard
		// it. bm25 is <= 0 with a more negative value meaning a better match, so
		// -b/(1-b) maps it onto [0,1) increasing in match quality, which is the
		// direction and range Score is defined in.
		sb.WriteString(`SELECT ` + memoryColsQualified + `, (-bm25(memory_fts)) / (1.0 - bm25(memory_fts))
			FROM memory_items m JOIN memory_fts ON memory_fts.item_id = m.id
			WHERE memory_fts MATCH ? AND ` + memoryLiveSQL)
		args = append(args, ftsPhrase(query), liveAt)
	}
	// Kind is an exact match on an indexed column, so it belongs in the query
	// rather than in a post-filter: it cuts the rows the sort has to order.
	for i, k := range q.Kinds {
		if i == 0 {
			sb.WriteString(` AND m.kind IN (`)
		} else {
			sb.WriteString(`, `)
		}
		sb.WriteString(`?`)
		args = append(args, k)
	}
	if len(q.Kinds) > 0 {
		sb.WriteString(`)`)
	}
	// The chain is one scope, or that scope's ancestors when the read widened, so
	// the predicate is an OR over its triples. Nil means unfiltered, no predicate.
	for i, sc := range chain {
		if i == 0 {
			sb.WriteString(` AND (`)
		} else {
			sb.WriteString(` OR `)
		}
		sb.WriteString(`(m.scope_instance = ? AND m.scope_project = ? AND m.scope_workspace = ?)`)
		args = append(args, sc.Instance, sc.Project, sc.Workspace)
	}
	if len(chain) > 0 {
		sb.WriteString(`)`)
	}
	// A widened recall ranks most-specific scope first, matching state.Scope.Depth
	// and state.SortRecall, so a workspace's own memory outranks the project
	// memory it inherits. The CASE takes no arguments because it reads the
	// innermost set column rather than comparing against the chain: within one
	// ancestor chain every level has a distinct innermost column, which is exactly
	// what Depth reports. It has to be ordered in SQL rather than after collection,
	// because LIMIT would otherwise truncate the wrong rows.
	sb.WriteString(` ORDER BY`)
	if q.Order == state.OrderRelevance {
		// Column 16 is the score expression. Ordering by ordinal rather than
		// repeating the expression keeps bm25 evaluated once per row.
		sb.WriteString(` 16 DESC,`)
	}
	if q.RanksByScope() {
		sb.WriteString(` CASE
			WHEN m.scope_workspace <> '' THEN 0
			WHEN m.scope_project <> '' THEN 1
			WHEN m.scope_instance <> '' THEN 2
			ELSE 3 END,`)
	}
	sb.WriteString(` m.created_at DESC, m.id DESC`)
	if q.Limit > 0 && !postFilter {
		sb.WriteString(` LIMIT ?`)
		args = append(args, q.Limit)
	}

	rows, err := m.p.reads().QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	items, err := collectScoredMemory(rows)
	if err != nil {
		return nil, err
	}
	if !postFilter {
		return items, nil
	}
	// The CreatedAt window is applied here rather than as a SQL range: created_at
	// is stored as RFC3339Nano, which drops trailing zeros from the fractional
	// second, so it is not fixed-width and does not compare lexicographically
	// ("...T00:00:00.000000001Z" sorts before "...T00:00:00Z"). Comparing parsed
	// times is the only correct answer available here.
	out := items[:0]
	for _, it := range items {
		if !q.Selects(it, now) || it.Score < q.MinScore {
			continue
		}
		out = append(out, it)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

// collectMemory drains rows into memory items, closing rows on every path.
func collectMemory(rows *sql.Rows) ([]state.MemoryItem, error) {
	defer func() { _ = rows.Close() }()
	out := make([]state.MemoryItem, 0)
	for rows.Next() {
		it, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// collectScoredMemory drains recall rows, which carry a trailing score column.
func collectScoredMemory(rows *sql.Rows) ([]state.MemoryItem, error) {
	defer func() { _ = rows.Close() }()
	out := make([]state.MemoryItem, 0)
	for rows.Next() {
		it, err := scanScoredMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (m *memory) Delete(ctx context.Context, id string) error {
	return m.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		existing, err := getLiveMemoryTx(ctx, tx, id)
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		r, ev, err := m.p.st.DeleteMemory(existing)
		return ev, func(tx *sql.Tx) error { return projectMemory(ctx, tx, r) }, err
	})
}

// getLiveMemoryTx loads a live memory item by id within tx, or returns
// ErrNotFound.
func getLiveMemoryTx(ctx context.Context, tx *sql.Tx, id string) (state.MemoryItem, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+memoryCols+` FROM memory_items WHERE id = ? AND deleted = 0`, id)
	it, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.MemoryItem{}, state.ErrNotFound
	}
	return it, err
}

// --- tx ----------------------------------------------------------------------

// tx runs fn inside a transaction (so a failed append+project leaves neither the
// event nor the projection changed). The shared engine owns the commit/rollback.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return sqlitex.Tx(ctx, s.db, fn)
}
