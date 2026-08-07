-- Memory gains a taint flag: untrusted input was in the context that produced the
-- fact, whatever the write's own provenance claims.
--
-- It is a stored column rather than something derived from sources at read time
-- because the laundering path is exactly the one where the sources look clean: an
-- agent reads a poisoned tool output, concludes something from it, and writes the
-- conclusion crediting itself. Only the writing context knew, and only at the write.
--
-- The flag gates the wake digest, not recall. Existing rows default to 0, which is
-- the honest answer for memory written before anything was tracking taint: nothing
-- observed it, so nothing can assert it. Whether those rows reach a digest is then
-- decided by their provenance and, for the agent's own notes, by promotion.
ALTER TABLE memory_items ADD COLUMN tainted INTEGER NOT NULL DEFAULT 0;

-- The digest reads pushable items, so it filters on this column over the whole
-- store rather than over one scope's rows; without the index that filter is the
-- scan the digest runs on every wake.
CREATE INDEX IF NOT EXISTS idx_memory_items_tainted ON memory_items (tainted);
