package sqlite

import (
	"context"
	"database/sql"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

const turnCols = `id, session_id, seq, role, content, created_at,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted`

func scanTurn(sc interface{ Scan(...any) error }) (state.Turn, error) {
	var (
		t             state.Turn
		created       string
		wall, counter int64
		deleted       int
	)
	if err := sc.Scan(&t.ID, &t.SessionID, &t.Seq, &t.Role, &t.Content, &created,
		&t.SyncVersion, &t.OriginInstanceID, &wall, &counter, &t.LastWriterID, &deleted); err != nil {
		return state.Turn{}, err
	}
	t.CreatedAt = parseTime(created)
	t.UpdatedHLC = hlcTime(wall, counter)
	t.Deleted = deleted != 0
	return t, nil
}

// SnapshotState checkpoints sessions, turns, skills and memory onto the state
// stream as a [spine.Snapshot] at the head Seq, so Rebuild resumes from it. The
// codec seals the payload, so a snapshot is verified before it is restored. A
// missing or rejected snapshot only slows a rebuild.
func (s *Store) SnapshotState(ctx context.Context) error {
	snap, err := s.readStateSnapshot(ctx)
	if err != nil {
		return err
	}
	payload, err := state.MarshalSnapshot(snap)
	if err != nil {
		return err
	}
	sp := spine.Snapshot{Stream: state.StateStream, Seq: snap.LastSeq, Payload: payload}
	if s.snapCodec != nil {
		if sp, err = s.snapCodec.Seal(ctx, s.Log(), sp); err != nil {
			return err
		}
	}
	return s.Log().SaveSnapshot(ctx, sp)
}

// maybeSnapshotState runs after a mutation commits and is best effort: a
// snapshot failure never fails the write.
func (s *Store) maybeSnapshotState(ctx context.Context) {
	if s.snapEvery <= 0 {
		return
	}
	if s.snapPendingState.Add(1) < int64(s.snapEvery) {
		return
	}
	s.snapPendingState.Store(0)
	_ = s.SnapshotState(ctx)
}

// readStateSnapshot includes tombstoned rows. The slug index is nil because
// the skills table is authoritative for slugs.
func (s *Store) readStateSnapshot(ctx context.Context) (state.Snapshot, error) {
	var snap state.Snapshot

	if err := s.scanRows(ctx, `SELECT `+sessionCols+` FROM sessions ORDER BY id`, func(rows *sql.Rows) error {
		ses, err := scanSession(rows)
		if err != nil {
			return err
		}
		snap.Sessions = append(snap.Sessions, ses)
		return nil
	}); err != nil {
		return snap, err
	}
	if err := s.scanRows(ctx, `SELECT `+turnCols+` FROM turns ORDER BY session_id, seq`, func(rows *sql.Rows) error {
		t, err := scanTurn(rows)
		if err != nil {
			return err
		}
		snap.Turns = append(snap.Turns, t)
		return nil
	}); err != nil {
		return snap, err
	}
	if err := s.scanRows(ctx, `SELECT `+skillCols+` FROM skills ORDER BY id`, func(rows *sql.Rows) error {
		sk, err := scanSkill(rows)
		if err != nil {
			return err
		}
		snap.Skills = append(snap.Skills, sk)
		return nil
	}); err != nil {
		return snap, err
	}
	if err := s.scanRows(ctx, `SELECT `+memoryCols+` FROM memory_items ORDER BY id`, func(rows *sql.Rows) error {
		it, err := scanMemory(rows)
		if err != nil {
			return err
		}
		snap.Items = append(snap.Items, it)
		return nil
	}); err != nil {
		return snap, err
	}

	if err := s.reads().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE stream = ?`, state.StateStream).Scan(&snap.LastSeq); err != nil {
		return snap, err
	}
	return snap, nil
}

func (s *Store) scanRows(ctx context.Context, query string, scan func(*sql.Rows) error) error {
	rows, err := s.reads().QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// stateSnapshotForRebuild returns (zero, 0) to fold the whole stream when there
// is no snapshot or the codec rejects it or it fails to decode. A snapshot that
// fails verification is never restored.
func (s *Store) stateSnapshotForRebuild(ctx context.Context) (state.Snapshot, int64) {
	snap, found, err := s.Log().LatestSnapshot(ctx, state.StateStream, 0)
	if err != nil || !found {
		return state.Snapshot{}, 0
	}
	if s.snapCodec != nil {
		opened, oerr := s.snapCodec.Open(ctx, snap)
		if oerr != nil {
			return state.Snapshot{}, 0
		}
		snap = opened
	}
	decoded, derr := state.UnmarshalSnapshot(snap.Payload)
	if derr != nil {
		return state.Snapshot{}, 0
	}
	return decoded, decoded.LastSeq
}

// restoreStateSnapshot uses the row writers of the live path and applyEvent,
// so resuming from a snapshot yields the rows a full fold would.
func (s *Store) restoreStateSnapshot(ctx context.Context, tx *sql.Tx, snap state.Snapshot) error {
	for _, ses := range snap.Sessions {
		if err := upsertSessionRow(ctx, tx, s, ses); err != nil {
			return err
		}
	}
	for _, t := range snap.Turns {
		if err := insertTurnRow(ctx, tx, s, t); err != nil {
			return err
		}
	}
	for _, sk := range snap.Skills {
		if err := projectSkill(ctx, tx, sk); err != nil {
			return err
		}
	}
	for _, it := range snap.Items {
		if err := projectMemory(ctx, tx, it); err != nil {
			return err
		}
	}
	return nil
}
