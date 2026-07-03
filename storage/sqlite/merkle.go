package sqlite

// This file holds the durable, tiled Merkle node store: the storage implementation of
// chain.NodeStore that lets one ever-growing per-stream log keep only its append
// frontier in memory while proof material lives in fixed-width tiles on disk. Nodes are
// packed into tiles of merkleTileWidth hashes at one level; a full tile is persisted
// and evicted, so the resident set is the O(log n) rightmost partial tile per level.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/chain"
)

// merkleTileWidth is the number of node hashes packed into one tile blob. It is the
// storage layout's own paging unit; the tree the tiles back is defined only by the
// RFC 6962 proof format, so this width can change without changing what the log proves.
const merkleTileWidth = 256

// merkleHashSize is the width of every stored node hash: the log is defined over the
// SHA-256 tree hasher, so a tile blob is a run of fixed-width hashes addressed by
// offset.
const merkleHashSize = sha256.Size

// tileKey addresses a tile within a stream: a run of up to merkleTileWidth node hashes
// at one level.
type tileKey struct {
	level uint
	index uint64
}

// MerkleNodeStore is the durable, tiled chain.NodeStore for one stream. It writes nodes
// into fixed-width tiles: the rightmost partial tile per level is held in memory and
// written back to SQLite when it fills or on Flush, then evicted, so a long-lived log's
// resident proof material is bounded by its height rather than its length. Reads of an
// evicted tile load its blob from SQLite. A MerkleNodeStore is not safe for concurrent
// use; one belongs to one stream's single-writer append path.
type MerkleNodeStore struct {
	p      *Store
	stream string
	// active holds partial tiles not yet known to be full, keyed by (level, tile). A
	// tile leaves active once full (persisted and evicted) so only the frontier stays
	// resident; a later read of a full tile comes from SQLite.
	active map[tileKey][]byte
}

var _ chain.NodeStore = (*MerkleNodeStore)(nil)

// MerkleNodes returns the durable Merkle node store for a stream, so the stream's
// verifiable log can page its proof nodes into tiles instead of holding them all in
// memory. Reopen a persisted log's tree with chain.LoadTree over the returned store.
func (s *Store) MerkleNodes(stream string) *MerkleNodeStore {
	return &MerkleNodeStore{p: s, stream: stream, active: map[tileKey][]byte{}}
}

// Node implements chain.NodeStore: the hash at (level, index), from the resident tile
// if present, else the tile's persisted blob. A missing node reports absent, not an
// error, so proof assembly names it precisely.
func (m *MerkleNodeStore) Node(level uint, index uint64) ([]byte, bool, error) {
	tk := tileKey{level: level, index: index / merkleTileWidth}
	off := int(index%merkleTileWidth) * merkleHashSize
	if b, ok := m.active[tk]; ok {
		return sliceNode(b, off)
	}
	var b []byte
	err := m.p.reads().QueryRowContext(context.Background(),
		`SELECT hashes FROM merkle_tiles WHERE stream = ? AND level = ? AND tile_index = ?`,
		m.stream, level, tk.index).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return sliceNode(b, off)
}

// PutNode implements chain.NodeStore: it records the hash at (level, index) into the
// tile's resident blob, growing the blob's filled prefix. When the write fills the tile
// it is persisted and evicted from memory; a tile that is not yet full is persisted on
// Flush. A node is written once, so this never overwrites a different hash.
func (m *MerkleNodeStore) PutNode(level uint, index uint64, hash []byte) error {
	if len(hash) != merkleHashSize {
		return fmt.Errorf("sqlite: merkle node hash is %d bytes, want %d", len(hash), merkleHashSize)
	}
	tk := tileKey{level: level, index: index / merkleTileWidth}
	slot := int(index % merkleTileWidth)
	off := slot * merkleHashSize

	b, ok := m.active[tk]
	if !ok {
		// Resume a partial tile that was persisted and evicted (a reopened log's
		// rightmost tile), so continued appends extend it rather than truncate it.
		loaded, lerr := m.loadTile(tk)
		if lerr != nil {
			return lerr
		}
		b = loaded
	}
	if len(b) < off+merkleHashSize {
		grown := make([]byte, off+merkleHashSize)
		copy(grown, b)
		b = grown
	}
	copy(b[off:off+merkleHashSize], hash)
	m.active[tk] = b

	if slot == merkleTileWidth-1 {
		// The tile is full and immutable: persist it and drop it from the resident
		// set so memory tracks the frontier, not the whole log.
		if err := m.persist(tk, b); err != nil {
			return err
		}
		delete(m.active, tk)
	}
	return nil
}

// Flush persists every resident partial tile, so the log's proof material up to the
// current size is durable. Call it before sealing a checkpoint (and before closing),
// so a reopen through chain.LoadTree finds every frontier node.
func (m *MerkleNodeStore) Flush() error {
	for tk, b := range m.active {
		if err := m.persist(tk, b); err != nil {
			return err
		}
	}
	return nil
}

// Resident reports how many partial tiles are held in memory. It is the O(log n)
// frontier of the log, not its length; tests assert the bound to guard against a
// regression that stops evicting full tiles.
func (m *MerkleNodeStore) Resident() int { return len(m.active) }

// loadTile reads a tile's persisted blob, or an empty blob when the tile does not
// exist yet.
func (m *MerkleNodeStore) loadTile(tk tileKey) ([]byte, error) {
	var b []byte
	err := m.p.reads().QueryRowContext(context.Background(),
		`SELECT hashes FROM merkle_tiles WHERE stream = ? AND level = ? AND tile_index = ?`,
		m.stream, tk.level, tk.index).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// persist writes a tile blob, replacing any earlier partial blob for the same tile.
func (m *MerkleNodeStore) persist(tk tileKey, b []byte) error {
	_, err := m.p.db.ExecContext(context.Background(),
		`INSERT INTO merkle_tiles (stream, level, tile_index, hashes) VALUES (?,?,?,?)
		 ON CONFLICT(stream, level, tile_index) DO UPDATE SET hashes = excluded.hashes`,
		m.stream, tk.level, tk.index, b)
	return err
}

// sliceNode returns the fixed-width hash at off within a tile blob, or absent when the
// blob has not been filled that far.
func sliceNode(b []byte, off int) ([]byte, bool, error) {
	if off+merkleHashSize > len(b) {
		return nil, false, nil
	}
	return b[off : off+merkleHashSize], true, nil
}
