-- Memory gains the two fields a write policy keys on: the subject a fact is about,
-- and the ids it replaces.
--
-- Subject is a stored, normalized column rather than something matched out of the
-- content because it is a key, not a phrase: a policy deciding what a new fact does
-- to the ones already on its topic has to find every item on that topic exactly,
-- including the ones whose text never spells the subject out. Existing rows default
-- to the empty string, which reads as "about no particular subject" and is the
-- honest answer for memory written before anything was grouping by one.
ALTER TABLE memory_items ADD COLUMN subject TEXT NOT NULL DEFAULT '';

-- Supersedes is a JSON array of item ids, stored the way sources and anchors are.
-- It is a link and not an index: nothing reads memory by "what superseded this", so
-- there is no lookup table here of the kind memory_anchors is. The empty string is
-- no supersession, matching how the other list columns encode empty.
ALTER TABLE memory_items ADD COLUMN supersedes TEXT NOT NULL DEFAULT '';

-- A subject-filtered recall is the read the write path takes before every write it
-- has to decide the semantics of, so it runs at least as often as writes do. It is
-- an exact match on one column, which is the shape an index serves best; without it
-- the filter is a scan of the whole store on a hot path.
CREATE INDEX IF NOT EXISTS idx_memory_items_subject ON memory_items (subject);
