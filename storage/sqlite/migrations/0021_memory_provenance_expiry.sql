-- Memory provenance becomes a list, and memory gains an expiry.
--
-- sources holds a JSON array of the inputs a fact was distilled from. The old
-- single-valued source column could only record one of them, so an item assembled
-- from several origins under-reported its provenance, and a purge of everything a
-- given source contributed to missed exactly the items where it was one input
-- among many. Existing rows carry their one source across as a one-element array,
-- so nothing loses provenance; rows that never had one stay empty.
--
-- expires_at is unix nanoseconds, 0 meaning never, deliberately not the RFC3339
-- text that created_at uses. RFC3339Nano drops trailing zeros from the fractional
-- second, so those values are not fixed-width and do not compare correctly as
-- text; expiry is evaluated on the read path of every recall, including the
-- prepared single-scope statement, so it has to be a predicate SQLite can answer
-- exactly rather than one the Go side re-checks after the fact.
ALTER TABLE memory_items ADD COLUMN sources TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_items ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0;

UPDATE memory_items SET sources = json_array(source) WHERE source <> '';

ALTER TABLE memory_items DROP COLUMN source;
