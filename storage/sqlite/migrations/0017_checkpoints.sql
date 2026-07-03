-- Signed checkpoints: a stream's tree heads. Each row is a COSE-signed commitment to
-- the Merkle root over the first `size` events of a stream, so a reopened log recovers
-- its authenticated length from the latest checkpoint and rebuilds its frontier from
-- the tiles. Keeping every checkpoint (not just the newest) lets a verifier prove the
-- append-only property between any two of them with a consistency proof. Checkpoints
-- are tiny and stay resident forever; the events they commit to are the source of
-- truth, so a dropped checkpoint only forces a re-fold, never a wrong answer.
CREATE TABLE checkpoints (
  stream TEXT    NOT NULL,
  size   INTEGER NOT NULL,
  cose   BLOB    NOT NULL,
  PRIMARY KEY (stream, size)
) WITHOUT ROWID;
