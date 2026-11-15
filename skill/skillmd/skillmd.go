// Package skillmd reads and writes SKILL.md, the file of the Agent Skills format
// (https://agentskills.io/specification). It handles the file only; the skill
// directory and its scripts/, references/ and assets/ belong to the loader.
//
// # Strictness
//
// [Parse] is tolerant and [Validate] is strict. About forty-five clients write
// SKILL.md with no shared validator, and the specification does not say what an
// unknown frontmatter key means, so Parse fails only on input it cannot represent
// and keeps unknown keys in Doc.Unknown. Validate applies the specification before
// anything is published. Import calls Parse; export calls Validate first.
//
// # Grammar
//
// The frontmatter is six keys: five scalars and one flat string map. A YAML library
// would bring anchors, aliases, merge keys, type coercion and recursive expansion to
// files fetched from public registries. This grammar reads plain and quoted scalars,
// block scalars, comments and one level under metadata, and refuses the rest.
//
// A malformed document is an error, never an empty skill, because an empty skill
// keeps its name in a menu after its instructions are gone.
package skillmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Limits in runes from the specification, plus MaxDocSize in bytes. The format sets
// no size ceiling, so without MaxDocSize a hostile file would decide memory use.
const (
	MaxNameLen          = 64
	MaxDescriptionLen   = 1024
	MaxCompatibilityLen = 500
	MaxDocSize          = 1 << 20
)

// Sentinel errors. A loader skips a file with [ErrNoFrontmatter] and rejects a pack
// whose SKILL.md breaks a rule.
var (
	// ErrNoFrontmatter reports input without a --- line at byte 0.
	ErrNoFrontmatter = errors.New("skillmd: no frontmatter")
	// ErrUnterminated reports a frontmatter block that is never closed.
	ErrUnterminated = errors.New("skillmd: unterminated frontmatter")
	// ErrTooLarge reports input above MaxDocSize.
	ErrTooLarge = errors.New("skillmd: document too large")
	// ErrSyntax reports frontmatter this package's grammar cannot represent.
	ErrSyntax = errors.New("skillmd: frontmatter syntax")
	// ErrInvalid reports a document that parses but breaks a specification rule.
	ErrInvalid = errors.New("skillmd: invalid document")
)

// Doc is one parsed SKILL.md. Undefined top-level keys are kept in Unknown, because
// the ecosystem disagrees on whether they are allowed (anthropics/skills#249), and
// keeping them makes a round trip lossless; [Validate] refuses them on publish.
type Doc struct {
	// Name must match the directory name. Required.
	Name string
	// Description is the only text loaded at discovery, so activation keys on it.
	// Required.
	Description string
	License     string
	// Compatibility states runtime or product requirements.
	Compatibility string
	// AllowedTools is allowed-tools split on spaces. It is a claim and never a
	// grant: honouring it from a foreign pack would let a downloaded file widen its
	// own permissions.
	AllowedTools []string
	// Metadata is the specification's extension map; structured values must encode
	// themselves as strings.
	Metadata     map[string]string
	Unknown      map[string]string
	unknownOrder []string // so [Format] keeps a foreign file's key order
	// Body is the markdown after the closing ---, without the newline that ends it.
	Body string
}

// Parse reads a SKILL.md. It fails only on input it cannot represent; call [Validate]
// for the specification's rules.
func Parse(src []byte) (Doc, error) {
	if len(src) > MaxDocSize {
		return Doc{}, fmt.Errorf("%w: %d bytes exceeds %d", ErrTooLarge, len(src), MaxDocSize)
	}
	text := string(src)
	// Windows editors often write a byte order mark.
	text = strings.TrimPrefix(text, "\ufeff")

	rest, ok := trimOpener(text)
	if !ok {
		return Doc{}, ErrNoFrontmatter
	}
	front, body, ok := splitAtCloser(rest)
	if !ok {
		return Doc{}, ErrUnterminated
	}
	doc, err := parseFrontmatter(front)
	if err != nil {
		return Doc{}, err
	}
	doc.Body = body
	return doc, nil
}

// trimOpener does not skip leading blank lines, because the opener must be at byte 0.
func trimOpener(text string) (string, bool) {
	for _, opener := range []string{"---\r\n", "---\n"} {
		if strings.HasPrefix(text, opener) {
			return text[len(opener):], true
		}
	}
	// A bare "---" is opened, so Parse reports [ErrUnterminated].
	if strings.TrimRight(text, "\r\n") == "---" {
		return "", true
	}
	return "", false
}

// splitAtCloser splits at the first line that is exactly ---.
func splitAtCloser(text string) (front, body string, ok bool) {
	offset := 0
	for offset <= len(text) {
		line, next := nextLine(text, offset)
		if strings.TrimRight(line, "\r") == "---" {
			return text[:offset], text[min(next, len(text)):], true
		}
		if next == offset {
			break
		}
		offset = next
	}
	return "", "", false
}

func nextLine(text string, offset int) (line string, next int) {
	if offset >= len(text) {
		return "", offset
	}
	end := strings.IndexByte(text[offset:], '\n')
	if end < 0 {
		return text[offset:], len(text)
	}
	return text[offset : offset+end], offset + end + 1
}

// parseFrontmatter reads one key: value per line at indent zero, with metadata
// opening one indented block of the same shape.
func parseFrontmatter(front string) (Doc, error) {
	doc := Doc{}
	lines := splitLines(front)

	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		if isSkippable(raw) {
			continue
		}
		if indentOf(raw) > 0 {
			return Doc{}, fmt.Errorf("%w: unexpected indented line %q", ErrSyntax, strings.TrimSpace(raw))
		}
		key, value, err := splitKeyValue(raw)
		if err != nil {
			return Doc{}, err
		}

		if key == "metadata" && strings.TrimSpace(value) == "" {
			meta, consumed, err := parseNestedMap(lines[i+1:])
			if err != nil {
				return Doc{}, err
			}
			doc.Metadata = meta
			i += consumed
			continue
		}

		scalar, consumed, err := readScalar(value, lines[i+1:])
		if err != nil {
			return Doc{}, fmt.Errorf("%w (key %q)", err, key)
		}
		i += consumed

		if err := doc.assign(key, scalar); err != nil {
			return Doc{}, err
		}
	}
	return doc, nil
}

func (d *Doc) assign(key, value string) error {
	switch key {
	case "name":
		d.Name = value
	case "description":
		d.Description = value
	case "license":
		d.License = value
	case "compatibility":
		d.Compatibility = value
	case "allowed-tools":
		d.AllowedTools = strings.Fields(value)
	case "metadata":
		// An inline metadata value is a flow mapping, which this grammar refuses.
		return fmt.Errorf("%w: metadata must be an indented block, not an inline value", ErrSyntax)
	default:
		if d.Unknown == nil {
			d.Unknown = map[string]string{}
		}
		if _, seen := d.Unknown[key]; !seen {
			d.unknownOrder = append(d.unknownOrder, key)
		}
		d.Unknown[key] = value
	}
	return nil
}

// parseNestedMap returns the metadata block and the lines it consumed. Every entry
// must share one indent, so a second nesting level is an error.
func parseNestedMap(lines []string) (map[string]string, int, error) {
	out := map[string]string{}
	indent := -1
	consumed := 0

	for _, raw := range lines {
		if isSkippable(raw) {
			consumed++
			continue
		}
		got := indentOf(raw)
		if got == 0 {
			break
		}
		if indent < 0 {
			indent = got
		}
		if got != indent {
			return nil, 0, fmt.Errorf("%w: inconsistent indent in metadata block at %q", ErrSyntax, strings.TrimSpace(raw))
		}
		key, value, err := splitKeyValue(raw)
		if err != nil {
			return nil, 0, err
		}
		scalar, err := unquote(strings.TrimSpace(value))
		if err != nil {
			return nil, 0, err
		}
		out[key] = scalar
		consumed++
	}
	return out, consumed, nil
}

func readScalar(value string, following []string) (string, int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "|" || trimmed == ">" || trimmed == "|-" || trimmed == ">-" ||
		trimmed == "|+" || trimmed == ">+" {
		return readBlockScalar(trimmed, following)
	}
	scalar, err := unquote(trimmed)
	return scalar, 0, err
}

// readBlockScalar reads a | or > block. Chomping: - strips the trailing newline, +
// keeps every one, and the default keeps one.
func readBlockScalar(introducer string, following []string) (string, int, error) {
	style := introducer[0]
	chomp := byte(0)
	if len(introducer) > 1 {
		chomp = introducer[1]
	}

	var block []string
	indent := -1
	consumed := 0
	for _, raw := range following {
		if strings.TrimSpace(raw) == "" {
			block = append(block, "")
			consumed++
			continue
		}
		got := indentOf(raw)
		if got == 0 {
			break
		}
		if indent < 0 {
			indent = got
		}
		if got < indent {
			break
		}
		block = append(block, raw[indent:])
		consumed++
	}
	if indent < 0 {
		return "", consumed, fmt.Errorf("%w: block scalar has no content", ErrSyntax)
	}

	// Chomping decides trailing blank lines.
	trailing := 0
	for len(block) > 0 && block[len(block)-1] == "" {
		block = block[:len(block)-1]
		trailing++
	}

	var text string
	if style == '|' {
		text = strings.Join(block, "\n")
	} else {
		text = foldLines(block)
	}

	switch chomp {
	case '-': // strip keeps no trailing newline
	case '+':
		text += strings.Repeat("\n", trailing+1)
	default:
		if text != "" {
			text += "\n"
		}
	}
	return text, consumed, nil
}

// foldLines joins lines with a space and keeps a blank line as a newline.
func foldLines(block []string) string {
	var b strings.Builder
	for i, line := range block {
		switch {
		case i == 0:
			b.WriteString(line)
		case line == "":
			b.WriteString("\n")
		case block[i-1] == "":
			b.WriteString(line)
		default:
			b.WriteString(" ")
			b.WriteString(line)
		}
	}
	return b.String()
}

// splitKeyValue splits at the first colon; a line without one is an error.
func splitKeyValue(raw string) (key, value string, err error) {
	line := strings.TrimRight(raw, "\r")
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", "", fmt.Errorf("%w: expected key: value, got %q", ErrSyntax, strings.TrimSpace(line))
	}
	key = strings.TrimSpace(line[:idx])
	if key == "" {
		return "", "", fmt.Errorf("%w: empty key in %q", ErrSyntax, strings.TrimSpace(line))
	}
	return key, line[idx+1:], nil
}

// unquote refuses anchors, aliases, tags and flow collections, so unsupported YAML
// is an error and never a wrong value.
func unquote(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	switch s[0] {
	case '\'':
		return unquoteSingle(s)
	case '"':
		return unquoteDouble(s)
	case '&', '*', '!':
		return "", fmt.Errorf("%w: anchors, aliases and tags are not supported (%q)", ErrSyntax, s)
	case '[', '{':
		return "", fmt.Errorf("%w: flow collections are not supported (%q)", ErrSyntax, s)
	}
	return s, nil
}

// unquoteSingle resolves a single-quoted scalar, in which ” is a literal quote and
// nothing else escapes.
func unquoteSingle(s string) (string, error) {
	if len(s) < 2 || !strings.HasSuffix(s, "'") {
		return "", fmt.Errorf("%w: unterminated single-quoted scalar %q", ErrSyntax, s)
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] != '\'' {
			b.WriteByte(inner[i])
			continue
		}
		if i+1 < len(inner) && inner[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return "", fmt.Errorf("%w: stray quote in single-quoted scalar %q", ErrSyntax, s)
	}
	return b.String(), nil
}

// unquoteDouble supports \n, \t, \r, \" and \\. Any other escape is an error, so a
// value never parses into something other than what it says.
func unquoteDouble(s string) (string, error) {
	if len(s) < 2 || !strings.HasSuffix(s, `"`) {
		return "", fmt.Errorf("%w: unterminated double-quoted scalar %q", ErrSyntax, s)
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] != '\\' {
			b.WriteByte(inner[i])
			continue
		}
		i++
		if i >= len(inner) {
			return "", fmt.Errorf("%w: trailing escape in %q", ErrSyntax, s)
		}
		switch inner[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			return "", fmt.Errorf("%w: unsupported escape %q in %q", ErrSyntax, string(inner[i]), s)
		}
	}
	return b.String(), nil
}

func splitLines(front string) []string {
	if front == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(front, "\n"), "\n")
}

// isSkippable reports a blank line or a full-line comment. A trailing " #" stays in
// the value, because guessing where a comment starts would truncate a description.
func isSkippable(raw string) bool {
	trimmed := strings.TrimSpace(strings.TrimRight(raw, "\r"))
	return trimmed == "" || strings.HasPrefix(trimmed, "#")
}

// indentOf counts leading spaces only; YAML does not indent with tabs.
func indentOf(raw string) int {
	return len(raw) - len(strings.TrimLeft(raw, " "))
}

// Validate applies the specification to a document before it is published; [Parse]
// does not. Name must equal dirName unless dirName is empty, as for a document with
// no directory yet.
func Validate(d Doc, dirName string) error {
	if err := ValidateName(d.Name); err != nil {
		return err
	}
	if dirName != "" && d.Name != dirName {
		return fmt.Errorf("%w: name %q must match directory %q", ErrInvalid, d.Name, dirName)
	}
	if n := utf8.RuneCountInString(d.Description); n == 0 || n > MaxDescriptionLen {
		return fmt.Errorf("%w: description is %d characters, want 1 to %d", ErrInvalid, n, MaxDescriptionLen)
	}
	if n := utf8.RuneCountInString(d.Compatibility); n > MaxCompatibilityLen {
		return fmt.Errorf("%w: compatibility is %d characters, want at most %d", ErrInvalid, n, MaxCompatibilityLen)
	}
	for key := range d.Metadata {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%w: metadata has an empty key", ErrInvalid)
		}
	}
	if len(d.Unknown) > 0 {
		return fmt.Errorf("%w: undefined frontmatter keys %v, put extensions in metadata", ErrInvalid, sortedKeys(d.Unknown))
	}
	return nil
}

// ValidateName checks a name is 1 to [MaxNameLen] lowercase letters, digits and
// hyphens, with no hyphen at either end or twice in a row. Each rule has its own
// error so an author learns which one broke.
func ValidateName(name string) error {
	n := utf8.RuneCountInString(name)
	if n == 0 || n > MaxNameLen {
		return fmt.Errorf("%w: name is %d characters, want 1 to %d", ErrInvalid, n, MaxNameLen)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("%w: name %q must not start or end with a hyphen", ErrInvalid, name)
	}
	if strings.Contains(name, "--") {
		return fmt.Errorf("%w: name %q must not contain consecutive hyphens", ErrInvalid, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("%w: name %q may hold only lowercase letters, digits and hyphens", ErrInvalid, name)
		}
	}
	return nil
}

// Format renders d as SKILL.md. Anything [Parse] accepted round-trips, unknown keys
// and their order included, so re-exporting a foreign pack leaves it unchanged.
func Format(d Doc) ([]byte, error) {
	var b strings.Builder
	b.WriteString("---\n")
	writeField(&b, "name", d.Name)
	writeField(&b, "description", d.Description)
	writeField(&b, "license", d.License)
	writeField(&b, "compatibility", d.Compatibility)
	if len(d.AllowedTools) > 0 {
		writeField(&b, "allowed-tools", strings.Join(d.AllowedTools, " "))
	}
	for _, key := range d.unknownKeys() {
		// Written even when empty, because dropping a foreign key loses data.
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(quoteInline(d.Unknown[key]))
		b.WriteString("\n")
	}
	if len(d.Metadata) > 0 {
		b.WriteString("metadata:\n")
		for _, key := range sortedKeys(d.Metadata) {
			b.WriteString("  ")
			b.WriteString(key)
			b.WriteString(": ")
			b.WriteString(quoteInline(d.Metadata[key]))
			b.WriteString("\n")
		}
	}
	b.WriteString("---\n")
	b.WriteString(d.Body)
	return []byte(b.String()), nil
}

// unknownKeys falls back to sorted order for a document built in memory.
func (d Doc) unknownKeys() []string {
	if len(d.unknownOrder) == len(d.Unknown) {
		return d.unknownOrder
	}
	return sortedKeys(d.Unknown)
}

// writeField skips an empty value.
func writeField(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(quote(value))
	b.WriteString("\n")
}

// quote writes a multi-line value as a block scalar for readers, and double-quotes
// it when a block would not round-trip.
func quote(s string) string {
	if strings.Contains(s, "\n") {
		if !blockSafe(s) {
			return quoteInline(s)
		}
		var b strings.Builder
		b.WriteString("|-\n")
		for _, line := range strings.Split(s, "\n") {
			if line == "" {
				b.WriteString("\n")
				continue
			}
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteString("\n")
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	return quoteInline(s)
}

// blockSafe reports whether a value survives a |- block. An indented line would set
// the block indent too high and truncate the value, and strip chomping cannot carry
// a leading or trailing newline.
func blockSafe(s string) bool {
	if strings.HasSuffix(s, "\n") || strings.HasPrefix(s, "\n") {
		return false
	}
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return false
		}
	}
	return true
}

// quoteInline writes one line, the only shape metadata values take, because
// parseNestedMap reads one line per key.
func quoteInline(s string) string {
	if needsQuoting(s) {
		return `"` + escapeDouble(s) + `"`
	}
	return s
}

// needsQuoting reports whether a plain scalar would read back differently: refused,
// taken for a comment or key, or trimmed.
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	if s != strings.TrimSpace(s) {
		return true
	}
	if strings.ContainsAny(s, "\n\r\t") {
		return true
	}
	switch s[0] {
	case '\'', '"', '&', '*', '!', '[', '{', '#', '-', '?', '|', '>', '%', '@', '`':
		return true
	}
	return strings.Contains(s, ": ") || strings.HasSuffix(s, ":")
}

// escapeDouble covers exactly the escapes unquoteDouble reads.
func escapeDouble(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\t", `\t`,
		"\r", `\r`,
	)
	return r.Replace(s)
}

// sortedKeys keeps rendering deterministic.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
