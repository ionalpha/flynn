package sqlite

// This file gives the state stream the same verified-snapshot lifecycle the resource
// stream has: SnapshotState checkpoints the four state tables onto the log, and Rebuild
// resumes from the latest snapshot instead of folding from seq 0. A snapshot is a derived
// cache over the immutable events, so a missing or rejected one only makes a rebuild
// slower, never wrong.

import (
	"context"
	"database/sql"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// turnCols matches the turns table column order used to scan a full turn row.
const turnCols = `id, session_id, seq, role, content, created_at,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted`

// scanTurn reads one turn row (live or tombstoned), the inverse of insertTurnRow.
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

// SnapshotState checkpoints the current state projection (sessions, turns, skills, and
// memory) onto the event log as a spine.Snapshot on the state stream, anchored at the
// stream's head Seq, so a Rebuild resumes from it. The payload is sealed by the
// configured codec, so a stored state snapshot is verified before it is ever restored.
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

// maybeSnapshotState counts one committed state mutation toward the automatic snapshot
// cadence and checkpoints the state stream when the cadence is reached. It runs after the
// mutation commits and is best effort: a snapshot failure never fails the write.
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

// readStateSnapshot reads the full state projection from the tables (every row, live and
// tombstoned) plus the stream head Seq, the durable analogue of the in-memory core's
// snapshot. The slug index is left nil: the skills table is authoritative for slug
// lookups, so a table-backed restore rebuilds nothing from it.
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

// scanRows runs a read query and calls scan for each row, closing the rows after.
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

// stateSnapshotForRebuild returns the latest usable state snapshot and the Seq to fold
// after, or (zero, 0) to fold the whole stream: no snapshot, one the codec rejects, or
// one that does not decode all fall back the same way. With a codec set, a snapshot that
// fails verification is never restored - the fallback is the fail-closed path.
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

// restoreStateSnapshot projects a snapshot's records back into the tables through the
// same idempotent row writers the live path and applyEvent use, so a rebuild that resumes
// from a snapshot lands identical rows to one that folds from the start.
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
