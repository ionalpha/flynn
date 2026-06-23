-- The unified resource substrate's durable projection. Every kind (Skill, Agent,
-- Tool, Policy, Goal, ... and Kind itself) is a row here, written through the same
-- command path as state: a resource event is appended to the shared events table
-- (stream 'resources') and projected into this table in one transaction.
--
-- The full envelope is present from day one: the sync fields (sync_version,
-- origin_instance_id, updated_hlc_*, last_writer_id, deleted), the content hash
-- (Merkle provenance), and reserved bitemporal valid-time (valid_from/valid_to),
-- so none of those ever needs a migration once real data exists.
--
-- Column order matches the resourceCols scan order in resource.go; keep them in
-- lockstep.

CREATE TABLE resources (
    id                  TEXT    PRIMARY KEY,
    api_version         TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    name                TEXT    NOT NULL,
    scope_instance      TEXT    NOT NULL DEFAULT '',
    scope_project       TEXT    NOT NULL DEFAULT '',
    scope_workspace     TEXT    NOT NULL DEFAULT '',
    labels              TEXT    NOT NULL DEFAULT '{}',  -- JSON object
    annotations         TEXT    NOT NULL DEFAULT '{}',  -- JSON object
    spec                TEXT,                            -- raw JSON, NULL when unset
    status              TEXT,                            -- raw JSON, NULL when unset
    sync_version        INTEGER NOT NULL,
    origin_instance_id  TEXT    NOT NULL,
    updated_hlc_wall    INTEGER NOT NULL,
    updated_hlc_counter INTEGER NOT NULL,
    last_writer_id      TEXT    NOT NULL,
    deleted             INTEGER NOT NULL DEFAULT 0,
    version             INTEGER NOT NULL,
    content_hash        TEXT    NOT NULL DEFAULT '',
    valid_from          TEXT,                            -- RFC3339Nano, NULL = since creation
    valid_to            TEXT,                            -- RFC3339Nano, NULL = still valid
    created_at          TEXT    NOT NULL,
    updated_at          TEXT    NOT NULL,
    -- A resource is addressed by (kind, scope, name); the tombstone holds the slot.
    UNIQUE (kind, scope_instance, scope_project, scope_workspace, name)
);
