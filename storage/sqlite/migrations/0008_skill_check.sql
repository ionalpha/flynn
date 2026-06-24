-- The verification command for a skill, kept so the skill can be re-graded later
-- (its check re-run as the environment changes), to re-confirm or retire it. Named
-- check_cmd because "check" is a reserved word. Existing rows default to no check.
ALTER TABLE skills ADD COLUMN check_cmd TEXT NOT NULL DEFAULT '';
