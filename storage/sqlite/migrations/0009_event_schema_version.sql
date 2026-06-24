-- Add a schema version to each event payload.
--
-- Payloads are JSON maps, so adding a key is backward compatible, but renaming,
-- removing, or retyping one is not. Recording the version each event was written
-- in lets newer code recognise an older payload and migrate it rather than
-- misread it. Existing rows predate versioning and are the baseline shape, so
-- they default to 1.
--
-- ADD COLUMN appends schema_version as the last column, so `SELECT *` returns it
-- last; the Go scan order in spine.go is updated to match (keep the two in
-- lockstep, as the initial events migration notes).

ALTER TABLE events ADD COLUMN schema_version INTEGER NOT NULL DEFAULT 1;
