// Package mdstream partitions streamed markdown into a stable prefix and a
// mutable tail. Text committed to scrollback is rendered once and never touched
// again, but markdown cannot be rendered correctly until its block is complete (a
// table cannot be laid out from half its rows; an open fence has no known end).
// So the commit boundary advances only at safe points: the end of a line that
// closes a code fence, or a blank line outside any fence. Everything after the
// last one is tail and is re-rendered each frame. A streaming table has no blank
// line inside it, so it stays in the tail until complete.
//
// The splitter never inspects render output and allocates nothing per write
// beyond the buffer append, so it is cheap on every streamed chunk.
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

// Stable returns the text committed since the last call, safe to render and
// print exactly once.
func (s *Splitter) Stable() string {
	if s.boundary <= s.committed {
		return ""
	}
	out := string(s.buf[s.committed:s.boundary])
	s.committed = s.boundary
	return out
}

// Tail returns the mutable remainder after the commit boundary.
func (s *Splitter) Tail() string {
	return string(s.buf[s.boundary:])
}

// Close marks the stream complete and returns the uncommitted text; at the end
// every block is as complete as it will get, so the whole tail is stable.
func (s *Splitter) Close() string {
	out := string(s.buf[s.committed:])
	s.committed = len(s.buf)
	s.boundary = len(s.buf)
	return out
}

// scan updates fence state and safe boundaries line by line. Only complete lines
// are scanned: one missing its newline can still change meaning (two backticks
// may become a fence), so the boundary never lands inside it.
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
				s.boundary = after
			}
		case isBlank(line):
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

func indexByteFrom(b []byte, from int, c byte) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func isBlank(line string) bool {
	return strings.TrimRight(line, " \t\r") == ""
}

// opensFence reports whether the line opens a fence: three or more backticks or
// tildes after at most three spaces.
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
	// A backtick fence's info string cannot contain backticks; that is an inline
	// code span.
	if c == '`' && strings.Contains(trimmed[n:], "`") {
		return 0, 0, false
	}
	return c, n, true
}

// closesFence reports whether the line closes the open fence: at least as many
// markers, nothing after them.
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

// trimFenceIndent strips up to three leading spaces; more makes an indented code
// line, not a fence.
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

func runLen(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}
