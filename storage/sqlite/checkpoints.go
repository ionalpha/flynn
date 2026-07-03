package sqlite

// This file holds durable signed checkpoints: a stream's tree heads. A checkpoint binds
// a COSE signature to the Merkle root over the first N events, so a reopened log
// recovers its authenticated length and a verifier can prove the append-only property
// between any two saved heads with a consistency proof. Checkpoints are tiny and kept
// forever; the events they commit to remain the source of truth.

import (
	"context"
	"database/sql"
	"errors"
)

// SaveCheckpoint persists a signed tree head for a stream: the COSE-signed commitment
// to the Merkle root over the first size events. Every checkpoint is kept, keyed by
// size, so a verifier can anchor a consistency proof at any earlier head; re-saving the
// same size replaces its signature.
func (s *Store) SaveCheckpoint(ctx context.Context, stream string, size uint64, cose []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO checkpoints (stream, size, cose) VALUES (?,?,?)
		 ON CONFLICT(stream, size) DO UPDATE SET cose = excluded.cose`,
		stream, size, cose)
	return err
}

// LatestCheckpoint returns the newest signed tree head for a stream (the greatest
// size), or ok=false when the stream has none. A reopened log recovers its
// authenticated length from it and rebuilds its frontier from the tiles at that size.
func (s *Store) LatestCheckpoint(ctx context.Context, stream string) (size uint64, cose []byte, ok bool, err error) {
	err = s.reads().QueryRowContext(ctx,
		`SELECT size, cose FROM checkpoints WHERE stream = ? ORDER BY size DESC LIMIT 1`,
		stream).Scan(&size, &cose)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	return size, cose, true, nil
}

// CheckpointAt returns the signed tree head stored at exactly size for a stream, or
// ok=false when none is stored there. It is the earlier head a consistency proof is
// anchored to when proving the log grew append-only from that point.
func (s *Store) CheckpointAt(ctx context.Context, stream string, size uint64) (cose []byte, ok bool, err error) {
	err = s.reads().QueryRowContext(ctx,
		`SELECT cose FROM checkpoints WHERE stream = ? AND size = ?`,
		stream, size).Scan(&cose)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return cose, true, nil
}
