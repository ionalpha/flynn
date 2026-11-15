package watch

import "strings"

// Clear removes the marker comment on the 1-based line, dropping the line when
// only whitespace remains and keeping a trailing carriage return. A line that no
// longer holds a marker is left alone and Clear returns false, so a lost race
// changes nothing.
func Clear(content []byte, line int) ([]byte, bool) {
	if line < 1 {
		return content, false
	}
	lines := strings.Split(string(content), "\n")
	if line > len(lines) {
		return content, false
	}
	raw := lines[line-1]
	cr := strings.HasSuffix(raw, "\r")
	body := strings.TrimSuffix(raw, "\r")
	_, _, code, ok := ScanLine(body)
	if !ok {
		return content, false
	}
	if strings.TrimSpace(code) == "" {
		lines = append(lines[:line-1], lines[line:]...)
	} else if cr {
		lines[line-1] = code + "\r"
	} else {
		lines[line-1] = code
	}
	return []byte(strings.Join(lines, "\n")), true
}
