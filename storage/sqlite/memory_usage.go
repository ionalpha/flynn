package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

const memoryUsageCols = `memory_id, instance_id, push_count, last_pushed_at, organic_uses, primed_uses, last_used_at,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id`

// projectMemoryUsage writes usage post-images. Shared by the live command path and
// applyEvent (Rebuild), so both project identically.
func projectMemoryUsage(ctx context.Context, tx *sql.Tx, rows []state.MemoryUsage) error {
	for _, u := range rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory_usage (`+memoryUsageCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(memory_id, instance_id) DO UPDATE SET
				push_count=excluded.push_count, last_pushed_at=excluded.last_pushed_at,
				organic_uses=excluded.organic_uses, primed_uses=excluded.primed_uses,
				last_used_at=excluded.last_used_at,
				sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
				updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
				last_writer_id=excluded.last_writer_id`,
			u.MemoryID, u.InstanceID, u.PushCount, expiryNanos(u.LastPushedAt),
			u.OrganicUses, u.PrimedUses, expiryNanos(u.LastUsedAt),
			u.SyncVersion, u.OriginInstanceID, u.UpdatedHLC.Wall, int64(u.UpdatedHLC.Counter), u.LastWriterID); err != nil {
			return err
		}
	}
	return nil
}

func scanMemoryUsage(sc interface{ Scan(...any) error }) (state.MemoryUsage, error) {
	var (
		u             state.MemoryUsage
		pushed, used  int64
		wall, counter int64
	)
	if err := sc.Scan(&u.MemoryID, &u.InstanceID, &u.PushCount, &pushed, &u.OrganicUses, &u.PrimedUses, &used,
		&u.SyncVersion, &u.OriginInstanceID, &wall, &counter, &u.LastWriterID); err != nil {
		return state.MemoryUsage{}, err
	}
	u.LastPushedAt = expiryTime(pushed)
	u.LastUsedAt = expiryTime(used)
	u.UpdatedHLC = hlcTime(wall, counter)
	return u, nil
}

// usagePrevTx loads this instance's stored usage rows for the given items within
// tx, rejecting an id with no live item behind it. Both checks run before anything
// is stamped, so a set carrying one bad id records nothing at all.
func (m *memory) usagePrevTx(ctx context.Context, tx *sql.Tx, memoryIDs []string) (map[string]state.MemoryUsage, error) {
	prev := make(map[string]state.MemoryUsage, len(memoryIDs))
	for _, id := range memoryIDs {
		if _, err := getLiveMemoryTx(ctx, tx, id); err != nil {
			return nil, err
		}
		row := tx.QueryRowContext(ctx,
			`SELECT `+memoryUsageCols+` FROM memory_usage WHERE memory_id = ? AND instance_id = ?`,
			id, m.p.st.InstanceID())
		u, err := scanMemoryUsage(row)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		prev[id] = u
	}
	return prev, nil
}

func (m *memory) RecordPush(ctx context.Context, memoryIDs []string) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	return m.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		prev, err := m.usagePrevTx(ctx, tx, memoryIDs)
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		rows, ev, err := m.p.st.RecordMemoryPush(prev, memoryIDs)
		return ev, func(tx *sql.Tx) error { return projectMemoryUsage(ctx, tx, rows) }, err
	})
}

func (m *memory) RecordUse(ctx context.Context, memoryID string, origin state.UsageOrigin) error {
	return m.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		prev, err := m.usagePrevTx(ctx, tx, []string{memoryID})
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		row, ev, err := m.p.st.RecordMemoryUse(prev, memoryID, origin)
		return ev, func(tx *sql.Tx) error { return projectMemoryUsage(ctx, tx, []state.MemoryUsage{row}) }, err
	})
}

func (m *memory) Usage(ctx context.Context, memoryIDs []string) ([]state.MemoryUsage, error) {
	return selectByMemoryID(ctx, m.p.reads(), memoryUsageCols, `memory_usage`, `memory_id, instance_id`, memoryIDs, scanMemoryUsage)
}
