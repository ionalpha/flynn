-- The durable work queue's table. Unlike the state and resource domains, jobs are
-- operational records, not event-sourced truth: a job is claimed, retried, and
-- completed many times, and replaying every lease onto the spine would bloat it
-- with no value. So jobs live in a plain table on the shared database (one engine,
-- one file, one connection), mutated in place under a transaction. The single
-- connection serialises writers, so a claim's SELECT-then-UPDATE leases each ready
-- job to exactly one worker.
--
-- Times are unix nanoseconds (INTEGER) to match the jobs.Job domain type, which
-- carries int64 nanos throughout for arithmetic on schedules and leases.
--
-- Column order matches the jobCols scan order in jobs.go; keep them in lockstep.

CREATE TABLE jobs (
    id                  TEXT    PRIMARY KEY,
    queue               TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    payload             BLOB,
    scope_instance      TEXT    NOT NULL DEFAULT '',
    scope_project       TEXT    NOT NULL DEFAULT '',
    scope_workspace     TEXT    NOT NULL DEFAULT '',
    state               TEXT    NOT NULL,
    attempt             INTEGER NOT NULL DEFAULT 0,
    max_attempts        INTEGER NOT NULL,
    last_error          TEXT    NOT NULL DEFAULT '',
    run_at              INTEGER NOT NULL,
    lease_expires       INTEGER NOT NULL DEFAULT 0,
    origin_instance_id  TEXT    NOT NULL,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);

-- The claim hot path scans one queue for ready work ordered by schedule, so index
-- (queue, state, run_at) to make claiming a bounded index range scan.
CREATE INDEX jobs_claim ON jobs (queue, state, run_at);
