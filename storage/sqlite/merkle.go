package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/chain"
)

// merkleTileWidth is hashes per tile. The tree is defined by RFC 6962, so the
// width can change without changing what the log proves.
const merkleTileWidth = 256

// merkleHashSize is the SHA-256 width; tiles address hashes by offset.
const merkleHashSize = sha256.Size

type tileKey struct {
	level uint
	index uint64
}

// MerkleNodeStore is a tiled [chain.NodeStore] for one stream. Only each
// level's partial tile stays in memory, so memory grows with tree height, not
// length. Not safe for concurrent use: it belongs to one stream's single
// writer.
type MerkleNodeStore struct {
	p      *Store
	stream string
	// active holds partial tiles; a full one is persisted and evicted.
	active map[tileKey][]byte
}

var _ chain.NodeStore = (*MerkleNodeStore)(nil)

// MerkleNodes returns the node store for a stream. Reopen a tree with
// [chain.LoadTree] over it.
func (s *Store) MerkleNodes(stream string) *MerkleNodeStore {
	return &MerkleNodeStore{p: s, stream: stream, active: map[tileKey][]byte{}}
}

// Node reports a missing node as absent with no error, so proof assembly can
// name it.
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

// PutNode persists and evicts a tile when it fills; partial tiles wait for
// Flush. Nodes are written once, so no hash is overwritten.
func (m *MerkleNodeStore) PutNode(level uint, index uint64, hash []byte) error {
	if len(hash) != merkleHashSize {
		return fmt.Errorf("sqlite: merkle node hash is %d bytes, want %d", len(hash), merkleHashSize)
	}
	tk := tileKey{level: level, index: index / merkleTileWidth}
	slot := int(index % merkleTileWidth)
	off := slot * merkleHashSize

	b, ok := m.active[tk]
	if !ok {
		// Load a reopened log's partial tile, so appends extend it.
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
		if err := m.persist(tk, b); err != nil {
			return err
		}
		delete(m.active, tk)
	}
	return nil
}

// Flush persists partial tiles. Call it before sealing a checkpoint or
// closing, so [chain.LoadTree] finds every frontier node.
func (m *MerkleNodeStore) Flush() error {
	//ordered:ok each tile is its own row, so stored state does not depend on order
	for tk, b := range m.active {
		if err := m.persist(tk, b); err != nil {
			return err
		}
	}
	return nil
}

// Resident reports partial tiles in memory, O(log n); tests assert the bound
// to catch a store that stops evicting.
func (m *MerkleNodeStore) Resident() int { return len(m.active) }

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

func (m *MerkleNodeStore) persist(tk tileKey, b []byte) error {
	_, err := m.p.db.ExecContext(context.Background(),
		`INSERT INTO merkle_tiles (stream, level, tile_index, hashes) VALUES (?,?,?,?)
		 ON CONFLICT(stream, level, tile_index) DO UPDATE SET hashes = excluded.hashes`,
		m.stream, tk.level, tk.index, b)
	return err
}

func sliceNode(b []byte, off int) ([]byte, bool, error) {
	if off+merkleHashSize > len(b) {
		return nil, false, nil
	}
	return b[off : off+merkleHashSize], true, nil
}
