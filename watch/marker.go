// Package watch turns source comments that start with ai! (change the code) or ai?
// (answer a question) into agent turns. These are the markers aider's watch mode
// uses. The marker's file and line go onto the run's sealed record, and the marker
// is cleared once picked up so it never fires twice.
package watch

import (
	"strconv"
	"strings"
)

// Kind is a marker's intent, carried so the objective can ask for an edit or an
// explanation.
type Kind string

const (
	// Act is the ai! marker: do the work described in the comment.
	Act Kind = "ai!"
	// Ask is the ai? marker: answer the question the comment poses.
	Ask Kind = "ai?"
)

// Marker is one request found in a source file.
type Marker struct {
	Kind Kind
	File string // root-relative, slash-separated
	Line int    // 1-based
	Text string // the instruction after the marker token
	Code string // the code on the line, marker comment removed
}

// commentLeaders are the comment openers a marker can follow. Requiring the marker
// right after a leader keeps an "ai!" in code from reading as a marker.
var commentLeaders = []string{"<!--", "//", "/*", "--", "#", ";"}

var closers = []string{"-->", "*/"}

// ScanLine reports the marker on one line. The earliest leader wins. ok is false
// when there is no marker or its instruction is empty.
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

// markerToken requires whitespace or end of string after the token, so "ai!!"
// does not match.
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

// stripClosers removes one trailing "*/" or "-->" so it stays out of the
// objective.
func stripClosers(s string) string {
	trimmed := strings.TrimRight(s, " \t")
	for _, c := range closers {
		if strings.HasSuffix(trimmed, c) {
			return trimmed[:len(trimmed)-len(c)]
		}
	}
	return s
}

// Scan returns every marker in content, tagged with file.
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

// Provenance renders the marker as "cmd/flynn/run.go:42 (ai!)". The format is
// stable so a run's inputs can be diffed.
func (m Marker) Provenance() string {
	return m.File + ":" + strconv.Itoa(m.Line) + " (" + string(m.Kind) + ")"
}
