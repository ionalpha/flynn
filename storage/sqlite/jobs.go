package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/jobs"
)

// Jobs returns the durable work queue backed by the same database as the state
// provider and event spine. Jobs are a plain projected table, not event-sourced:
// they are operational records mutated in place, so they do not flow through the
// command path. The returned Queue is valid until the Store is closed.
func (s *Store) Jobs() jobs.Queue { return &jobQueue{s} }

// jobQueue implements jobs.Queue over the shared SQLite database. The lifecycle
// decisions (defaults, claim transition, retry-vs-dead) are delegated to the
// shared functions in package jobs, so this backend and the in-memory reference
// behave identically; only storage and atomic claim selection live here.
type jobQueue struct{ p *Store }

var _ jobs.Queue = (*jobQueue)(nil)

// jobCols matches the jobs table column order and the scanJob order.
const jobCols = `id, queue, kind, payload, scope_instance, scope_project, scope_workspace,
	state, attempt, max_attempts, last_error, run_at, lease_expires, origin_instance_id, created_at, updated_at`

func scanJob(sc interface{ Scan(...any) error }) (jobs.Job, error) {
	var (
		j     jobs.Job
		state string
	)
	if err := sc.Scan(&j.ID, &j.Queue, &j.Kind, &j.Payload,
		&j.Scope.Instance, &j.Scope.Project, &j.Scope.Workspace,
		&state, &j.Attempt, &j.MaxAttempts, &j.LastError, &j.RunAt, &j.LeaseExpires,
		&j.OriginInstanceID, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return jobs.Job{}, err
	}
	j.State = jobs.State(state)
	return j, nil
}

func insertJob(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, j jobs.Job,
) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO jobs (`+jobCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Queue, j.Kind, j.Payload, j.Scope.Instance, j.Scope.Project, j.Scope.Workspace,
		string(j.State), j.Attempt, j.MaxAttempts, j.LastError, j.RunAt, j.LeaseExpires,
		j.OriginInstanceID, j.CreatedAt, j.UpdatedAt)
	return err
}

func updateJob(ctx context.Context, tx *sql.Tx, j jobs.Job) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state=?, attempt=?, last_error=?, run_at=?, lease_expires=?, updated_at=? WHERE id=?`,
		string(j.State), j.Attempt, j.LastError, j.RunAt, j.LeaseExpires, j.UpdatedAt, j.ID)
	return err
}

func (q *jobQueue) Enqueue(ctx context.Context, p jobs.EnqueueParams) (jobs.Job, error) {
	if p.Kind == "" {
		return jobs.Job{}, jobs.ErrInvalidJob
	}
	now := q.p.clk.Now().UnixNano()
	j := jobs.BuildJob(p, now, q.p.gen.New(), q.p.instanceID)
	if err := insertJob(ctx, q.p.db, j); err != nil {
		return jobs.Job{}, fmt.Errorf("sqlite: enqueue job: %w", err)
	}
	return j, nil
}

func (q *jobQueue) Claim(ctx context.Context, p jobs.ClaimParams) ([]jobs.Job, error) {
	queue, limit := jobs.ClaimDefaults(p)
	now := q.p.clk.Now().UnixNano()

	var claimed []jobs.Job
	err := q.p.tx(ctx, func(tx *sql.Tx) error {
		// Reap jobs that timed out on their final attempt: a running job whose lease
		// expired with no attempts left is dead, not retried past MaxAttempts.
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = ?, last_error = 'lease expired without completion', lease_expires = 0, updated_at = ?
			 WHERE queue = ? AND state = ? AND lease_expires <= ? AND attempt >= max_attempts`,
			string(jobs.StateDead), now, queue, string(jobs.StateRunning), now); err != nil {
			return err
		}
		// Select ready jobs in the shared total order (earliest schedule first):
		// pending jobs whose RunAt has arrived, or running jobs whose lease has
		// expired and still have attempts left (crash recovery). The single
		// connection serialises this with any other claim, so a job is leased to one
		// worker at a time.
		rows, err := tx.QueryContext(ctx,
			`SELECT `+jobCols+` FROM jobs
			 WHERE queue = ? AND (
				(state = ? AND run_at <= ?) OR
				(state = ? AND lease_expires <= ? AND attempt < max_attempts)
			 )
			 ORDER BY run_at, created_at, id
			 LIMIT ?`,
			queue, string(jobs.StatePending), now, string(jobs.StateRunning), now, limit)
		if err != nil {
			return err
		}
		ready, err := collectJobs(rows)
		if err != nil {
			return err
		}
		for i := range ready {
			jobs.MarkClaimed(&ready[i], now, p.LeaseFor)
			if err := updateJob(ctx, tx, ready[i]); err != nil {
				return err
			}
		}
		claimed = ready
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: claim jobs: %w", err)
	}
	return claimed, nil
}

func (q *jobQueue) Complete(ctx context.Context, id string) error {
	return q.transition(ctx, id, func(j *jobs.Job) {
		jobs.MarkDone(j, q.p.clk.Now().UnixNano())
	})
}

func (q *jobQueue) Fail(ctx context.Context, id, errMsg string, retryAt int64) error {
	return q.transition(ctx, id, func(j *jobs.Job) {
		jobs.MarkFailed(j, errMsg, retryAt, q.p.clk.Now().UnixNano())
	})
}

// transition loads a running job, applies a state change, and writes it back, all
// in one transaction. It enforces the running-only guard shared by Complete and
// Fail: a job that is not currently leased cannot be completed or failed.
func (q *jobQueue) transition(ctx context.Context, id string, apply func(*jobs.Job)) error {
	return q.p.tx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id)
		j, err := scanJob(row)
		if errors.Is(err, sql.ErrNoRows) {
			return jobs.ErrNotFound
		}
		if err != nil {
			return err
		}
		if j.State != jobs.StateRunning {
			return jobs.ErrNotRunning
		}
		apply(&j)
		return updateJob(ctx, tx, j)
	})
}

func (q *jobQueue) Get(ctx context.Context, id string) (jobs.Job, error) {
	row := q.p.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	return j, err
}

// Close is a no-op: the queue shares the Store's database, which the Store owns
// and closes.
func (q *jobQueue) Close() error { return nil }

func collectJobs(rows *sql.Rows) ([]jobs.Job, error) {
	defer func() { _ = rows.Close() }()
	out := make([]jobs.Job, 0)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
