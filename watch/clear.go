package watch

import "strings"

// Clear removes the marker on the given 1-based line from content and reports
// whether it changed anything. The marker's comment is stripped from its line; if
// nothing but whitespace remains the line is dropped entirely, otherwise the code
// before the comment is kept. A line that no longer holds a marker (already cleared,
// edited out) leaves content untouched and returns false, so a lost race clears
// nothing rather than corrupting the file. The file's newline style is preserved:
// a trailing carriage return on the target line survives when the code is kept.
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
