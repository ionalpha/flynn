-- Initial schema for the SQLite state.Provider.
--
-- Every record carries the full sync envelope from day one (sync_version,
-- origin_instance_id, updated_hlc_*, last_writer_id, deleted) so optimistic
-- concurrency, hybrid-logical-clock ordering, tombstones, and fleet/P2P merge
-- are reachable without ever migrating the envelope in later. Soft deletes never
-- remove a row: the tombstone keeps the (scope, slug) slot and propagates in sync.
--
-- Column order in each table matches the Go scan order in sqlite.go (queries use
-- `SELECT t.*`), so keep the two in lockstep.

CREATE TABLE sessions (
    id                  TEXT    PRIMARY KEY,
    title               TEXT    NOT NULL DEFAULT '',
    model               TEXT    NOT NULL DEFAULT '',
    created_at          TEXT    NOT NULL,
    updated_at          TEXT    NOT NULL,
    sync_version        INTEGER NOT NULL,
    origin_instance_id  TEXT    NOT NULL,
    updated_hlc_wall    INTEGER NOT NULL,
    updated_hlc_counter INTEGER NOT NULL,
    last_writer_id      TEXT    NOT NULL,
    deleted             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sessions_created_at ON sessions (created_at, id);

CREATE TABLE turns (
    id                  TEXT    PRIMARY KEY,
    session_id          TEXT    NOT NULL REFERENCES sessions (id),
    seq                 INTEGER NOT NULL,
    role                TEXT    NOT NULL DEFAULT '',
    content             TEXT    NOT NULL DEFAULT '',
    created_at          TEXT    NOT NULL,
    sync_version        INTEGER NOT NULL,
    origin_instance_id  TEXT    NOT NULL,
    updated_hlc_wall    INTEGER NOT NULL,
    updated_hlc_counter INTEGER NOT NULL,
    last_writer_id      TEXT    NOT NULL,
    deleted             INTEGER NOT NULL DEFAULT 0,
    UNIQUE (session_id, seq)
);

CREATE TABLE skills (
    id                  TEXT    PRIMARY KEY,
    slug                TEXT    NOT NULL,
    name                TEXT    NOT NULL DEFAULT '',
    body                TEXT    NOT NULL DEFAULT '',
    tags                TEXT    NOT NULL DEFAULT '[]',  -- JSON array of strings
    scope_instance      TEXT    NOT NULL DEFAULT '',
    scope_project       TEXT    NOT NULL DEFAULT '',
    scope_workspace     TEXT    NOT NULL DEFAULT '',
    version             INTEGER NOT NULL,
    created_at          TEXT    NOT NULL,
    updated_at          TEXT    NOT NULL,
    sync_version        INTEGER NOT NULL,
    origin_instance_id  TEXT    NOT NULL,
    updated_hlc_wall    INTEGER NOT NULL,
    updated_hlc_counter INTEGER NOT NULL,
    last_writer_id      TEXT    NOT NULL,
    deleted             INTEGER NOT NULL DEFAULT 0,
    UNIQUE (scope_instance, scope_project, scope_workspace, slug)
);

-- Full-text index over live skills. Maintained in the same transaction as the
-- skills projection (the dual write); holds a row only while the skill is live,
-- so a tombstone drops out of search.
CREATE VIRTUAL TABLE skills_fts USING fts5 (
    skill_id UNINDEXED,
    name,
    body,
    tags
);

CREATE TABLE memory_items (
    id                  TEXT    PRIMARY KEY,
    kind                TEXT    NOT NULL DEFAULT '',
    content             TEXT    NOT NULL DEFAULT '',
    scope_instance      TEXT    NOT NULL DEFAULT '',
    scope_project       TEXT    NOT NULL DEFAULT '',
    scope_workspace     TEXT    NOT NULL DEFAULT '',
    source              TEXT    NOT NULL DEFAULT '',
    created_at          TEXT    NOT NULL,
    sync_version        INTEGER NOT NULL,
    origin_instance_id  TEXT    NOT NULL,
    updated_hlc_wall    INTEGER NOT NULL,
    updated_hlc_counter INTEGER NOT NULL,
    last_writer_id      TEXT    NOT NULL,
    deleted             INTEGER NOT NULL DEFAULT 0
);

CREATE VIRTUAL TABLE memory_fts USING fts5 (
    item_id UNINDEXED,
    content
);
