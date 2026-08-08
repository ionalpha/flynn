package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

const memoryPromotionCols = `memory_id, promoted, decided_by, reason, decided_at,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id`

// projectMemoryPromotion writes a promotion post-image. Shared by the live command
// path and applyEvent (Rebuild), so both project identically.
func projectMemoryPromotion(ctx context.Context, tx *sql.Tx, p state.MemoryPromotion) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memory_promotions (`+memoryPromotionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(memory_id) DO UPDATE SET
			promoted=excluded.promoted, decided_by=excluded.decided_by, reason=excluded.reason,
			decided_at=excluded.decided_at,
			sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id`,
		p.MemoryID, boolToInt(p.Promoted), p.By, p.Reason, expiryNanos(p.DecidedAt),
		p.SyncVersion, p.OriginInstanceID, p.UpdatedHLC.Wall, int64(p.UpdatedHLC.Counter), p.LastWriterID)
	return err
}

func scanMemoryPromotion(sc interface{ Scan(...any) error }) (state.MemoryPromotion, error) {
	var (
		p             state.MemoryPromotion
		promoted      int
		decided       int64
		wall, counter int64
	)
	if err := sc.Scan(&p.MemoryID, &promoted, &p.By, &p.Reason, &decided,
		&p.SyncVersion, &p.OriginInstanceID, &wall, &counter, &p.LastWriterID); err != nil {
		return state.MemoryPromotion{}, err
	}
	p.Promoted = promoted != 0
	p.DecidedAt = expiryTime(decided)
	p.UpdatedHLC = hlcTime(wall, counter)
	return p, nil
}

func (m *memory) Promote(ctx context.Context, d state.PromotionDecision) (state.MemoryPromotion, error) {
	var out state.MemoryPromotion
	err := m.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		if !d.Valid() {
			return spine.AppendInput{}, nil, state.ErrInvalid
		}
		// The item is checked inside the transaction that writes the decision, so a
		// promotion cannot land against an item a concurrent delete just tombstoned.
		if _, err := getLiveMemoryTx(ctx, tx, d.MemoryID); err != nil {
			return spine.AppendInput{}, nil, err
		}
		prev, err := m.promotionPrevTx(ctx, tx, d.MemoryID)
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		row, ev, err := m.p.st.RecordMemoryPromotion(prev, d)
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		out = row
		return ev, func(tx *sql.Tx) error { return projectMemoryPromotion(ctx, tx, row) }, nil
	})
	if err != nil {
		return state.MemoryPromotion{}, err
	}
	return out, nil
}

// promotionPrevTx loads the stored decision for an item within tx, or nil when
// nobody has decided yet.
func (m *memory) promotionPrevTx(ctx context.Context, tx *sql.Tx, memoryID string) (*state.MemoryPromotion, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+memoryPromotionCols+` FROM memory_promotions WHERE memory_id = ?`, memoryID)
	p, err := scanMemoryPromotion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (m *memory) Promotions(ctx context.Context, memoryIDs []string) ([]state.MemoryPromotion, error) {
	return selectByMemoryID(ctx, m.p.reads(), memoryPromotionCols, `memory_promotions`, `memory_id`, memoryIDs, scanMemoryPromotion)
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
