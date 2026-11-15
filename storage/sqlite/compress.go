package sqlite

// Warm-tier compression is pure-Go zstd, so the binary stays static with no
// cgo or SQLite extension. Hot blobs stay uncompressed because replay reads
// them on the common path; only rare archived reads pay the decode.

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// zstdEncoder is shared; EncodeAll is safe for concurrent use. Archival runs
// off the write path, so SpeedBetterCompression's extra time is cheap, and the
// level does not slow decoding.
var (
	zstdEncoder     *zstd.Encoder
	zstdEncoderOnce sync.Once
	zstdDecoder     *zstd.Decoder
	zstdDecoderOnce sync.Once
)

func encoder() *zstd.Encoder {
	zstdEncoderOnce.Do(func() {
		// These options cannot fail; the panic guards a future option change.
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
		// DecodeAll is safe for concurrent callers at any concurrency setting.
		dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			panic(fmt.Sprintf("sqlite: build zstd decoder: %v", err))
		}
		zstdDecoder = dec
	})
	return zstdDecoder
}

// compressBody returns a fresh slice that never aliases a pooled buffer.
func compressBody(raw []byte) []byte {
	return encoder().EncodeAll(raw, nil)
}

// decompressBody returns an error for a corrupt record, never a partial body.
func decompressBody(packed []byte) ([]byte, error) {
	raw, err := decoder().DecodeAll(packed, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: decompress warm body: %w", err)
	}
	return raw, nil
}
