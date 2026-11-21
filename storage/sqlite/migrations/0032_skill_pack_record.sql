-- A bundled skill remembers what the pack last wrote, so an update can tell an
-- untouched skill from one a host edited.
--
-- pack_digest is a fingerprint of the pack-authored fields at the last pack write;
-- diverged is set while a pack update is withheld because the stored copy no longer
-- matches it. Both default to empty, which is also what a skill seeded by an earlier
-- build holds: the next seed treats that as "no record" and stamps one, so no
-- backfill is possible or attempted here. Event replay agrees without a rule of its
-- own, because the state event omits both keys when empty.
ALTER TABLE skills ADD COLUMN pack_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE skills ADD COLUMN diverged INTEGER NOT NULL DEFAULT 0;
