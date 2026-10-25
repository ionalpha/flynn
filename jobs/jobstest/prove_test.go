package jobstest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/conformtest"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/jobs/jobstest"
)

// faulty wraps a correct queue and lets a case replace exactly one behaviour.
// The hooks receive the inner queue and the clock driving it.
type faulty struct {
	jobs.Queue
	clk     *clock.Manual
	enqueue func(f *faulty, p jobs.EnqueueParams) (jobs.Job, error)
	claim   func(f *faulty, p jobs.ClaimParams) ([]jobs.Job, error)
	finish  func(f *faulty, id string) error
	fail    func(f *faulty, id, msg string, retryAt int64) error
	get     func(f *faulty, id string) (jobs.Job, error)
	recover func(f *faulty) (int, error)
}

func (f *faulty) Enqueue(ctx context.Context, p jobs.EnqueueParams) (jobs.Job, error) {
	if f.enqueue != nil {
		return f.enqueue(f, p)
	}
	return f.Queue.Enqueue(ctx, p)
}

func (f *faulty) Claim(ctx context.Context, p jobs.ClaimParams) ([]jobs.Job, error) {
	if f.claim != nil {
		return f.claim(f, p)
	}
	return f.Queue.Claim(ctx, p)
}

func (f *faulty) Complete(ctx context.Context, id string) error {
	if f.finish != nil {
		return f.finish(f, id)
	}
	return f.Queue.Complete(ctx, id)
}

func (f *faulty) Fail(ctx context.Context, id, msg string, retryAt int64) error {
	if f.fail != nil {
		return f.fail(f, id, msg, retryAt)
	}
	return f.Queue.Fail(ctx, id, msg, retryAt)
}

func (f *faulty) Get(ctx context.Context, id string) (jobs.Job, error) {
	if f.get != nil {
		return f.get(f, id)
	}
	return f.Queue.Get(ctx, id)
}

func (f *faulty) Recover(ctx context.Context) (int, error) {
	if f.recover != nil {
		return f.recover(f)
	}
	return f.Queue.Recover(ctx)
}

// harness builds a correct in-memory queue and applies break to a wrapper of it.
func harness(brk func(f *faulty)) func() jobstest.Harness {
	return func() jobstest.Harness {
		clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
		f := &faulty{Queue: jobs.NewMemory(jobs.WithClock(clk)), clk: clk}
		brk(f)
		return jobstest.Harness{Queue: f, Clock: clk}
	}
}

func TestSuiteFailsOnBrokenQueue(t *testing.T) {
	c := func(name, want string, brk func(f *faulty)) conformtest.Case {
		mk := harness(brk)
		return conformtest.Case{Name: name, Want: want, Suite: func(t *testing.T) { jobstest.RunSuite(t, mk) }}
	}
	// withParams rewrites the enqueue params before the inner queue sees them.
	withParams := func(edit func(f *faulty, p *jobs.EnqueueParams)) func(f *faulty) {
		return func(f *faulty) {
			f.enqueue = func(f *faulty, p jobs.EnqueueParams) (jobs.Job, error) {
				edit(f, &p)
				return f.Queue.Enqueue(context.Background(), p)
			}
		}
	}
	// withClaim rewrites the claim params before the inner queue sees them.
	withClaim := func(edit func(p *jobs.ClaimParams)) func(f *faulty) {
		return func(f *faulty) {
			f.claim = func(f *faulty, p jobs.ClaimParams) ([]jobs.Job, error) {
				edit(&p)
				return f.Queue.Claim(context.Background(), p)
			}
		}
	}
	inflateAttempts := withParams(func(_ *faulty, p *jobs.EnqueueParams) {
		if p.MaxAttempts > 0 {
			p.MaxAttempts += 10
		}
	})

	conformtest.Prove(t, []conformtest.Case{
		c("enqueue leaves the queue name empty", "EnqueueAppliesDefaults", func(f *faulty) {
			f.enqueue = func(f *faulty, p jobs.EnqueueParams) (jobs.Job, error) {
				j, err := f.Queue.Enqueue(context.Background(), p)
				j.Queue = ""
				return j, err
			}
		}),
		c("enqueue accepts an empty kind", "EnqueueRequiresKind", withParams(func(_ *faulty, p *jobs.EnqueueParams) {
			if p.Kind == "" {
				p.Kind = "anonymous"
			}
		})),
		c("claim returns a job without leasing it", "ClaimLeasesReadyJob", func(f *faulty) {
			f.claim = func(f *faulty, p jobs.ClaimParams) ([]jobs.Job, error) {
				got, err := f.Queue.Claim(context.Background(), p)
				for i := range got {
					got[i].State = jobs.StatePending
				}
				return got, err
			}
		}),
		c("claim hands a leased job to a second worker", "ClaimIsExclusive", func(f *faulty) {
			var last []jobs.Job
			f.claim = func(f *faulty, p jobs.ClaimParams) ([]jobs.Job, error) {
				got, err := f.Queue.Claim(context.Background(), p)
				if len(got) == 0 {
					return last, err
				}
				last = got
				return got, err
			}
		}),
		c("claim ignores RunAt", "ClaimRespectsRunAt", withParams(func(_ *faulty, p *jobs.EnqueueParams) { p.RunAt = 0 })),
		c("claim exceeds the limit", "ClaimRespectsLimit", withClaim(func(p *jobs.ClaimParams) { p.Limit *= 2 })),
		c("lease never expires", "ClaimReclaimsExpiredLease", withClaim(func(p *jobs.ClaimParams) { p.LeaseFor = int64(100 * 365 * 24 * time.Hour) })),
		c("expired lease re-leases past MaxAttempts", "ExhaustedLeaseTimesOutToDead", inflateAttempts),
		c("claim crosses queues", "ClaimIsolatesQueues", func(f *faulty) {
			f.enqueue = func(f *faulty, p jobs.EnqueueParams) (jobs.Job, error) {
				p.Queue = "shared"
				return f.Queue.Enqueue(context.Background(), p)
			}
			f.claim = func(f *faulty, p jobs.ClaimParams) ([]jobs.Job, error) {
				p.Queue = "shared"
				return f.Queue.Claim(context.Background(), p)
			}
		}),
		c("claim ignores RunAt order", "ClaimOrdersByRunAt", withParams(func(f *faulty, p *jobs.EnqueueParams) {
			// Mirror near-term schedules around now, so the later job becomes due first.
			now := f.clk.Now().UnixNano()
			if d := p.RunAt - now; d >= 0 && d <= int64(10*time.Second) {
				p.RunAt = now - d - 1
			}
		})),
		c("complete does nothing", "CompleteMarksDone", func(f *faulty) {
			f.finish = func(*faulty, string) error { return nil }
		}),
		c("fail always kills the job", "FailRetriesThenDies", func(f *faulty) {
			f.fail = func(f *faulty, id, msg string, _ int64) error {
				return f.Queue.Fail(context.Background(), id, msg, -1)
			}
		}),
		c("fail ignores retryAt", "FailSchedulesRetryAt", func(f *faulty) {
			f.fail = func(f *faulty, id, msg string, retryAt int64) error {
				if retryAt > 0 {
					retryAt = f.clk.Now().UnixNano()
				}
				return f.Queue.Fail(context.Background(), id, msg, retryAt)
			}
		}),
		c("negative retryAt still retries", "FailPermanentlyDeadsAtOnce", func(f *faulty) {
			f.fail = func(f *faulty, id, msg string, retryAt int64) error {
				if retryAt < 0 {
					retryAt = f.clk.Now().UnixNano()
				}
				return f.Queue.Fail(context.Background(), id, msg, retryAt)
			}
		}),
		c("complete accepts a job that is not running", "CompleteAndFailGuardState", func(f *faulty) {
			f.finish = func(f *faulty, id string) error {
				err := f.Queue.Complete(context.Background(), id)
				if errors.Is(err, jobs.ErrNotRunning) {
					return nil
				}
				return err
			}
		}),
		c("get invents unknown jobs", "GetUnknownIsNotFound", func(f *faulty) {
			f.get = func(f *faulty, id string) (jobs.Job, error) {
				j, err := f.Queue.Get(context.Background(), id)
				if errors.Is(err, jobs.ErrNotFound) {
					return jobs.Job{ID: id}, nil
				}
				return j, err
			}
		}),
		c("recover does nothing", "RecoverReclaimsRunningAtOnce", func(f *faulty) {
			f.recover = func(*faulty) (int, error) { return 0, nil }
		}),
		c("recover revives exhausted jobs", "RecoverReapsExhausted", inflateAttempts),
		c("recover counts pending jobs", "RecoverIgnoresPending", func(f *faulty) {
			f.recover = func(f *faulty) (int, error) {
				n, err := f.Queue.Recover(context.Background())
				return n + 1, err
			}
		}),
	})
}
