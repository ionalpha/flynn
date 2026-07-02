-- Partial covering index for the key-only resync read (resource.KeyLister):
-- ListKeys selects only the address columns of live rows, so with kind, the
-- scope columns, and name indexed under a deleted = 0 predicate the query is
-- answered entirely from the index, in index order, without touching the table
-- or any tombstone. The UNIQUE(kind, scope..., name) constraint cannot serve
-- this: it includes tombstoned rows, so every visited entry would need a table
-- probe just to check deleted.
CREATE INDEX idx_resources_live_keys
    ON resources (kind, scope_instance, scope_project, scope_workspace, name)
    WHERE deleted = 0;
