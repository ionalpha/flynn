-- Outcome evidence for skills: how many runs recalled a skill (uses) and how many
-- of those runs then succeeded (wins). These drive ranking and retirement by how
-- well a skill has actually performed, rather than by recency alone. Existing rows
-- default to no evidence.
ALTER TABLE skills ADD COLUMN uses INTEGER NOT NULL DEFAULT 0;
ALTER TABLE skills ADD COLUMN wins INTEGER NOT NULL DEFAULT 0;
