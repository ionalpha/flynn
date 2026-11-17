package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/usage"
)

// ModelUsage returns the model usage store, valid until Close. It is offered outside
// [state.Provider], which a host may already implement, and found by asserting
// [state.ModelUsageProvider].
func (s *Store) ModelUsage() state.ModelUsageStore { return &modelUsageStore{s} }

// modelUsageStore applies the identity rules of [state.PlanModelUsageAppend] inside the
// transaction that writes, so two processes appending the same key cannot both
// find it absent.
type modelUsageStore struct{ p *Store }

var (
	_ state.ModelUsageProvider = (*Store)(nil)
	_ state.ModelUsageStore    = (*modelUsageStore)(nil)
)

// errNothingNew ends an append whose records were all stored already. It rolls
// back a transaction that has nothing to write, so no event is logged for a
// retry; Append turns it into success.
var errNothingNew = errors.New("sqlite: usage records already stored")

// modelUsageOrder matches [state.SortModelUsageRecords]. Fixed-width UTC at makes string
// order time order.
const modelUsageOrder = ` ORDER BY at, call_id, run_id`

const modelUsageByRunSQL = `SELECT record FROM model_usage WHERE run_id = ?` + modelUsageOrder

// modelUsageTreeSQL uses UNION and not UNION ALL: a run already reached is not walked
// again, which is what ends the walk on a ParentRun cycle.
const modelUsageTreeSQL = `WITH RECURSIVE tree(run_id) AS (
		SELECT ? UNION SELECT r.run_id FROM model_usage r JOIN tree t ON r.parent_run_id = t.run_id
	) SELECT record FROM model_usage WHERE run_id IN (SELECT run_id FROM tree)` + modelUsageOrder

// modelUsageWindowSQL builds the Window query, binding every value.
func modelUsageWindowSQL(from, to time.Time, principal string) (string, []any) {
	q := `SELECT record FROM model_usage WHERE 1 = 1`
	var args []any
	if !from.IsZero() {
		q += ` AND at >= ?`
		args = append(args, formatTime(from))
	}
	if !to.IsZero() {
		q += ` AND at < ?`
		args = append(args, formatTime(to))
	}
	if principal != "" {
		q += ` AND principal = ?`
		args = append(args, principal)
	}
	return q + modelUsageOrder, args
}

func (u *modelUsageStore) Append(ctx context.Context, records ...usage.Record) error {
	if len(records) == 0 {
		return nil
	}
	err := u.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		fresh, err := state.PlanModelUsageAppend(records, func(run, call string) (usage.Record, bool, error) {
			return lookupModelUsageTx(ctx, tx, run, call)
		})
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		if len(fresh) == 0 {
			return spine.AppendInput{}, nil, errNothingNew
		}
		ev, err := u.p.st.RecordModelUsage(fresh)
		return ev, func(tx *sql.Tx) error { return projectModelUsage(ctx, tx, fresh) }, err
	})
	if errors.Is(err, errNothingNew) {
		return nil
	}
	return err
}

func (u *modelUsageStore) ByRun(ctx context.Context, run string) ([]usage.Record, error) {
	return u.query(ctx, modelUsageByRunSQL, run)
}

func (u *modelUsageStore) ByTree(ctx context.Context, root string) ([]usage.Record, error) {
	if root == "" {
		// An empty parent id means "no parent", so walking from it would return
		// every root run in the store.
		return []usage.Record{}, nil
	}
	return u.query(ctx, modelUsageTreeSQL, root)
}

func (u *modelUsageStore) Window(ctx context.Context, from, to time.Time, principal string) ([]usage.Record, error) {
	q, args := modelUsageWindowSQL(from, to, principal)
	return u.query(ctx, q, args...)
}

func (u *modelUsageStore) query(ctx context.Context, q string, args ...any) ([]usage.Record, error) {
	rows, err := u.p.reads().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []usage.Record{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var r usage.Record
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func lookupModelUsageTx(ctx context.Context, tx *sql.Tx, run, call string) (usage.Record, bool, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT record FROM model_usage WHERE run_id = ? AND call_id = ?`, run, call).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return usage.Record{}, false, nil
	}
	if err != nil {
		return usage.Record{}, false, err
	}
	var r usage.Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return usage.Record{}, false, err
	}
	return r, true, nil
}

// projectModelUsage is shared by the live path, Rebuild and snapshot restore. A key
// already present keeps its row: the store never holds two records for one key,
// so a log that somehow names one twice resolves to the first, as a live append
// would.
func projectModelUsage(ctx context.Context, tx *sql.Tx, rs []usage.Record) error {
	for _, r := range rs {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO model_usage (run_id, call_id, parent_run_id, principal, provider, model, at, record)
			 VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(run_id, call_id) DO NOTHING`,
			r.Run, r.Call, r.ParentRun, r.Principal, r.Provider, r.Model, formatTime(r.At), string(raw)); err != nil {
			return err
		}
	}
	return nil
}
