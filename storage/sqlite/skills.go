package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

type skills struct{ p *Store }

// skillCols matches the skills table column order.
const skillCols = `id, slug, name, description, body, tags, offers, reads, wins, check_cmd, scope_instance, scope_project, scope_workspace,
	version, created_at, updated_at,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted`

// skillColsQualified is skillCols against the `s` alias, for the search query, which
// joins the FTS table and so cannot use bare column names. It is a constant beside
// skillCols rather than a list written out at the call site: the two were duplicated
// by hand, and the copy silently fell one column behind, which scanSkill can only
// report as an argument-count mismatch at run time.
const skillColsQualified = `s.id, s.slug, s.name, s.description, s.body, s.tags, s.offers, s.reads, s.wins, s.check_cmd,
	s.scope_instance, s.scope_project, s.scope_workspace,
	s.version, s.created_at, s.updated_at,
	s.sync_version, s.origin_instance_id, s.updated_hlc_wall, s.updated_hlc_counter, s.last_writer_id, s.deleted`

func scanSkill(sc interface{ Scan(...any) error }) (state.Skill, error) {
	var (
		s                state.Skill
		tags             string
		created, updated string
		wall, counter    int64
		deleted          int
	)
	if err := sc.Scan(&s.ID, &s.Slug, &s.Name, &s.Description, &s.Body, &tags, &s.Offers, &s.Reads, &s.Wins, &s.Check,
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
		`INSERT INTO skills (`+skillCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
			slug=excluded.slug, name=excluded.name, description=excluded.description,
			body=excluded.body, tags=excluded.tags,
			offers=excluded.offers, reads=excluded.reads, wins=excluded.wins, check_cmd=excluded.check_cmd,
			scope_instance=excluded.scope_instance, scope_project=excluded.scope_project, scope_workspace=excluded.scope_workspace,
			version=excluded.version, created_at=excluded.created_at, updated_at=excluded.updated_at,
			sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`,
		sk.ID, sk.Slug, sk.Name, sk.Description, sk.Body, marshalTags(sk.Tags), sk.Offers, sk.Reads, sk.Wins, sk.Check,
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
		// Ordered by how well the row matches, then by slug for a stable answer.
		// A limit cuts this list, so ordering it by slug would make the cap select
		// alphabetically: for a term many skills share, everything sorted after the
		// first few is unreachable however well it matches. The bm25 weights follow
		// the columns of skills_fts and say where a hit counts most: the description
		// is what a skill publishes about when to reach for it, the name is the
		// handle, and the body is the long text a stray word lands in.
		sqlStr := `SELECT ` + skillColsQualified + `
			FROM skills s JOIN skills_fts f ON f.skill_id = s.id
			WHERE f.skills_fts MATCH ? AND s.deleted = 0
			ORDER BY bm25(skills_fts, 0.0, 5.0, 10.0, 1.0, 3.0), s.slug`
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
		`INSERT INTO skills_fts (skill_id, name, description, body, tags) VALUES (?,?,?,?,?)`,
		sk.ID, sk.Name, sk.Description, sk.Body, strings.Join(sk.Tags, " "))
	return err
}
