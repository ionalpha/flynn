package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/jobs"
)

// Jobs returns the work queue, valid until Close. Jobs are operational records
// mutated in place, outside the event log.
func (s *Store) Jobs() jobs.Queue { return &jobQueue{s} }

// jobQueue delegates lifecycle decisions to package jobs, so it behaves like
// the in-memory queue; only storage and atomic claim are here.
type jobQueue struct{ p *Store }

var (
	_ jobs.Queue    = (*jobQueue)(nil)
	_ jobs.Waker    = (*jobQueue)(nil)
	_ jobs.Releaser = (*jobQueue)(nil)
)

// Ready signals only this process's writes; polling covers other processes.
func (q *jobQueue) Ready() <-chan struct{} { return q.p.jobsReady }

// jobCols must match the table and scanJob order.
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
	jobs.Notify(q.p.jobsReady)
	return j, nil
}

func (q *jobQueue) Claim(ctx context.Context, p jobs.ClaimParams) ([]jobs.Job, error) {
	queue, limit := jobs.ClaimDefaults(p)
	now := q.p.clk.Now().UnixNano()

	var claimed []jobs.Job
	err := q.p.tx(ctx, func(tx *sql.Tx) error {
		// A lease expired on the final attempt makes the job dead.
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = ?, last_error = 'lease expired without completion', lease_expires = 0, updated_at = ?
			 WHERE queue = ? AND state = ? AND lease_expires <= ? AND attempt >= max_attempts`,
			string(jobs.StateDead), now, queue, string(jobs.StateRunning), now); err != nil {
			return err
		}
		// Due pending jobs, or expired leases with attempts left, earliest
		// first. The single connection serialises claims, so one worker holds a
		// job at a time.
		rows, err := tx.QueryContext(ctx,
			`SELECT `+jobCols+` FROM jobs
			 WHERE queue = ? AND (
				(state = ? AND run_at <= ?) OR
				(state = ? AND lease_expires <= ? AND attempt < max_attempts)
			 )
			 ORDER BY run_at, created_at, id`,
			queue, string(jobs.StatePending), now, string(jobs.StateRunning), now)
		if err != nil {
			return err
		}
		ready, err := takeJobs(rows, p, limit)
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
	var pending bool
	err := q.transition(ctx, id, func(j *jobs.Job) {
		jobs.MarkFailed(j, errMsg, retryAt, q.p.clk.Now().UnixNano())
		pending = j.State == jobs.StatePending
	})
	// Wake a worker so a due retry need not wait for the poll.
	if err == nil && pending {
		jobs.Notify(q.p.jobsReady)
	}
	return err
}

// transition refuses a job that is not leased, for Complete and Fail.
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
	row := q.p.reads().QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	return j, err
}

// Recover expires every running lease, so work a crashed process left is
// reclaimed at startup without waiting out the lease.
func (q *jobQueue) Recover(ctx context.Context) (int, error) {
	now := q.p.clk.Now().UnixNano()
	var n int64
	err := q.p.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE jobs SET lease_expires = 0, updated_at = ? WHERE state = ?`,
			now, string(jobs.StateRunning))
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: recover jobs: %w", err)
	}
	if n > 0 {
		jobs.Notify(q.p.jobsReady)
	}
	return int(n), nil
}

// ReleasePayload implements [jobs.Releaser].
func (q *jobQueue) ReleasePayload(ctx context.Context, queue string, payload []byte) (int, error) {
	now := q.p.clk.Now().UnixNano()
	var n int64
	err := q.p.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE jobs SET lease_expires = 0, updated_at = ? WHERE queue = ? AND state = ? AND payload = ?`,
			now, queue, string(jobs.StateRunning), payload)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: release jobs: %w", err)
	}
	if n > 0 {
		jobs.Notify(q.p.jobsReady)
	}
	return int(n), nil
}

// Close is a no-op; the Store owns the database.
func (q *jobQueue) Close() error { return nil }

// takeJobs reads rows until limit of them pass p's filter. The filter cannot be a LIMIT in
// SQL, which would count the rows it rejects.
func takeJobs(rows *sql.Rows, p jobs.ClaimParams, limit int) ([]jobs.Job, error) {
	defer func() { _ = rows.Close() }()
	out := make([]jobs.Job, 0)
	for len(out) < limit && rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		if jobs.Accepted(p, j) {
			out = append(out, j)
		}
	}
	return out, rows.Err()
}
