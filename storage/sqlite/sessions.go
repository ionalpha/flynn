package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

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
