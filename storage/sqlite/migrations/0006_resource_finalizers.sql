-- Deletion lifecycle for resources: finalizers + a deletion timestamp. When a
-- resource has finalizers, a delete does not remove it; it sets deletion_timestamp
-- and the resource stays live (deleted = 0) until every finalizer is cleared, then
-- the record tombstones. This is how external state (a worktree, a child run) is
-- cleaned up reliably across crashes. Both columns are envelope metadata, excluded
-- from the content hash. Existing rows default to no finalizers and not deleting.
ALTER TABLE resources ADD COLUMN finalizers TEXT NOT NULL DEFAULT '[]'; -- JSON array
ALTER TABLE resources ADD COLUMN deletion_timestamp TEXT;               -- RFC3339Nano, NULL = not deleting
