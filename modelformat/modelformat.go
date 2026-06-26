// Package modelformat identifies a model file's real format from its leading bytes
// and refuses the formats that execute code when loaded.
//
// A model file's name lies: a ".bin" or ".pt" is usually a Python pickle (or a zip of
// pickles) that runs arbitrary code the moment a runtime loads it, regardless of the
// extension. So the decision of whether a file is safe to parse is made from its
// content, not its name, and it is an allowlist: only the formats that are pure data
// (GGUF and safetensors) are admitted; pickles, zip archives, and anything
// unrecognized are refused. This is the gate in front of any model load; the actual
// parsing of an admitted file (for example by package gguf) happens after it passes.
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
	// FormatGGUF is the single-file quantized format local runtimes load: pure data.
	FormatGGUF Format = "gguf"
	// FormatSafetensors is a header-prefixed tensor container with no code: pure data.
	FormatSafetensors Format = "safetensors"
	// FormatPickle is a Python pickle, which runs arbitrary code on load.
	FormatPickle Format = "pickle"
	// FormatZip is a zip archive (PyTorch .pt/.bin are zips of pickles): code on load.
	FormatZip Format = "zip"
	// FormatUnknown is anything not recognized; it is refused, since an unknown format
	// cannot be vouched for.
	FormatUnknown Format = "unknown"
)

// headerLen is how many leading bytes are enough to identify any format below.
const headerLen = 16

// maxSafetensorsHeader bounds the safetensors header-length prefix that is considered
// plausible, so a random 8 bytes are not misread as a safetensors header.
const maxSafetensorsHeader = 1 << 30 // 1 GiB of JSON header is already absurd

// SafeToParse reports whether a format is pure data and so safe to hand to a parser.
// Only GGUF and safetensors qualify; everything else, including unknown, does not.
func (f Format) SafeToParse() bool {
	return f == FormatGGUF || f == FormatSafetensors
}

// Detect identifies the format from a file's leading bytes. It never fails: bytes it
// does not recognize are FormatUnknown. Pass at least the first headerLen bytes (fewer
// is allowed; a short or empty input is simply unrecognized).
func Detect(head []byte) Format {
	switch {
	case len(head) >= 4 && string(head[:4]) == "GGUF":
		return FormatGGUF
	case len(head) >= 4 && head[0] == 'P' && head[1] == 'K' &&
		(head[2] == 0x03 || head[2] == 0x05 || head[2] == 0x07):
		// PK\x03\x04 (local file), PK\x05\x06 (empty archive), PK\x07\x08 (spanned).
		return FormatZip
	case len(head) >= 2 && head[0] == 0x80 && head[1] >= 1 && head[1] <= 5:
		// Pickle protocol 2 through 5 open with the PROTO opcode (0x80) and a version.
		return FormatPickle
	case looksSafetensors(head):
		return FormatSafetensors
	default:
		return FormatUnknown
	}
}

// looksSafetensors reports whether the bytes begin like a safetensors file: an 8-byte
// little-endian header length within a plausible range, immediately followed by the
// opening brace of the JSON header.
func looksSafetensors(head []byte) bool {
	if len(head) < 9 {
		return false
	}
	n := binary.LittleEndian.Uint64(head[:8])
	return n >= 2 && n <= maxSafetensorsHeader && head[8] == '{'
}

// Check reads a file's leading bytes from r, identifies the format, and returns a
// Forbidden error naming it when it is not safe to parse, so a caller refuses to load a
// code-executing or unrecognized model file rather than handing it to a runtime. On a
// safe format it returns the format and a nil error.
func Check(r io.Reader) (Format, error) {
	head := make([]byte, headerLen)
	n, err := io.ReadFull(r, head)
	// A file shorter than headerLen is fine to identify from what there is; only a hard
	// read error (not a short read) is a real failure.
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
