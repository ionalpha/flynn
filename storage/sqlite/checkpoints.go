package sqlite

import (
	"context"
	"database/sql"
	"errors"
)

// SaveCheckpoint persists a COSE-signed commitment to the Merkle root over the
// first size events. Every checkpoint is kept by size, so a consistency proof
// can anchor at any earlier head; re-saving a size replaces its signature.
func (s *Store) SaveCheckpoint(ctx context.Context, stream string, size uint64, cose []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO checkpoints (stream, size, cose) VALUES (?,?,?)
		 ON CONFLICT(stream, size) DO UPDATE SET cose = excluded.cose`,
		stream, size, cose)
	return err
}

// LatestCheckpoint returns the greatest-size head, or ok=false. A reopened log
// recovers its authenticated length from it.
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

// CheckpointAt returns the head stored at exactly size, or ok=false.
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
