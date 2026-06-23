// Package sqlite is the durable, single-file state.Provider: the standalone
// agent's persistent backend. It is a pure-Go build (modernc.org/sqlite, no cgo)
// so the agent stays a single static binary.
//
// It passes the same statetest.RunSuite conformance suite as the in-memory
// provider, so the two are byte-for-byte interchangeable. Each write is a "dual
// write": the projection table and its FTS5 search index are updated inside one
// transaction, so search can never drift from the records it indexes.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/migrate"
	"github.com/ionalpha/flynn/state"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Option configures the provider.
type Option func(*Provider)

// WithInstanceID sets the origin/last-writer instance stamped onto records this
// provider creates (default "local"), so fleet/P2P merge can attribute writes.
func WithInstanceID(id string) Option {
	return func(p *Provider) {
		if id != "" {
			p.instanceID = id
		}
	}
}

// Provider is the SQLite-backed state.Provider.
type Provider struct {
	db         *sql.DB
	hlc        *hlc.Clock
	instanceID string
}

var _ state.Provider = (*Provider)(nil)

// Open opens (creating if needed) a SQLite database at dsn and migrates it to
// the latest schema. dsn is a file path, or ":memory:" for an ephemeral store.
//
// The connection pool is capped at a single connection: SQLite serialises
// writers anyway, and one connection keeps a ":memory:" database alive and makes
// every operation see a consistent view. A future change can widen reads under
// WAL; correctness comes first.
func Open(ctx context.Context, dsn string, opts ...Option) (*Provider, error) {
	// Apply pragmas per-connection via the DSN so they hold for every connection
	// the driver opens: enforce foreign keys and wait rather than fail on a lock.
	conn := dsn + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(1)

	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate.Run(ctx, db, sub); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: migrate: %w", err)
	}

	p := &Provider{db: db, hlc: hlc.NewClock(), instanceID: "local"}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// Name identifies the backend ("sqlite").
func (p *Provider) Name() string { return "sqlite" }

// Sessions returns the durable conversation store.
func (p *Provider) Sessions() state.SessionStore { return &sessions{p} }

// Skills returns the scoped, FTS5-searchable skill store.
func (p *Provider) Skills() state.SkillStore { return &skills{p} }

// Memory returns the durable memory store.
func (p *Provider) Memory() state.MemoryStore { return &memory{p} }

// Close closes the underlying database.
func (p *Provider) Close() error { return p.db.Close() }

// --- shared helpers ---------------------------------------------------------

func newID() string { return ids.New() }

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

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

// --- sessions ---------------------------------------------------------------

type sessions struct{ p *Provider }

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

func (s *sessions) Create(ctx context.Context, ses state.Session) (state.Session, error) {
	if ses.ID == "" {
		ses.ID = newID()
	}
	now := time.Now().UTC()
	if ses.CreatedAt.IsZero() {
		ses.CreatedAt = now
	}
	ses.UpdatedAt = now
	if ses.OriginInstanceID == "" {
		ses.OriginInstanceID = s.p.instanceID
	}
	ses.LastWriterID = s.p.instanceID
	ses.UpdatedHLC = s.p.hlc.Now()
	ses.SyncVersion = 1
	ses.Deleted = false

	_, err := s.p.db.ExecContext(ctx,
		`INSERT INTO sessions (`+sessionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		ses.ID, ses.Title, ses.Model, formatTime(ses.CreatedAt), formatTime(ses.UpdatedAt),
		ses.SyncVersion, ses.OriginInstanceID, ses.UpdatedHLC.Wall, int64(ses.UpdatedHLC.Counter), ses.LastWriterID, 0)
	if err != nil {
		return state.Session{}, fmt.Errorf("sqlite: create session: %w", err)
	}
	return ses, nil
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
	var out state.Turn
	err := s.p.tx(ctx, func(tx *sql.Tx) error {
		var deleted int
		err := tx.QueryRowContext(ctx, `SELECT deleted FROM sessions WHERE id = ?`, t.SessionID).Scan(&deleted)
		if errors.Is(err, sql.ErrNoRows) || deleted != 0 {
			return state.ErrNotFound
		}
		if err != nil {
			return err
		}

		if t.ID == "" {
			t.ID = newID()
		}
		var maxSeq int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM turns WHERE session_id = ?`, t.SessionID).Scan(&maxSeq); err != nil {
			return err
		}
		t.Seq = maxSeq + 1
		if t.CreatedAt.IsZero() {
			t.CreatedAt = time.Now().UTC()
		}
		if t.OriginInstanceID == "" {
			t.OriginInstanceID = s.p.instanceID
		}
		now := s.p.hlc.Now()
		t.LastWriterID = s.p.instanceID
		t.UpdatedHLC = now
		t.SyncVersion = 1
		t.Deleted = false

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO turns (id, session_id, seq, role, content, created_at,
				sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,0)`,
			t.ID, t.SessionID, t.Seq, t.Role, t.Content, formatTime(t.CreatedAt),
			t.SyncVersion, t.OriginInstanceID, now.Wall, int64(now.Counter), t.LastWriterID); err != nil {
			return err
		}
		// Appending a turn mutates the session: bump its envelope.
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET updated_at = ?, sync_version = sync_version + 1,
				updated_hlc_wall = ?, updated_hlc_counter = ?, last_writer_id = ? WHERE id = ?`,
			formatTime(t.CreatedAt), now.Wall, int64(now.Counter), s.p.instanceID, t.SessionID); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
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
	now := s.p.hlc.Now()
	res, err := s.p.db.ExecContext(ctx,
		`UPDATE sessions SET deleted = 1, sync_version = sync_version + 1,
			updated_at = ?, updated_hlc_wall = ?, updated_hlc_counter = ?, last_writer_id = ?
		 WHERE id = ? AND deleted = 0`,
		formatTime(time.Now().UTC()), now.Wall, int64(now.Counter), s.p.instanceID, id)
	if err != nil {
		return err
	}
	return notFoundIfNoRows(res)
}

// --- skills -----------------------------------------------------------------

type skills struct{ p *Provider }

// skillCols matches the skills table column order; queries use `SELECT s.*`.
const skillCols = `id, slug, name, body, tags, scope_instance, scope_project, scope_workspace,
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
	if err := sc.Scan(&s.ID, &s.Slug, &s.Name, &s.Body, &tags,
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

func (s *skills) Upsert(ctx context.Context, sk state.Skill) (state.Skill, error) {
	var out state.Skill
	err := s.p.tx(ctx, func(tx *sql.Tx) error {
		// Look up the existing record by (scope, slug), tombstones included: the
		// row holds the slot, so an upsert over a tombstone resurrects it.
		var (
			id          string
			created     string
			origin      string
			version     int
			syncVersion int64
			found       bool
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, created_at, origin_instance_id, version, sync_version FROM skills
			 WHERE scope_instance = ? AND scope_project = ? AND scope_workspace = ? AND slug = ?`,
			sk.Scope.Instance, sk.Scope.Project, sk.Scope.Workspace, sk.Slug).
			Scan(&id, &created, &origin, &version, &syncVersion)
		switch {
		case err == nil:
			found = true
		case errors.Is(err, sql.ErrNoRows):
			found = false
		default:
			return err
		}

		now := s.p.hlc.Now()
		ts := time.Now().UTC()
		if found {
			// Opt-in optimistic concurrency: a non-zero SyncVersion must match.
			if sk.SyncVersion != 0 && sk.SyncVersion != syncVersion {
				return state.ErrConflict
			}
			sk.ID = id
			sk.CreatedAt = parseTime(created)
			sk.OriginInstanceID = origin // origin is preserved
			sk.Version = version + 1
			sk.SyncVersion = syncVersion + 1
			sk.LastWriterID = s.p.instanceID
			sk.UpdatedHLC = now
			sk.UpdatedAt = ts
			if _, err := tx.ExecContext(ctx,
				`UPDATE skills SET name = ?, body = ?, tags = ?, version = ?, updated_at = ?,
					sync_version = ?, updated_hlc_wall = ?, updated_hlc_counter = ?, last_writer_id = ?, deleted = ?
				 WHERE id = ?`,
				sk.Name, sk.Body, marshalTags(sk.Tags), sk.Version, formatTime(sk.UpdatedAt),
				sk.SyncVersion, now.Wall, int64(now.Counter), sk.LastWriterID, boolToInt(sk.Deleted), sk.ID); err != nil {
				return err
			}
			out = sk
			return reindexSkill(ctx, tx, sk)
		}

		// Creating: a non-zero SyncVersion expected a record that does not exist.
		if sk.SyncVersion != 0 {
			return state.ErrConflict
		}
		if sk.ID == "" {
			sk.ID = newID()
		}
		if sk.Version == 0 {
			sk.Version = 1
		}
		sk.SyncVersion = 1
		if sk.OriginInstanceID == "" {
			sk.OriginInstanceID = s.p.instanceID
		}
		sk.LastWriterID = s.p.instanceID
		sk.UpdatedHLC = now
		sk.CreatedAt, sk.UpdatedAt = ts, ts
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO skills (`+skillCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sk.ID, sk.Slug, sk.Name, sk.Body, marshalTags(sk.Tags),
			sk.Scope.Instance, sk.Scope.Project, sk.Scope.Workspace,
			sk.Version, formatTime(sk.CreatedAt), formatTime(sk.UpdatedAt),
			sk.SyncVersion, sk.OriginInstanceID, now.Wall, int64(now.Counter), sk.LastWriterID, boolToInt(sk.Deleted)); err != nil {
			return err
		}
		out = sk
		return reindexSkill(ctx, tx, sk)
	})
	if err != nil {
		return state.Skill{}, err
	}
	return out, nil
}

func (s *skills) Get(ctx context.Context, idOrSlug string) (state.Skill, error) {
	row := s.p.db.QueryRowContext(ctx, `SELECT * FROM skills WHERE id = ? AND deleted = 0`, idOrSlug)
	sk, err := scanSkill(row)
	if err == nil {
		return sk, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Skill{}, err
	}
	row = s.p.db.QueryRowContext(ctx, `SELECT * FROM skills WHERE slug = ? AND deleted = 0 ORDER BY created_at, id LIMIT 1`, idOrSlug)
	sk, err = scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Skill{}, state.ErrNotFound
	}
	return sk, err
}

func (s *skills) List(ctx context.Context, scope state.Scope) ([]state.Skill, error) {
	rows, err := s.p.db.QueryContext(ctx,
		`SELECT * FROM skills WHERE scope_instance = ? AND scope_project = ? AND scope_workspace = ? AND deleted = 0 ORDER BY slug`,
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
		sqlStr := `SELECT * FROM skills WHERE deleted = 0 ORDER BY slug`
		if limit > 0 {
			sqlStr += ` LIMIT ?`
			rows, err = s.p.db.QueryContext(ctx, sqlStr, limit)
		} else {
			rows, err = s.p.db.QueryContext(ctx, sqlStr)
		}
	} else {
		sqlStr := `SELECT s.* FROM skills s JOIN skills_fts f ON f.skill_id = s.id
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
	return s.p.tx(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM skills WHERE id = ? AND deleted = 0`, idOrSlug).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT id FROM skills WHERE slug = ? AND deleted = 0 ORDER BY created_at, id LIMIT 1`, idOrSlug).Scan(&id)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return state.ErrNotFound
		}
		if err != nil {
			return err
		}
		now := s.p.hlc.Now()
		if _, err := tx.ExecContext(ctx,
			`UPDATE skills SET deleted = 1, version = version + 1, sync_version = sync_version + 1,
				updated_at = ?, updated_hlc_wall = ?, updated_hlc_counter = ?, last_writer_id = ? WHERE id = ?`,
			formatTime(time.Now().UTC()), now.Wall, int64(now.Counter), s.p.instanceID, id); err != nil {
			return err
		}
		// Drop it from the live search index.
		_, err = tx.ExecContext(ctx, `DELETE FROM skills_fts WHERE skill_id = ?`, id)
		return err
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

// --- memory -----------------------------------------------------------------

type memory struct{ p *Provider }

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

func (m *memory) Write(ctx context.Context, it state.MemoryItem) (state.MemoryItem, error) {
	if it.ID == "" {
		it.ID = newID()
	}
	if it.CreatedAt.IsZero() {
		it.CreatedAt = time.Now().UTC()
	}
	if it.OriginInstanceID == "" {
		it.OriginInstanceID = m.p.instanceID
	}
	now := m.p.hlc.Now()
	it.LastWriterID = m.p.instanceID
	it.UpdatedHLC = now
	it.SyncVersion = 1
	it.Deleted = false

	err := m.p.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory_items (`+memoryCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			it.ID, it.Kind, it.Content, it.Scope.Instance, it.Scope.Project, it.Scope.Workspace, it.Source,
			formatTime(it.CreatedAt), it.SyncVersion, it.OriginInstanceID, now.Wall, int64(now.Counter), it.LastWriterID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO memory_fts (item_id, content) VALUES (?, ?)`, it.ID, it.Content)
		return err
	})
	if err != nil {
		return state.MemoryItem{}, err
	}
	return it, nil
}

func (m *memory) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	query := strings.TrimSpace(q.Query)
	scoped := q.Scope != (state.Scope{})

	var sb strings.Builder
	args := make([]any, 0, 5)
	if query == "" {
		sb.WriteString(`SELECT m.* FROM memory_items m WHERE m.deleted = 0`)
	} else {
		sb.WriteString(`SELECT m.* FROM memory_items m JOIN memory_fts f ON f.item_id = m.id
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
	return m.p.tx(ctx, func(tx *sql.Tx) error {
		now := m.p.hlc.Now()
		res, err := tx.ExecContext(ctx,
			`UPDATE memory_items SET deleted = 1, sync_version = sync_version + 1,
				updated_hlc_wall = ?, updated_hlc_counter = ?, last_writer_id = ? WHERE id = ? AND deleted = 0`,
			now.Wall, int64(now.Counter), m.p.instanceID, id)
		if err != nil {
			return err
		}
		if err := notFoundIfNoRows(res); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE item_id = ?`, id)
		return err
	})
}

// --- tx + misc --------------------------------------------------------------

// tx runs fn inside a transaction, committing on success and rolling back on any
// error (so a failed dual write leaves neither the projection nor its index
// changed).
func (p *Provider) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Commit()
}

func notFoundIfNoRows(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return state.ErrNotFound
	}
	return nil
}
