package sqlite

// Warm-tier body compression. A payload body that has aged into a sealed segment is
// held zstd-compressed in the warm store, not verbatim: the bodies are the bulk of the
// bytes and the hot log stays small once they move out. Compression is application-level
// and pure Go (klauspost/compress) - no loadable SQLite extension, no cgo, no VFS - so it
// keeps the single-static-binary discipline and stays swappable behind these two helpers.
//
// The hot `blobs` table is left uncompressed on purpose: the hot bodies are the recent
// ones a rebuild or replay reads on the common path, and paying a decode for those would
// tax the hot read. Only the warm tier, read on the rare replay-of-archived-history path,
// pays the decompress - where zstd's fast decode (allocation-free after warmup) keeps the
// cost of reaching back into sealed history low.

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// zstdEncoder compresses warm-tier bodies. It is built once and shared: a zstd.Encoder is
// safe for concurrent use through EncodeAll, which is stateless per call. SpeedBetter is
// the archival level - the move to warm runs off the hot write path, so the extra ratio
// over the default is worth the compress time, and decode speed is unaffected by the
// encode level. The nil-destination EncodeAll allocates a fresh slice per call, so a
// compressed body never aliases a pooled buffer.
var (
	zstdEncoder     *zstd.Encoder
	zstdEncoderOnce sync.Once
	zstdDecoder     *zstd.Decoder
	zstdDecoderOnce sync.Once
)

func encoder() *zstd.Encoder {
	zstdEncoderOnce.Do(func() {
		// EncodeAll ignores the concurrency setting (it is single-shot), so the only
		// knob that matters here is the level. An error is impossible with these
		// options, so the store-time panic can never fire in practice; it guards a
		// future option change rather than a runtime path.
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
		if err != nil {
			panic(fmt.Sprintf("sqlite: build zstd encoder: %v", err))
		}
		zstdEncoder = enc
	})
	return zstdEncoder
}

func decoder() *zstd.Decoder {
	zstdDecoderOnce.Do(func() {
		// A zstd.Decoder with no concurrency reads one stream at a time; DecodeAll is
		// safe for concurrent callers regardless, each decoding into its own slice.
		dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			panic(fmt.Sprintf("sqlite: build zstd decoder: %v", err))
		}
		zstdDecoder = dec
	})
	return zstdDecoder
}

// compressBody zstd-compresses a warm-tier body. It returns a freshly allocated slice, so
// the result is owned by the caller and safe to hand to the driver.
func compressBody(raw []byte) []byte {
	return encoder().EncodeAll(raw, nil)
}

// decompressBody restores a warm-tier body compressed by compressBody. A decode failure is
// a corrupt or truncated warm record and is returned as an error, never a partial body -
// the caller rehydrating an event surfaces it rather than handing back wrong bytes.
func decompressBody(packed []byte) ([]byte, error) {
	raw, err := decoder().DecodeAll(packed, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: decompress warm body: %w", err)
	}
	return raw, nil
}
