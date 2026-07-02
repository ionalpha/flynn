-- Read-path indexes for the two point lookups that previously scanned:
--
-- memory_items has only its id primary key, so scoped recall (the agent-startup
-- read) was a full table scan plus a sort pass. The partial index puts live rows
-- in (scope, created_at DESC, id DESC) order, which is exactly the recall
-- predicate and ORDER BY, so a scoped recall is an index range scan with no
-- temp b-tree.
CREATE INDEX idx_memory_items_live_scope
    ON memory_items (scope_instance, scope_project, scope_workspace, created_at DESC, id DESC)
    WHERE deleted = 0;

-- Bare-slug skill lookup (Get falls back to slug when the id probe misses) took
-- the table-scan branch: slug is not the primary key and had no index. Live
-- rows only; tombstones are never returned by the lookup.
CREATE INDEX idx_skills_live_slug
    ON skills (slug, created_at, id)
    WHERE deleted = 0;
