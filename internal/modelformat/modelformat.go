// Package modelformat identifies a model file's format from its leading bytes and
// refuses formats that execute code when loaded.
//
// A ".bin" or ".pt" is usually a Python pickle, or a zip of pickles, that runs
// arbitrary code when a runtime loads it, whatever the extension. So safety is
// decided from content against an allowlist: only the pure-data formats (GGUF and
// safetensors) pass, and pickles, zip archives and unknown formats are refused.
// Parsing an admitted file, as package gguf does, happens after this gate.
package modelformat

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/ionalpha/flynn/fault"
)

// Format is a model file format identified from its leading bytes.
type Format string

const (
	// FormatGGUF is the single-file quantized format local runtimes load; pure data.
	FormatGGUF Format = "gguf"
	// FormatSafetensors is a header-prefixed tensor container with no code; pure data.
	FormatSafetensors Format = "safetensors"
	// FormatPickle is a Python pickle, which runs arbitrary code on load.
	FormatPickle Format = "pickle"
	// FormatZip is a zip archive (PyTorch .pt and .bin are zips of pickles); code on
	// load.
	FormatZip Format = "zip"
	// FormatUnknown is anything unrecognized. It is refused, since it cannot be
	// vouched for.
	FormatUnknown Format = "unknown"
)

// headerLen is enough leading bytes to identify every format below.
const headerLen = 16

// maxSafetensorsHeader bounds a plausible safetensors header length, so 8 random
// bytes are not misread as a header.
const maxSafetensorsHeader = 1 << 30 // 1 GiB of JSON header is already absurd

// SafeToParse reports whether f is pure data and safe to parse: only GGUF and
// safetensors.
func (f Format) SafeToParse() bool {
	return f == FormatGGUF || f == FormatSafetensors
}

// Detect identifies the format from a file's leading bytes and never fails:
// unrecognized bytes, including a short or empty input, are FormatUnknown. Pass at
// least headerLen bytes.
func Detect(head []byte) Format {
	switch {
	case len(head) >= 4 && string(head[:4]) == "GGUF":
		return FormatGGUF
	case len(head) >= 4 && head[0] == 'P' && head[1] == 'K' &&
		(head[2] == 0x03 || head[2] == 0x05 || head[2] == 0x07):
		// PK\x03\x04 (local file), PK\x05\x06 (empty archive), PK\x07\x08 (spanned).
		return FormatZip
	case len(head) >= 2 && head[0] == 0x80 && head[1] >= 1 && head[1] <= 5:
		// Pickle protocols 2 to 5 open with the PROTO opcode (0x80) and a version.
		return FormatPickle
	case looksSafetensors(head):
		return FormatSafetensors
	default:
		return FormatUnknown
	}
}

// looksSafetensors reports whether head starts like a safetensors file: an 8-byte
// little-endian header length in a plausible range, then the JSON header's opening
// brace.
func looksSafetensors(head []byte) bool {
	if len(head) < 9 {
		return false
	}
	n := binary.LittleEndian.Uint64(head[:8])
	return n >= 2 && n <= maxSafetensorsHeader && head[8] == '{'
}

// Check reads a file's leading bytes from r and identifies the format. An unsafe
// format returns a Forbidden error naming it, so a caller never hands a
// code-executing or unrecognized model file to a runtime.
func Check(r io.Reader) (Format, error) {
	head := make([]byte, headerLen)
	n, err := io.ReadFull(r, head)
	// A file shorter than headerLen is identified from what there is; only a read
	// error other than a short read fails.
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return FormatUnknown, fmt.Errorf("modelformat: read header: %w", err)
	}
	f := Detect(head[:n])
	if !f.SafeToParse() {
		return f, fault.New(fault.Forbidden, "model_format_unsafe",
			fmt.Sprintf("modelformat: refusing a %s model file; only data-only formats (gguf, safetensors) are loaded, never one that executes code on load", f))
	}
	return f, nil
}
