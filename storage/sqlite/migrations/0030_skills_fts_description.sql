-- The description joins the full-text index, because it is the retrieval key.
--
-- 0023 added the column and left it out of skills_fts, on the grounds that an FTS5
-- table's columns cannot be added by ALTER and the schema change should not carry a
-- projection rebuild. The consequence was quiet and total: search matched a skill's
-- name, body and tags, so a skill whose subject was stated where the specification
-- says to state it was not a candidate for its own objectives. Ranking scores over
-- the description, but nothing that is never gathered can be ranked.
--
-- The virtual table is dropped and rebuilt because that is the only way to add the
-- column, and repopulated from the skills projection rather than by replaying the
-- log: the projection is the post-image of every skill event already folded, so the
-- index it produces is the same one and it costs one scan of live rows.
DROP TABLE skills_fts;

CREATE VIRTUAL TABLE skills_fts USING fts5 (
	skill_id UNINDEXED,
	name,
	description,
	body,
	tags
);

INSERT INTO skills_fts (skill_id, name, description, body, tags)
SELECT id, name, description, body, tags FROM skills WHERE deleted = 0;
