-- Content-addressed payload bodies: a large event payload is stored once in `blobs`,
-- keyed by the SHA-256 of its stored bytes, and the event row keeps only that key in
-- `payload_blob`. Small payloads stay inline in `events.payload` as before; an empty
-- `payload_blob` means "read the inline column". Because a body is addressed by its own
-- hash, two events carrying the same body (a repeated tool output, a re-sent model reply)
-- share one row, so the append-only log's hot bytes grow with distinct bodies, not with
-- their repetition. The tree commits to the full canonical event, so a rehydrated payload
-- reproduces the exact bytes the chain already signed: separating the body out changes
-- where it is stored, never what was recorded.
--
-- This is the split the tiered-storage lifecycle needs: the event rows and their proof
-- material stay small and hot, while the bodies - which dominate the byte count - become
-- an independently movable set that a later warm/cold tier can relocate by content id
-- without touching the log or its checkpoints.

ALTER TABLE events ADD COLUMN payload_blob TEXT NOT NULL DEFAULT '';

-- A rowid table (not WITHOUT ROWID): a body can be many kilobytes, and WITHOUT ROWID
-- stores the whole row in the primary-key B-tree, which SQLite recommends only for small
-- rows. Here the content id is an index into a rowid heap that holds the large bodies.
CREATE TABLE blobs (
  content_id TEXT    NOT NULL PRIMARY KEY, -- lowercase hex SHA-256 of `body`
  body       TEXT    NOT NULL,             -- the payload's stored bytes, verbatim
  size       INTEGER NOT NULL,             -- byte length of `body`, for tier accounting
  refs       INTEGER NOT NULL DEFAULT 0    -- number of events referencing this body
);
