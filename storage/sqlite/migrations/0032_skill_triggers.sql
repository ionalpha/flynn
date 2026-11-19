-- Triggers a skill declares for the host to load it without the model asking: a JSON
-- array of {kind, match, count, seconds}, empty when it declares none. Existing rows
-- default to none.
ALTER TABLE skills ADD COLUMN triggers TEXT NOT NULL DEFAULT '';
