-- Memory gains anchors: opaque refs to things outside this store that a fact is
-- about, so a reader holding a ref can recall what is known about it without
-- composing a lexical query.
--
-- The refs are stored twice, on purpose, in the same shape the content index
-- already uses. memory_items.anchors is the canonical JSON the item is read back
-- from, so hydrating a recall stays one row per item with no join. memory_anchors
-- is a projection maintained beside it purely so the anchor predicate is an
-- indexed lookup instead of a scan over decoded JSON; like memory_fts it holds a
-- row only while the item is live, so a tombstone drops out of anchored recall
-- without the read having to re-check.
--
-- The columns are deliberately untyped-by-domain: kind and ref_id are whatever the
-- referring system calls its records. Nothing here resolves them, there is no
-- foreign key, and a ref whose referent is gone is a normal row rather than an
-- integrity failure.
ALTER TABLE memory_items ADD COLUMN anchors TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS memory_anchors (
	item_id TEXT NOT NULL,
	kind    TEXT NOT NULL,
	ref_id  TEXT NOT NULL,
	PRIMARY KEY (item_id, kind, ref_id)
) WITHOUT ROWID;

-- The lookup direction: given a ref, which items are anchored to it.
CREATE INDEX IF NOT EXISTS idx_memory_anchors_ref ON memory_anchors (kind, ref_id);
