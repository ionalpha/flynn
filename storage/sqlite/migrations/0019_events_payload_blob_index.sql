-- Index the events -> blob reference so the archival sweep stops scanning the log.
--
-- The tiered-storage sweep asks which hot bodies are fully sealed: for each blob it
-- runs NOT EXISTS (SELECT 1 FROM events WHERE payload_blob = b.content_id AND seq >
-- sealed_size). events had only its (stream, seq) primary key, so that probe was a
-- full table scan per distinct hot blob, i.e. O(distinct_blobs x total_events) that
-- grows with the log. This partial index turns each probe into a seek.
--
-- Partial on payload_blob != '': only events that reference a blob (a non-empty
-- content id) can ever satisfy the join, and inline-payload rows are the majority, so
-- excluding them keeps the index to the referencing set and off the write path for
-- inline appends.
CREATE INDEX idx_events_payload_blob
    ON events (payload_blob)
    WHERE payload_blob != '';
