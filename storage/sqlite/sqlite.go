// Package sqlite is the agent's durable, single-file backend. One SQLite database
// (pure-Go modernc.org/sqlite, no cgo) backs every persistence domain: the state
// provider (sessions, skills, memory) and the event spine share one file and one
// connection, so all durable data lives in one place and a state mutation's event
// and its projection commit in one transaction.
//
// Writes go through the command path: every mutation is stamped once by the shared
// state.Stamper (IDs, HLC, versions, the sync envelope, CAS, tombstones), appended
// to the event spine, and projected into the tables by applyEvent, all inside a
// single transaction. The same applyEvent reprojects the log in Rebuild, so a
// rebuilt-from-log database is identical to a live one and the event log can never
// drift from the tables. No write touches a projection table except through
// applyEvent, which keeps full event-sourcing reachable: state is a fold of the
// spine.
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

// Store is the SQLite backend. It implements state.Provider (sessions, skills,
// memory) and exposes the event spine via Log(), all over one database and one
// connection so cross-domain work shares a single file and transaction.
type Store struct {
	db         *sql.DB
	clk        clock.Clock
	hlc        *hlc.Clock
	gen        *ids.Generator
	st         *state.Stamper
	instanceID string
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
	s := &Store{db: db, clk: clock.System{}, instanceID: "local"}
	for _, o := range opts {
		o(s)
	}
	if s.gen == nil {
		s.gen = ids.NewGenerator(ids.WithClock(s.clk))
	}
	s.hlc = hlc.NewClock(hlc.WithPhysical(s.clk))
	s.st = state.NewStamper(s.instanceID, s.clk, s.hlc, s.gen)
	return s, nil
}

// Name identifies the backend ("sqlite").
func (s *Store) Name() string { return "sqlite" }

// Sessions returns the durable conversation store.
func (s *Store) Sessions() state.SessionStore { return &sessions{s} }

// Skills returns the scoped, FTS5-searchable skill store.
func (s *Store) Skills() state.SkillStore { return &skills{s} }

// Memory returns the durable memory store.
func (s *Store) Memory() state.MemoryStore { return &memory{s} }

// Log returns the durable event spine backed by the same database, so events and
// state share one file. The returned Log uses the Store's connection and clock and
// is valid until the Store is closed.
func (s *Store) Log() spine.Log { return &eventLog{db: s.db, clk: s.clk} }

// Close closes the underlying database, releasing both the state provider and the
// event log.
func (s *Store) Close() error { return s.db.Close() }

// commit runs the command path for one mutation: build stamps the record and
// produces the event to append (doing any tx-scoped lookup it needs for CAS),
// then commit appends the event to the spine and projects it into the tables,
// both in one transaction. Append-and-project is atomic, so the log and the
// projection can never diverge.
func (s *Store) commit(ctx context.Context, build func(tx *sql.Tx) (spine.AppendInput, error)) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		in, err := build(tx)
		if err != nil {
			return err
		}
		e, err := appendTx(ctx, tx, s.clk, in)
		if err != nil {
			return err
		}
		return s.applyEvent(ctx, tx, e)
	})
}

// Rebuild reprojects the state tables from the event log: it folds the state
// stream through the same applyEvent the live write path uses, so the projection
// is reconciled to the log. It is idempotent (every event is the post-image of its
// record, applied by id), so running it repeatedly is safe. Its existence is the
// proof that the log is the source of truth and the tables are a derived view.
func (s *Store) Rebuild(ctx context.Context) error {
	events, err := s.Log().Read(ctx, spine.Query{Stream: state.StateStream})
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
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
		return upsertSessionRow(ctx, tx, ses)
	case state.EvTurnAppended:
		t, err := state.DecodeTurn(e.Payload)
		if err != nil {
			return err
		}
		ses, err := state.DecodeSession(e.Payload)
		if err != nil {
			return err
		}
		if err := insertTurnRow(ctx, tx, t); err != nil {
			return err
		}
		return upsertSessionRow(ctx, tx, ses)
	case state.EvSkillUpserted, state.EvSkillDeleted:
		sk, err := state.DecodeSkill(e.Payload)
		if err != nil {
			return err
		}
		if err := upsertSkillRow(ctx, tx, sk); err != nil {
			return err
		}
		return reindexSkill(ctx, tx, sk)
	case state.EvMemoryWritten, state.EvMemoryDeleted:
		it, err := state.DecodeMemoryItem(e.Payload)
		if err != nil {
			return err
		}
		if err := upsertMemoryRow(ctx, tx, it); err != nil {
			return err
		}
		return reindexMemory(ctx, tx, it)
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
func upsertSessionRow(ctx context.Context, tx *sql.Tx, ses state.Session) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (`+sessionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
			title=excluded.title, model=excluded.model,
			created_at=excluded.created_at, updated_at=excluded.updated_at,
			sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`,
		ses.ID, ses.Title, ses.Model, formatTime(ses.CreatedAt), formatTime(ses.UpdatedAt),
		ses.SyncVersion, ses.OriginInstanceID, ses.UpdatedHLC.Wall, int64(ses.UpdatedHLC.Counter), ses.LastWriterID, boolToInt(ses.Deleted))
	return err
}

// insertTurnRow writes a turn post-image. Turns are append-only, but an upsert by
// id keeps Rebuild idempotent (replaying the same event is a no-op write).
func insertTurnRow(ctx context.Context, tx *sql.Tx, t state.Turn) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO turns (id, session_id, seq, role, content, created_at,
			sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
			session_id=excluded.session_id, seq=excluded.seq, role=excluded.role, content=excluded.content,
			created_at=excluded.created_at, sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`,
		t.ID, t.SessionID, t.Seq, t.Role, t.Content, formatTime(t.CreatedAt),
		t.SyncVersion, t.OriginInstanceID, t.UpdatedHLC.Wall, int64(t.UpdatedHLC.Counter), t.LastWriterID, boolToInt(t.Deleted))
	return err
}

func (s *sessions) Create(ctx context.Context, ses state.Session) (state.Session, error) {
	var rec state.Session
	err := s.p.commit(ctx, func(*sql.Tx) (spine.AppendInput, error) {
		r, ev, err := s.p.st.CreateSession(ses)
		rec = r
		return ev, err
	})
	if err != nil {
		return state.Session{}, fmt.Errorf("sqlite: create session: %w", err)
	}
	return rec, nil
}

func (s *sessions) Get(ctx context.Context, id string) (state.Session, error) {
	row := s.p.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ? AND deleted = 0`, id)
	ses, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Session{}, state.ErrNotFound
	}
	return ses, err
}

func (s *sessions) List(ctx context.Context) ([]state.Session, error) {
	rows, err := s.p.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE deleted = 0 ORDER BY created_at, id`)
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
	err := s.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, error) {
		ses, err := getSessionTx(ctx, tx, t.SessionID)
		if err != nil {
			return spine.AppendInput{}, err
		}
		var maxSeq int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM turns WHERE session_id = ?`, t.SessionID).Scan(&maxSeq); err != nil {
			return spine.AppendInput{}, err
		}
		r, _, ev, err := s.p.st.AppendTurn(ses, t, maxSeq+1)
		rec = r
		return ev, err
	})
	return rec, err
}

func (s *sessions) Turns(ctx context.Context, sessionID string) ([]state.Turn, error) {
	rows, err := s.p.db.QueryContext(ctx,
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
	return s.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, error) {
		ses, err := getSessionTx(ctx, tx, id)
		if err != nil {
			return spine.AppendInput{}, err
		}
		_, ev, err := s.p.st.DeleteSession(ses)
		return ev, err
	})
}

// getSessionTx loads a live session within tx (for the envelope it bumps), or
// returns ErrNotFound if it is missing or tombstoned.
func getSessionTx(ctx context.Context, tx *sql.Tx, id string) (state.Session, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ? AND deleted = 0`, id)
	ses, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Session{}, state.ErrNotFound
	}
	return ses, err
}

// --- skills -----------------------------------------------------------------

type skills struct{ p *Store }

// skillCols matches the skills table column order.
const skillCols = `id, slug, name, body, tags, uses, wins, scope_instance, scope_project, scope_workspace,
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
	if err := sc.Scan(&s.ID, &s.Slug, &s.Name, &s.Body, &tags, &s.Uses, &s.Wins,
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
		`INSERT INTO skills (`+skillCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
			slug=excluded.slug, name=excluded.name, body=excluded.body, tags=excluded.tags,
			uses=excluded.uses, wins=excluded.wins,
			scope_instance=excluded.scope_instance, scope_project=excluded.scope_project, scope_workspace=excluded.scope_workspace,
			version=excluded.version, created_at=excluded.created_at, updated_at=excluded.updated_at,
			sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`,
		sk.ID, sk.Slug, sk.Name, sk.Body, marshalTags(sk.Tags), sk.Uses, sk.Wins,
		sk.Scope.Instance, sk.Scope.Project, sk.Scope.Workspace,
		sk.Version, formatTime(sk.CreatedAt), formatTime(sk.UpdatedAt),
		sk.SyncVersion, sk.OriginInstanceID, sk.UpdatedHLC.Wall, int64(sk.UpdatedHLC.Counter), sk.LastWriterID, boolToInt(sk.Deleted))
	return err
}

func (s *skills) Upsert(ctx context.Context, sk state.Skill) (state.Skill, error) {
	var rec state.Skill
	err := s.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, error) {
		existing, err := getSkillBySlugTx(ctx, tx, sk.Scope, sk.Slug)
		if err != nil {
			return spine.AppendInput{}, err
		}
		r, ev, err := s.p.st.UpsertSkill(existing, sk)
		rec = r
		return ev, err
	})
	if err != nil {
		return state.Skill{}, err
	}
	return rec, nil
}

func (s *skills) Get(ctx context.Context, idOrSlug string) (state.Skill, error) {
	row := s.p.db.QueryRowContext(ctx, `SELECT `+skillCols+` FROM skills WHERE id = ? AND deleted = 0`, idOrSlug)
	sk, err := scanSkill(row)
	if err == nil {
		return sk, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Skill{}, err
	}
	row = s.p.db.QueryRowContext(ctx, `SELECT `+skillCols+` FROM skills WHERE slug = ? AND deleted = 0 ORDER BY created_at, id LIMIT 1`, idOrSlug)
	sk, err = scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Skill{}, state.ErrNotFound
	}
	return sk, err
}

func (s *skills) List(ctx context.Context, scope state.Scope) ([]state.Skill, error) {
	rows, err := s.p.db.QueryContext(ctx,
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
			rows, err = s.p.db.QueryContext(ctx, sqlStr, limit)
		} else {
			rows, err = s.p.db.QueryContext(ctx, sqlStr)
		}
	} else {
		sqlStr := `SELECT s.id, s.slug, s.name, s.body, s.tags, s.uses, s.wins, s.scope_instance, s.scope_project, s.scope_workspace,
			s.version, s.created_at, s.updated_at,
			s.sync_version, s.origin_instance_id, s.updated_hlc_wall, s.updated_hlc_counter, s.last_writer_id, s.deleted
			FROM skills s JOIN skills_fts f ON f.skill_id = s.id
			WHERE f.skills_fts MATCH ? AND s.deleted = 0 ORDER BY s.slug`
		if limit > 0 {
			sqlStr += ` LIMIT ?`
			rows, err = s.p.db.QueryContext(ctx, sqlStr, ftsPhrase(q), limit)
		} else {
			rows, err = s.p.db.QueryContext(ctx, sqlStr, ftsPhrase(q))
		}
	}
	if err != nil {
		return nil, err
	}
	return collectSkills(rows)
}

func (s *skills) Delete(ctx context.Context, idOrSlug string) error {
	return s.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, error) {
		existing, err := getLiveSkillTx(ctx, tx, idOrSlug)
		if err != nil {
			return spine.AppendInput{}, err
		}
		_, ev, err := s.p.st.DeleteSkill(existing)
		return ev, err
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

const memoryCols = `id, kind, content, scope_instance, scope_project, scope_workspace, source, created_at,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted`

func scanMemory(sc interface{ Scan(...any) error }) (state.MemoryItem, error) {
	var (
		m             state.MemoryItem
		created       string
		wall, counter int64
		deleted       int
	)
	if err := sc.Scan(&m.ID, &m.Kind, &m.Content,
		&m.Scope.Instance, &m.Scope.Project, &m.Scope.Workspace, &m.Source, &created,
		&m.SyncVersion, &m.OriginInstanceID, &wall, &counter, &m.LastWriterID, &deleted); err != nil {
		return state.MemoryItem{}, err
	}
	m.CreatedAt = parseTime(created)
	m.UpdatedHLC = hlcTime(wall, counter)
	m.Deleted = deleted != 0
	return m, nil
}

func upsertMemoryRow(ctx context.Context, tx *sql.Tx, it state.MemoryItem) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memory_items (`+memoryCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
			kind=excluded.kind, content=excluded.content,
			scope_instance=excluded.scope_instance, scope_project=excluded.scope_project, scope_workspace=excluded.scope_workspace,
			source=excluded.source, created_at=excluded.created_at,
			sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`,
		it.ID, it.Kind, it.Content, it.Scope.Instance, it.Scope.Project, it.Scope.Workspace, it.Source,
		formatTime(it.CreatedAt), it.SyncVersion, it.OriginInstanceID, it.UpdatedHLC.Wall, int64(it.UpdatedHLC.Counter), it.LastWriterID, boolToInt(it.Deleted))
	return err
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
	err := m.p.commit(ctx, func(*sql.Tx) (spine.AppendInput, error) {
		r, ev, err := m.p.st.WriteMemory(it)
		rec = r
		return ev, err
	})
	if err != nil {
		return state.MemoryItem{}, err
	}
	return rec, nil
}

func (m *memory) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	query := strings.TrimSpace(q.Query)
	scoped := q.Scope != (state.Scope{})

	var sb strings.Builder
	args := make([]any, 0, 5)
	if query == "" {
		sb.WriteString(`SELECT m.id, m.kind, m.content, m.scope_instance, m.scope_project, m.scope_workspace, m.source, m.created_at,
			m.sync_version, m.origin_instance_id, m.updated_hlc_wall, m.updated_hlc_counter, m.last_writer_id, m.deleted
			FROM memory_items m WHERE m.deleted = 0`)
	} else {
		sb.WriteString(`SELECT m.id, m.kind, m.content, m.scope_instance, m.scope_project, m.scope_workspace, m.source, m.created_at,
			m.sync_version, m.origin_instance_id, m.updated_hlc_wall, m.updated_hlc_counter, m.last_writer_id, m.deleted
			FROM memory_items m JOIN memory_fts f ON f.item_id = m.id
			WHERE f.memory_fts MATCH ? AND m.deleted = 0`)
		args = append(args, ftsPhrase(query))
	}
	if scoped {
		sb.WriteString(` AND m.scope_instance = ? AND m.scope_project = ? AND m.scope_workspace = ?`)
		args = append(args, q.Scope.Instance, q.Scope.Project, q.Scope.Workspace)
	}
	sb.WriteString(` ORDER BY m.created_at DESC, m.id DESC`)
	if q.Limit > 0 {
		sb.WriteString(` LIMIT ?`)
		args = append(args, q.Limit)
	}

	rows, err := m.p.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
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

func (m *memory) Delete(ctx context.Context, id string) error {
	return m.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, error) {
		existing, err := getLiveMemoryTx(ctx, tx, id)
		if err != nil {
			return spine.AppendInput{}, err
		}
		_, ev, err := m.p.st.DeleteMemory(existing)
		return ev, err
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
