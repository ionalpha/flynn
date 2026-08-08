-- Skill evidence splits into what was offered and what was read.
--
-- 0007 added uses and wins and called them outcome evidence. What the code
-- incremented was every skill recall put in front of the model, once per run,
-- with no signal that the model read any of them; wins was the same set again on
-- runs that happened to converge. The ratio therefore measured how often a
-- skill's keywords appeared in objectives that went well, and ranking, decay and
-- per-skill grading all read it as though it measured whether the skill helped.
--
-- The uses column is renamed rather than reset, because its contents are correct
-- under the name it should always have had: every increment was an offer. Wins is
-- reset, because offers-on-successful-runs is not a quantity the new definition
-- has a place for and carrying it forward would leave a column whose meaning
-- depends on when the row was written. Reads starts empty everywhere: no
-- historical run recorded whether its skills were read, and there is no
-- reconstruction of that fact that is not a guess.
--
-- Event replay agrees with this without a second rule. The state event's `Uses`
-- key decodes into Offers and its `Wins` key decodes into nothing, so a database
-- rebuilt from the stream lands where this migration leaves it.
ALTER TABLE skills RENAME COLUMN uses TO offers;
ALTER TABLE skills ADD COLUMN reads INTEGER NOT NULL DEFAULT 0;
UPDATE skills SET wins = 0;
