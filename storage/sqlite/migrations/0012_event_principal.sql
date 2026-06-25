-- Principal attribution for events: the identity on whose authority an event was
-- produced (which agent in a fan-out, which human in a multi-user host). It is the
-- audit "who", distinct from the coarse actor kind. Existing rows default to the
-- empty principal, the standalone agent itself. Appended last, so SELECT * returns
-- it after schema_version (keep the scan order in lockstep).
ALTER TABLE events ADD COLUMN principal TEXT NOT NULL DEFAULT '';
