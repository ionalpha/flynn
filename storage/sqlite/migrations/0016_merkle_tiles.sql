-- Merkle node tiles: the proof material of a stream's append-only log, packed into
-- fixed-width tiles instead of one row per node. A tile holds up to 256 node hashes at
-- one level, concatenated, so a growing log's nodes page to storage while only the
-- append frontier stays in memory. Nodes are write-once: a tile grows until it is full
-- and is never rewritten after. This table is derived from the immutable events, so
-- dropping it only makes a proof unavailable until the log is refolded, never wrong.
-- One tile per (stream, level, tile_index); the key clusters a level's tiles in order.
CREATE TABLE merkle_tiles (
  stream     TEXT    NOT NULL,
  level      INTEGER NOT NULL,
  tile_index INTEGER NOT NULL,
  hashes     BLOB    NOT NULL,
  PRIMARY KEY (stream, level, tile_index)
) WITHOUT ROWID;
