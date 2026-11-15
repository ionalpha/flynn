// Package ndjson is the byte-level framing every headless transport shares: one JSON
// object per line, split on LF only.
//
// A record ends at U+000A. U+2028 and U+2029 inside a JSON string are data: a splitter
// that ended lines on them (JavaScript does) would cut a record in two and let one half
// read as a frame. [Reader] never looks at them, and [Writer] escapes both so such a
// consumer still sees one record. A record is UTF-8, a single object, at most [MaxLine]
// bytes and at most [MaxDepth] deep. A line that breaks a rule is reported as a
// [*FrameError] with the reader already past its LF, so one bad line never costs the
// lines after it.
package ndjson

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	// MaxLine is the longest accepted inbound line, in bytes, not counting the LF.
	MaxLine = 1 << 20
	// MaxDepth is the deepest accepted nesting of arrays and objects. The top-level
	// object is depth 1.
	MaxDepth = 64
)

// Kind classifies a line the reader refused.
type Kind int

// The kinds of refused line. A transport maps each to its own error code.
const (
	// KindOversize is a line longer than [MaxLine]. Its bytes were discarded as they
	// arrived, never buffered.
	KindOversize Kind = iota + 1
	// KindDepth is a line nested deeper than [MaxDepth].
	KindDepth
	// KindBatch is a line whose top-level value is an array. Batches are not part of
	// the protocol.
	KindBatch
	// KindNotObject is a line whose top-level value is not an object.
	KindNotObject
	// KindEncoding is a line that is not valid UTF-8.
	KindEncoding
	// KindUnterminated is a final line cut off by the end of the stream. It is refused
	// because whoever controls where a stream ends could shorten a frame into a
	// different one.
	KindUnterminated
)

// String names the kind for logs and test failures.
func (k Kind) String() string {
	switch k {
	case KindOversize:
		return "oversize"
	case KindDepth:
		return "depth"
	case KindBatch:
		return "batch"
	case KindNotObject:
		return "not_object"
	case KindEncoding:
		return "encoding"
	case KindUnterminated:
		return "unterminated"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// FrameError reports a line the reader refused. The stream is still usable after one:
// the next call to [Reader.Next] reads the following line.
type FrameError struct {
	Kind Kind
	// Size is the refused line's length in bytes, without the LF. For an oversize line
	// it is the true length, though the bytes were not kept.
	Size int64
}

// Error implements error.
func (e *FrameError) Error() string {
	return fmt.Sprintf("ndjson: refused %s line of %d bytes", e.Kind, e.Size)
}

// Reader reads LF-delimited records from a stream.
type Reader struct {
	br   *bufio.Reader
	line []byte
}

// readChunk is the size of the reader's fixed read buffer. It bounds what one pass over
// an oversize line holds, whatever the line's length.
const readChunk = 32 << 10

// NewReader returns a Reader over r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, readChunk)}
}

// Next returns the next record, without its LF. The slice is valid until the next call.
// Blank lines are skipped. A refused line returns a [*FrameError] and leaves the reader
// at the start of the next line. At the end of the stream it returns [io.EOF]; any other
// error is the underlying reader's.
func (r *Reader) Next() ([]byte, error) {
	for {
		line, size, terminated, err := r.readLine()
		if err != nil {
			return nil, err
		}
		if size > MaxLine {
			return nil, &FrameError{Kind: KindOversize, Size: size}
		}
		if !terminated {
			return nil, &FrameError{Kind: KindUnterminated, Size: size}
		}
		first := firstNonSpace(line)
		if first < 0 {
			continue
		}
		if !utf8.Valid(line) {
			return nil, &FrameError{Kind: KindEncoding, Size: size}
		}
		switch line[first] {
		case '{':
		case '[':
			return nil, &FrameError{Kind: KindBatch, Size: size}
		default:
			return nil, &FrameError{Kind: KindNotObject, Size: size}
		}
		if tooDeep(line) {
			return nil, &FrameError{Kind: KindDepth, Size: size}
		}
		return line, nil
	}
}

// readLine reads up to and including the next LF. Past [MaxLine] it counts and
// discards bytes, so the memory held is bounded by the cap and the read buffer. The
// line is nil for an oversize line.
func (r *Reader) readLine() (line []byte, size int64, terminated bool, err error) {
	r.line = r.line[:0]
	for {
		chunk, rerr := r.br.ReadSlice('\n')
		if rerr == nil {
			chunk = chunk[:len(chunk)-1]
		}
		size += int64(len(chunk))
		if size <= MaxLine {
			r.line = append(r.line, chunk...)
		} else {
			r.line = r.line[:0]
		}
		switch {
		case rerr == nil:
			return r.line, size, true, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			if size == 0 {
				return nil, 0, false, io.EOF
			}
			return r.line, size, false, nil
		default:
			return nil, size, false, rerr
		}
	}
}

// firstNonSpace returns the index of the first byte that is not JSON whitespace, or -1
// when the line is blank.
func firstNonSpace(b []byte) int {
	for i, c := range b {
		switch c {
		case ' ', '\t', '\r':
		default:
			return i
		}
	}
	return -1
}

// tooDeep reports whether line nests deeper than [MaxDepth]. It is one pass without
// recursion, so a hostile line cannot exhaust the stack, and it stops at the first
// byte past the limit. Brackets inside strings do not count.
func tooDeep(line []byte) bool {
	depth := 0
	inString := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inString {
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > MaxDepth {
				return true
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return false
}
