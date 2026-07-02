// Package mdstream partitions streamed markdown into a stable prefix and a
// mutable tail. Streaming text into a terminal renderer has a conflict: text
// committed to scrollback is rendered once and never touched again, but
// markdown cannot be rendered correctly until its enclosing block is
// complete (a table cannot be laid out from half its rows; an unclosed
// fence's end is unknown). The splitter resolves it by advancing a commit
// boundary only at provably safe points, so the renderer commits the stable
// prefix exactly once and re-renders only the small mutable tail each frame.
//
// Safe boundaries are structural, not heuristic: the end of a line that
// closes a code fence, or a blank line outside any fence. Everything after
// the last safe boundary is tail. This yields the holdback behavior the
// session needs for free: a streaming table has no blank line inside it, so
// it stays in the tail (re-rendered whole each frame, laid out correctly
// once complete), and everything inside an open fence stays in the tail
// until the fence closes.
//
// The splitter never inspects render output and allocates nothing per write
// beyond the buffer append: it is a cursor over bytes, cheap enough to run
// on every streamed chunk.
package mdstream

import "strings"

// Splitter accumulates streamed markdown and tracks the commit boundary. The
// zero value is ready to use.
type Splitter struct {
	buf       []byte
	committed int // bytes before this offset have been returned by Stable
	scanned   int // bytes before this offset have been boundary-scanned
	boundary  int // the latest safe commit boundary found so far
	lineStart int // offset where the line being scanned begins
	inFence   bool
	fence     byte // the fence marker rune of the open fence (` or ~)
	fenceLen  int  // the opening fence's marker count (closing needs >=)
}

// Write appends the next streamed chunk and advances the boundary scan.
func (s *Splitter) Write(chunk string) {
	s.buf = append(s.buf, chunk...)
	s.scan()
}

// Stable returns the newly committed text since the last call: the part of
// the stream that is now provably safe to render and print exactly once.
func (s *Splitter) Stable() string {
	if s.boundary <= s.committed {
		return ""
	}
	out := string(s.buf[s.committed:s.boundary])
	s.committed = s.boundary
	return out
}

// Tail returns the mutable remainder after the commit boundary: the text a
// live region re-renders each frame.
func (s *Splitter) Tail() string {
	return string(s.buf[s.boundary:])
}

// Close marks the stream complete and returns any remaining uncommitted
// text: at end of stream every block is as complete as it will ever be, so
// the whole tail becomes stable.
func (s *Splitter) Close() string {
	out := string(s.buf[s.committed:])
	s.committed = len(s.buf)
	s.boundary = len(s.buf)
	return out
}

// scan advances line by line over the unscanned bytes, updating fence state
// and recording each safe boundary. Only complete lines are scanned: a line
// still missing its newline could still change meaning (a partial pair of
// backticks may become a fence), so the boundary never lands inside one.
func (s *Splitter) scan() {
	for {
		nl := indexByteFrom(s.buf, s.scanned, '\n')
		if nl < 0 {
			s.scanned = len(s.buf)
			return
		}
		line := string(s.buf[s.lineStart:nl])
		after := nl + 1
		s.scanned = after
		s.lineStart = after

		switch {
		case s.inFence:
			if closesFence(line, s.fence, s.fenceLen) {
				s.inFence = false
				// The line after a closed fence is a safe boundary: the block
				// is structurally complete.
				s.boundary = after
			}
		case isBlank(line):
			// A blank line outside any fence ends the enclosing block.
			s.boundary = after
		default:
			if marker, count, isOpen := opensFence(line); isOpen {
				s.inFence = true
				s.fence = marker
				s.fenceLen = count
			}
		}
	}
}

// indexByteFrom is bytes.IndexByte from an offset, without reslicing noise.
func indexByteFrom(b []byte, from int, c byte) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// isBlank reports whether the line holds nothing but spaces and tabs.
func isBlank(line string) bool {
	return strings.TrimRight(line, " \t\r") == ""
}

// opensFence reports whether the line opens a code fence: three or more
// backticks or tildes after at most three spaces of indentation.
func opensFence(line string) (marker byte, count int, isOpen bool) {
	trimmed, indented := trimFenceIndent(line)
	if !indented || trimmed == "" {
		return 0, 0, false
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0, false
	}
	n := runLen(trimmed, c)
	if n < 3 {
		return 0, 0, false
	}
	// A backtick fence's info string may not contain backticks (that is an
	// inline code span, not a fence).
	if c == '`' && strings.Contains(trimmed[n:], "`") {
		return 0, 0, false
	}
	return c, n, true
}

// closesFence reports whether the line closes a fence opened with the given
// marker and length: at least as many markers, nothing after them.
func closesFence(line string, marker byte, openLen int) bool {
	trimmed, indented := trimFenceIndent(line)
	if !indented || trimmed == "" || trimmed[0] != marker {
		return false
	}
	n := runLen(trimmed, marker)
	if n < openLen {
		return false
	}
	return strings.TrimRight(trimmed[n:], " \t\r") == ""
}

// trimFenceIndent strips up to three leading spaces; more indentation makes
// the line an indented code line, not a fence.
func trimFenceIndent(line string) (string, bool) {
	for i := 0; i < 4 && i < len(line); i++ {
		if line[i] != ' ' {
			return line[i:], true
		}
	}
	if len(line) < 4 {
		return "", true
	}
	return "", false
}

// runLen counts the leading run of c in s.
func runLen(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}
