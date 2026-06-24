-- Provenance of the last write on every resource: 'human', 'agent', or 'system'.
-- Cross-instance merge uses it for precedence, so a person's correction is never
-- silently overwritten by a later automated write. It is envelope metadata, not
-- content, so it is excluded from the content hash. Existing rows default to
-- 'agent' (every write so far was the agent's).
ALTER TABLE resources ADD COLUMN writer_actor TEXT NOT NULL DEFAULT 'agent';
