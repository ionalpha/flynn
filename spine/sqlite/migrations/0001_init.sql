-- Initial schema for the SQLite spine.Log: the append-only event store.
--
-- (stream, seq) is the primary key: seq is monotonic within a stream and an
-- event is never updated or deleted, so the key both orders the log and enforces
-- that two events can't claim the same slot. Column order matches the Go scan
-- order in sqlite.go (queries use `SELECT *`), so keep the two in lockstep.

CREATE TABLE events (
    stream             TEXT    NOT NULL,
    seq                INTEGER NOT NULL,
    time               TEXT    NOT NULL,
    type               TEXT    NOT NULL DEFAULT '',
    actor              TEXT    NOT NULL DEFAULT '',
    payload            TEXT    NOT NULL DEFAULT 'null',  -- JSON
    trace_id           TEXT    NOT NULL DEFAULT '',
    span_id            TEXT    NOT NULL DEFAULT '',
    causation_id       TEXT    NOT NULL DEFAULT '',
    origin_instance_id TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (stream, seq)
);
