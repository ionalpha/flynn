// Package watch turns in-tree source comments into governed run inputs. It tails a
// working tree for aider-style ai! and ai? markers left in trailing comments; when
// one appears it becomes a turn the agent runs, and the marker's file and line are
// carried onto the run so its provenance lands in the sealed record like any other
// input. The marker is cleared once picked up so the same request never fires twice.
package watch

import (
	"strconv"
	"strings"
)

// Kind is a marker's intent. Act (ai!) asks the agent to change the code; Ask (ai?)
// asks it to answer about the code. Both become a turn input; the kind is carried so
// the composed objective can tell the run to edit versus explain.
type Kind string

const (
	// Act is the ai! marker: do the work described in the comment.
	Act Kind = "ai!"
	// Ask is the ai? marker: answer the question the comment poses.
	Ask Kind = "ai?"
)

// Marker is one ai! / ai? request found in a source file: the intent, the file and
// 1-based line it sits on, the instruction text after the marker token, and the
// code on that line with the marker comment removed (the local context the request
// is about). Provenance renders File and Line for the recorded objective.
type Marker struct {
	Kind Kind
	File string
	Line int
	Text string
	Code string
}

// commentLeaders are the comment openers a marker can trail, longest first so a
// leader that is a prefix of another (// before /) is matched at its full length.
// The token immediately after the leader must be ai! or ai?, which keeps a bare
// "ai!" appearing mid-code from ever reading as a marker.
var commentLeaders = []string{"<!--", "//", "/*", "--", "#", ";"}

// closers strip a marker's trailing block-comment terminator so the instruction
// does not carry "*/" or "-->" into the objective.
var closers = []string{"-->", "*/"}

// ScanLine reports the marker on a single line, if any. It finds the earliest
// comment leader whose following token is ai! or ai?, takes the rest of the comment
// as the instruction (trailing block terminators removed), and returns the code
// before the comment as context. ok is false when the line holds no marker or the
// instruction is empty (an empty marker has nothing to run).
func ScanLine(line string) (kind Kind, instruction, code string, ok bool) {
	best := -1
	for _, leader := range commentLeaders {
		idx := strings.Index(line, leader)
		if idx < 0 || (best >= 0 && idx >= best) {
			continue
		}
		rest := strings.TrimLeft(line[idx+len(leader):], " \t")
		k, ok := markerToken(rest)
		if !ok {
			continue
		}
		instr := strings.TrimSpace(stripClosers(rest[len(k):]))
		if instr == "" {
			continue
		}
		best = idx
		kind, instruction, code = k, instr, strings.TrimRight(line[:idx], " \t")
	}
	return kind, instruction, code, best >= 0
}

// markerToken reports whether s opens with an ai! or ai? token that is not glued to
// a longer word (so "ai!!" or "airplane" never match), returning the token's Kind.
func markerToken(s string) (Kind, bool) {
	for _, k := range []Kind{Act, Ask} {
		if strings.HasPrefix(s, string(k)) {
			after := s[len(k):]
			if after == "" || after[0] == ' ' || after[0] == '\t' {
				return k, true
			}
		}
	}
	return "", false
}

// stripClosers removes a single trailing block-comment terminator from s.
func stripClosers(s string) string {
	trimmed := strings.TrimRight(s, " \t")
	for _, c := range closers {
		if strings.HasSuffix(trimmed, c) {
			return trimmed[:len(trimmed)-len(c)]
		}
	}
	return s
}

// Scan finds every marker in a file's content, tagging each with the file path and
// its 1-based line. Lines without a marker are skipped.
func Scan(file string, content []byte) []Marker {
	var markers []Marker
	for i, line := range strings.Split(string(content), "\n") {
		kind, instr, code, ok := ScanLine(line)
		if !ok {
			continue
		}
		markers = append(markers, Marker{Kind: kind, File: file, Line: i + 1, Text: instr, Code: code})
	}
	return markers
}

// Provenance renders where a marker came from for the recorded objective, e.g.
// "cmd/flynn/run.go:42 (ai!)". It is stable so a run's inputs are diffable.
func (m Marker) Provenance() string {
	return m.File + ":" + strconv.Itoa(m.Line) + " (" + string(m.Kind) + ")"
}
