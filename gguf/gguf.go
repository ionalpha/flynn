// Package gguf reads the metadata header of a GGUF model file safely.
//
// GGUF is the single-file format local model runtimes load. Its parser is a real
// code-execution surface: malformed files have repeatedly exploited the C parsers in
// popular runtimes. This reader exists so the metadata a downloaded model carries,
// above all its embedded chat template, can be inspected and overridden in memory-safe
// Go before the file is ever handed to a runtime, rather than trusting that runtime's
// parser to read it. It reads only the header and metadata, never the tensor data, and
// is hardened against hostile input: every length and count is bounded, so a malicious
// file is rejected rather than exhausting memory or looping without end.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ggufMagic is "GGUF" as a little-endian uint32, the first four bytes of the file.
const ggufMagic = 0x46554747

// Bounds on what the reader will accept before declaring a file hostile or corrupt.
// They are generous for real models (a vocabulary array runs to a few hundred thousand
// entries, a chat template to a few kilobytes) yet small enough that a crafted header
// cannot drive a large allocation or an unbounded loop.
const (
	maxKVCount     = 1 << 20 // metadata entries
	maxKeyLen      = 1 << 16 // bytes in a metadata key
	maxStringLen   = 1 << 26 // bytes in a metadata string value
	maxArrayCount  = 1 << 30 // elements in a metadata array (skipped, never allocated)
	maxArrayNest   = 8       // depth of nested arrays
	chatTemplateKW = "tokenizer.chat_template"
)

// GGUF metadata value type tags, from the format specification.
const (
	typeUint8 uint32 = iota
	typeInt8
	typeUint16
	typeInt16
	typeUint32
	typeInt32
	typeFloat32
	typeBool
	typeString
	typeArray
	typeUint64
	typeInt64
	typeFloat64
)

// ErrNotGGUF is returned when the data does not begin with the GGUF magic, so a caller
// can tell "this is not a GGUF file" apart from "this GGUF file is malformed".
var ErrNotGGUF = errors.New("gguf: not a GGUF file")

// Metadata is the string-valued metadata read from a GGUF header. Only string values
// are retained, since the security-relevant fields (the chat template, the
// architecture, the name) are strings; numeric and array values are validated and
// skipped, not stored.
type Metadata struct {
	Version uint32
	strings map[string]string
}

// String returns the value of a string metadata key.
func (m *Metadata) String(key string) (string, bool) {
	v, ok := m.strings[key]
	return v, ok
}

// ChatTemplate returns the model-embedded chat template, if the file carries one.
//
// The returned value is UNTRUSTED: a hostile model can embed a template that rewrites
// the prompt contract to inject instructions at inference. Do not feed it to a prompt.
// Use ChooseChatTemplate to decide what template to actually run with.
func (m *Metadata) ChatTemplate() (string, bool) { return m.String(chatTemplateKW) }

// Architecture returns the model architecture label (e.g. "llama"), or "" if absent.
func (m *Metadata) Architecture() string { v, _ := m.String("general.architecture"); return v }

// Name returns the model's self-reported name, or "" if absent.
func (m *Metadata) Name() string { v, _ := m.String("general.name"); return v }

// ReadMetadata reads the GGUF header and metadata from r, stopping before the tensor
// data. It returns ErrNotGGUF if the magic does not match, and a descriptive error if
// the header is malformed or exceeds the safety bounds.
func ReadMetadata(r io.Reader) (*Metadata, error) {
	br := bufio.NewReader(r)

	magic, err := readU32(br)
	if err != nil {
		return nil, fmt.Errorf("gguf: read magic: %w", err)
	}
	if magic != ggufMagic {
		return nil, ErrNotGGUF
	}

	version, err := readU32(br)
	if err != nil {
		return nil, fmt.Errorf("gguf: read version: %w", err)
	}
	// Versions 2 and 3 share the 64-bit-count metadata layout this reader parses.
	// Version 1 used 32-bit counts and a different layout; it is long obsolete and is
	// refused rather than parsed by guesswork.
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("gguf: unsupported version %d", version)
	}

	// Tensor count is read to advance past it; the tensor table and data are not read.
	if _, err := readU64(br); err != nil {
		return nil, fmt.Errorf("gguf: read tensor count: %w", err)
	}
	kvCount, err := readU64(br)
	if err != nil {
		return nil, fmt.Errorf("gguf: read metadata count: %w", err)
	}
	if kvCount > maxKVCount {
		return nil, fmt.Errorf("gguf: metadata count %d exceeds limit", kvCount)
	}

	m := &Metadata{Version: version, strings: make(map[string]string)}
	for i := range kvCount {
		key, err := readString(br, maxKeyLen)
		if err != nil {
			return nil, fmt.Errorf("gguf: read key %d: %w", i, err)
		}
		vtype, err := readU32(br)
		if err != nil {
			return nil, fmt.Errorf("gguf: read type of %q: %w", key, err)
		}
		if vtype == typeString {
			val, err := readString(br, maxStringLen)
			if err != nil {
				return nil, fmt.Errorf("gguf: read value of %q: %w", key, err)
			}
			m.strings[key] = val
			continue
		}
		if err := skipValue(br, vtype, 0); err != nil {
			return nil, fmt.Errorf("gguf: skip value of %q: %w", key, err)
		}
	}
	return m, nil
}

// skipValue advances past a non-string value of the given type without retaining it.
// Arrays are walked element by element so a hostile element type or nesting is caught,
// and depth is bounded so nested arrays cannot exhaust the stack.
func skipValue(br *bufio.Reader, vtype uint32, depth int) error {
	if sz, ok := fixedSize(vtype); ok {
		return discard(br, int64(sz))
	}
	switch vtype {
	case typeString:
		n, err := readU64(br)
		if err != nil {
			return err
		}
		if n > maxStringLen {
			return fmt.Errorf("string length %d exceeds limit", n)
		}
		return discard(br, int64(n))
	case typeArray:
		if depth >= maxArrayNest {
			return errors.New("array nesting too deep")
		}
		elemType, err := readU32(br)
		if err != nil {
			return err
		}
		count, err := readU64(br)
		if err != nil {
			return err
		}
		if count > maxArrayCount {
			return fmt.Errorf("array count %d exceeds limit", count)
		}
		// Fixed-size elements (the common case, e.g. a vocabulary of token ids) are
		// skipped in one step, so a large but valid count costs nothing and a hostile
		// count cannot drive a per-element loop. Variable-size elements are walked.
		if sz, ok := fixedSize(elemType); ok {
			return discard(br, int64(count)*int64(sz))
		}
		for range count {
			if err := skipValue(br, elemType, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown value type %d", vtype)
	}
}

// readString reads a GGUF string: a uint64 length followed by that many bytes, with
// the length bounded by limit so a hostile size cannot drive a large allocation.
func readString(br *bufio.Reader, limit uint64) (string, error) {
	n, err := readU64(br)
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", fmt.Errorf("string length %d exceeds limit %d", n, limit)
	}
	if n == 0 {
		return "", nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// discard advances n bytes, reporting an error if the input ends first.
func discard(br *bufio.Reader, n int64) error {
	if n < 0 {
		return errors.New("negative length")
	}
	if _, err := io.CopyN(io.Discard, br, n); err != nil {
		return err
	}
	return nil
}

// fixedSize returns the byte size of a fixed-width value type, and false for the
// variable-width types (string, array).
func fixedSize(vtype uint32) (int, bool) {
	switch vtype {
	case typeUint8, typeInt8, typeBool:
		return 1, true
	case typeUint16, typeInt16:
		return 2, true
	case typeUint32, typeInt32, typeFloat32:
		return 4, true
	case typeUint64, typeInt64, typeFloat64:
		return 8, true
	default:
		return 0, false
	}
}

func readU32(br *bufio.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(br, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func readU64(br *bufio.Reader) (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(br, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}
