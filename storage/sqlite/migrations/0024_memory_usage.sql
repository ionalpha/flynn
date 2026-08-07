-- Memory gains usage instrumentation: how often an item was pushed at a reader
-- who did not ask for it, how often it was actually used, and when each last
-- happened.
--
-- It is a table beside memory_items rather than columns on it, because a memory
-- item is append-only and its row is the post-image of what a writer asserted.
-- Counting a read is an observation about the item, not a revision of it, and
-- folding one into the item would rewrite the record every time somebody looked
-- at it.
--
-- One row per (memory_id, instance_id), and an instance only ever writes its own.
-- Two instances then cannot conflict on a counter they do not share, so a
-- replicated fleet sums its rows exactly where a single shared counter merged
-- last-writer-wins would drop increments on the floor. It is also the only shape
-- that keeps cross-instance overlap measurable: an aggregate would have thrown
-- away who pushed what.
--
-- There is no foreign key to memory_items. Usage outlives its item's tombstone on
-- purpose, so a curator reviewing what was pushed and never used can still see the
-- record of an item that has since been retired.
--
-- The timestamps are unix nanoseconds with 0 for "never", the same encoding
-- memory_items.expires_at uses, and for the same reason: a fixed-width integer
-- orders correctly under comparison, where the RFC3339Nano text elsewhere in this
-- schema drops trailing zeros and so does not.
CREATE TABLE IF NOT EXISTS memory_usage (
	memory_id           TEXT    NOT NULL,
	instance_id         TEXT    NOT NULL,
	push_count          INTEGER NOT NULL DEFAULT 0,
	last_pushed_at      INTEGER NOT NULL DEFAULT 0,
	organic_uses        INTEGER NOT NULL DEFAULT 0,
	primed_uses         INTEGER NOT NULL DEFAULT 0,
	last_used_at        INTEGER NOT NULL DEFAULT 0,
	sync_version        INTEGER NOT NULL DEFAULT 0,
	origin_instance_id  TEXT    NOT NULL DEFAULT '',
	updated_hlc_wall    INTEGER NOT NULL DEFAULT 0,
	updated_hlc_counter INTEGER NOT NULL DEFAULT 0,
	last_writer_id      TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (memory_id, instance_id)
) WITHOUT ROWID;

-- The fleet-wide read: every instance's row for one item, which is what a total
-- and the overlap metric are computed from.
CREATE INDEX IF NOT EXISTS idx_memory_usage_item ON memory_usage (memory_id);
