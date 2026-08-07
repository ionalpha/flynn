-- Memory gains push promotions: a trusted reviewer's standing decision about
-- whether an item may be put in front of a reader who did not ask for it.
--
-- It is a table beside memory_items for the same reason usage is. The item row is
-- what its writer asserted and is append-only; a promotion is somebody else's
-- judgment about it, made later and revisable, and folding it into the item would
-- rewrite the fact every time a reviewer changed their mind.
--
-- One row per item, holding the decision in force. The history is not lost by
-- keeping one row: every decision is on the event stream, so the row answers "may
-- this be pushed today" and the stream answers "who decided that, when, and why".
--
-- There is no foreign key to memory_items, matching memory_usage: a decision
-- outlives its item's tombstone so a reviewer can still see what was approved
-- before the item was retired.
--
-- decided_at is unix nanoseconds, the fixed-width encoding the other memory
-- timestamps use, because the RFC3339Nano text elsewhere in this schema drops
-- trailing zeros and so does not order correctly under comparison.
CREATE TABLE IF NOT EXISTS memory_promotions (
	memory_id           TEXT    NOT NULL PRIMARY KEY,
	promoted            INTEGER NOT NULL DEFAULT 0,
	decided_by          TEXT    NOT NULL DEFAULT '',
	reason              TEXT    NOT NULL DEFAULT '',
	decided_at          INTEGER NOT NULL DEFAULT 0,
	sync_version        INTEGER NOT NULL DEFAULT 0,
	origin_instance_id  TEXT    NOT NULL DEFAULT '',
	updated_hlc_wall    INTEGER NOT NULL DEFAULT 0,
	updated_hlc_counter INTEGER NOT NULL DEFAULT 0,
	last_writer_id      TEXT    NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- The digest's read: which items are currently promoted, without scanning the
-- revoked ones it will discard anyway.
CREATE INDEX IF NOT EXISTS idx_memory_promotions_promoted ON memory_promotions (promoted);
