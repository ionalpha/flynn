package chain

import "crypto/sha256"

// hashSize is the width of every stored Merkle node hash. The log is defined over the
// RFC 6962 SHA-256 hasher, so every leaf and internal-node hash is exactly this many
// bytes; the tiled layout addresses a node inside a tile by offset and relies on the
// fixed width.
const hashSize = sha256.Size

// tileHeight sets the tile width as a power of two: each tile holds up to 2^tileHeight
// node hashes at one level. A growing log writes nodes left to right at each level, so
// a tile fills densely and can be paged out as a single fixed-width blob, which is what
// lets proof material move to slower storage without keeping the whole node set in
// memory.
const (
	tileHeight = 8
	tileWidth  = 1 << tileHeight // 256 hashes per tile
)

// NodeStore holds the Merkle node hashes keyed by (level, index): Append writes each
// completed node and proof assembly reads them back. Separating node storage behind
// this interface is what lets one ever-growing log keep only the O(log n) append
// frontier in memory (the compact range) while proofs are assembled from nodes that
// may live packed in a tile, on disk, or in a colder tier. A durable store implements
// it to persist a long-lived stream's proof material without holding it all in memory.
//
// A node is written once and never changes, so a stored hash is stable and a Node
// result may be retained without copying.
type NodeStore interface {
	// Node returns the hash at (level, index) and whether it is present.
	Node(level uint, index uint64) ([]byte, bool, error)
	// PutNode records the hash at (level, index).
	PutNode(level uint, index uint64, hash []byte) error
}

// cloneableStore is a NodeStore that can snapshot itself for a sealed run. The
// in-memory stores implement it; a durable store need not, because a run sealed
// through a Builder always accumulates its nodes in memory.
type cloneableStore interface {
	NodeStore
	clone() NodeStore
}

// memNodeStore is the direct in-memory node map: one entry per node. It is the default
// store and preserves the original in-RAM layout, so a caller that does not page nodes
// out behaves exactly as before.
type memNodeStore struct {
	nodes map[nodeKey][]byte
}

func newMemNodeStore() *memNodeStore {
	return &memNodeStore{nodes: map[nodeKey][]byte{}}
}

func (s *memNodeStore) Node(level uint, index uint64) ([]byte, bool, error) {
	h, ok := s.nodes[nodeKey{level: level, index: index}]
	return h, ok, nil
}

func (s *memNodeStore) PutNode(level uint, index uint64, hash []byte) error {
	s.nodes[nodeKey{level: level, index: index}] = hash
	return nil
}

func (s *memNodeStore) clone() NodeStore {
	c := &memNodeStore{nodes: make(map[nodeKey][]byte, len(s.nodes))}
	for k, v := range s.nodes {
		c.nodes[k] = v
	}
	return c
}

// tileID addresses a tile: a run of up to tileWidth node hashes at one level.
type tileID struct {
	level uint
	index uint64 // node index / tileWidth
}

// tiledNodeStore packs node hashes into fixed-width tiles: a tile is the concatenation
// of up to tileWidth consecutive node hashes at one level, addressed by offset. A
// durable store persists one blob per tile, so holding nodes as tiles rather than one
// map entry each is what bounds how much of a long log's proof material has to be
// resident to serve a proof. A tile fills densely from the left as the log grows; the
// stored length marks the filled prefix, so an unwritten slot is simply absent rather
// than an ambiguous zero hash.
type tiledNodeStore struct {
	tiles map[tileID][]byte
}

func newTiledNodeStore() *tiledNodeStore {
	return &tiledNodeStore{tiles: map[tileID][]byte{}}
}

func (s *tiledNodeStore) Node(level uint, index uint64) ([]byte, bool, error) {
	b, ok := s.tiles[tileID{level: level, index: index / tileWidth}]
	if !ok {
		return nil, false, nil
	}
	off := int(index%tileWidth) * hashSize
	if off+hashSize > len(b) {
		return nil, false, nil
	}
	return b[off : off+hashSize], true, nil
}

func (s *tiledNodeStore) PutNode(level uint, index uint64, hash []byte) error {
	id := tileID{level: level, index: index / tileWidth}
	off := int(index%tileWidth) * hashSize
	b := s.tiles[id]
	if len(b) < off+hashSize {
		grown := make([]byte, off+hashSize)
		copy(grown, b)
		b = grown
	}
	copy(b[off:off+hashSize], hash)
	s.tiles[id] = b
	return nil
}

func (s *tiledNodeStore) clone() NodeStore {
	c := &tiledNodeStore{tiles: make(map[tileID][]byte, len(s.tiles))}
	for k, v := range s.tiles {
		nb := make([]byte, len(v))
		copy(nb, v)
		c.tiles[k] = nb
	}
	return c
}
