package chain

import "crypto/sha256"

// hashSize is the width of every node hash (RFC 6962 SHA-256). The tiled layout
// addresses a node by offset and relies on the fixed width.
const hashSize = sha256.Size

// tileHeight sets the tile width: each tile holds up to 2^tileHeight node hashes
// at one level. A log writes nodes left to right, so a tile fills densely and
// pages out as one fixed-width blob.
const (
	tileHeight = 8
	tileWidth  = 1 << tileHeight // 256 hashes per tile
)

// NodeStore holds Merkle node hashes by (level, index). It lets a growing log keep
// only the O(log n) append frontier in memory while proofs read nodes from a
// tile, disk or a colder tier.
//
// A node is written once and never changes, so a Node result may be retained
// without copying.
type NodeStore interface {
	Node(level uint, index uint64) ([]byte, bool, error)
	PutNode(level uint, index uint64, hash []byte) error
}

// cloneableStore can snapshot itself for a sealed run. A durable store need not:
// a Builder always accumulates in memory.
type cloneableStore interface {
	NodeStore
	clone() NodeStore
}

// memNodeStore is the default store: one map entry per node.
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

// tiledNodeStore packs up to tileWidth consecutive node hashes per level into a
// tile, addressed by offset; a durable store persists one blob per tile. The
// stored length marks the filled prefix, so an unwritten slot is absent rather
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
