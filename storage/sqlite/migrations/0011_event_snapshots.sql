-- Stream snapshots: a materialized projection of a stream up to a Seq, so a
-- rebuild resumes from the latest snapshot and folds only the events after it
-- instead of replaying the whole stream. A snapshot is a derived cache over the
-- immutable events, never a replacement: dropping this table only makes a rebuild
-- slower, never wrong. One snapshot per (stream, seq); a newer one supersedes an
-- older by carrying a higher seq.
CREATE TABLE snapshots (
  stream  TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  payload BLOB    NOT NULL,
  PRIMARY KEY (stream, seq)
) WITHOUT ROWID;
