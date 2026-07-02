-- SpecHash joins the envelope: a stable hash of the desired state alone,
-- stamped at write time so controllers compare a stored field instead of
-- re-canonicalizing the spec every reconcile tick. Empty on rows written
-- before the column existed; readers fall back to computing it.
ALTER TABLE resources ADD COLUMN spec_hash TEXT NOT NULL DEFAULT '';
