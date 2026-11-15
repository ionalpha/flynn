// Package ids generates time-sortable unique identifiers (UUIDv7): a 48-bit
// millisecond timestamp followed by randomness. Creation-order sorting makes inserts
// land at an index's tail, and they cannot collide across a syncing fleet.
//
// Time and entropy are injectable, so a replay reproduces the same IDs.
package ids

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"sync"

	"github.com/ionalpha/flynn/clock"
)

// Generator produces UUIDv7 identifiers from an injectable clock and entropy
// source. Construct it with NewGenerator; the zero value is unusable. It is safe for
// concurrent use.
type Generator struct {
	clk  clock.Clock
	rand io.Reader
	mu   sync.Mutex
}

// Option configures a Generator.
type Option func(*Generator)

// WithClock sets the time source (default: clock.System).
func WithClock(c clock.Clock) Option {
	return func(g *Generator) {
		if c != nil {
			g.clk = c
		}
	}
}

// WithEntropy sets the randomness source (default: crypto/rand). A deterministic
// reader makes IDs reproducible for replay.
func WithEntropy(r io.Reader) Option {
	return func(g *Generator) {
		if r != nil {
			g.rand = r
		}
	}
}

// NewGenerator returns a Generator on the system clock and crypto/rand unless
// options say otherwise.
func NewGenerator(opts ...Option) *Generator {
	g := &Generator{clk: clock.System{}, rand: rand.Reader}
	for _, o := range opts {
		o(g)
	}
	return g
}

// New returns a new UUIDv7 in canonical 8-4-4-4-12 form.
func (g *Generator) New() string {
	var b [16]byte
	// 48-bit big-endian millisecond timestamp.
	ms := g.clk.Now().UnixMilli()
	b[0] = byte((ms >> 40) & 0xff)
	b[1] = byte((ms >> 32) & 0xff)
	b[2] = byte((ms >> 24) & 0xff)
	b[3] = byte((ms >> 16) & 0xff)
	b[4] = byte((ms >> 8) & 0xff)
	b[5] = byte(ms & 0xff)

	g.mu.Lock()
	_, _ = io.ReadFull(g.rand, b[6:])
	g.mu.Unlock()

	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return format(b)
}

func format(b [16]byte) string {
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}

// defaultTokenBytes is 256 bits, past the brute-force horizon for a bearer secret.
const defaultTokenBytes = 32

// Token returns a random bearer secret of nBytes entropy (256 bits when
// non-positive), URL-safe base64 without padding. It carries no timestamp, so it
// reveals nothing about when it was made. A deterministic entropy source
// reproduces it for replay.
func (g *Generator) Token(nBytes int) (string, error) {
	if nBytes <= 0 {
		nBytes = defaultTokenBytes
	}
	b := make([]byte, nBytes)
	g.mu.Lock()
	_, err := io.ReadFull(g.rand, b)
	g.mu.Unlock()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var def = NewGenerator()

// New returns a new UUIDv7 from the default generator.
func New() string { return def.New() }

// Entropy returns g's randomness source, or the default generator's when g is nil.
// A caller needing raw bytes (minting a keypair) takes them here, so the process has
// one injectable source and a replay stays deterministic.
func Entropy(g *Generator) io.Reader {
	if g == nil {
		g = def
	}
	return g.rand
}

// Token returns an opaque 256-bit bearer secret from the default generator.
func Token() (string, error) { return def.Token(defaultTokenBytes) }
