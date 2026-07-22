// Package skillmd reads and writes SKILL.md, the file at the center of the Agent
// Skills format (https://agentskills.io/specification). A skill is a directory
// holding a SKILL.md plus optional scripts/, references/ and assets/ siblings; this
// package owns the file, not the directory, so it stays a pure codec that a loader
// can point at any bytes it has already decided to trust enough to read.
//
// # Two levels of strictness, on purpose
//
// Parse is tolerant and Validate is strict, and the split is the whole design.
// Import has to swallow the existing ecosystem, where roughly forty-five clients
// write SKILL.md files with no shared validator and the specification is silent on
// what an unrecognized frontmatter key means. Export has to emit something a
// conformant reader cannot reject. So Parse fails only on input it genuinely cannot
// represent, keeping unknown keys in Doc.Unknown rather than discarding or refusing
// them, and Validate applies the specification's own constraints to a document we
// are about to publish. A caller that reads a third-party pack calls Parse; a caller
// that writes one calls Validate first.
//
// # Why this does not use a YAML library
//
// The specification's frontmatter is a closed set of six keys: five scalars and one
// flat map of string to string. Reading that does not need a general YAML engine,
// and pulling one in would hand a parser with anchors, aliases, merge keys,
// implicit type coercion and recursive expansion the job of reading files fetched
// from public registries. The grammar here covers what the format actually uses,
// plain and quoted scalars, block scalars, comments and one level of nesting under
// metadata, and refuses the rest by construction. Less to get wrong, and it fuzzes.
//
// Everything this package rejects, it rejects loudly. A malformed document is never
// silently reported as an empty skill, because a silently empty skill is a skill
// whose instructions vanished while its name still shows up in a menu.
package skillmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Limits the specification states, in runes, plus one this package adds.
//
// MaxNameLen, MaxDescriptionLen and MaxCompatibilityLen are the specification's.
// MaxDocSize is ours: the format sets no ceiling, and a loader reading an untrusted
// tree needs one, because "no limit" against a hostile pack means the first
// pathological file decides how much memory the process uses.
const (
	MaxNameLen          = 64
	MaxDescriptionLen   = 1024
	MaxCompatibilityLen = 500
	MaxDocSize          = 1 << 20
)

// Sentinel errors. Callers distinguish "this is not a SKILL.md at all" from "this is
// a SKILL.md that breaks a rule", because a loader walking a directory tree treats
// those differently: the first is a file to skip, the second is a pack to reject.
var (
	// ErrNoFrontmatter reports input that does not open with a frontmatter block.
	// The specification requires the document to begin at byte 0 with a --- line.
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

// Doc is one parsed SKILL.md: the frontmatter fields the specification defines, the
// markdown body, and whatever else the frontmatter carried.
//
// Unknown holds top-level frontmatter keys the specification does not define, in the
// order they appeared. The specification does not say whether a reader may accept
// them, and the ecosystem disagrees with itself (anthropics/skills#249: the official
// skill-creator tells authors "Do not include any other fields in YAML frontmatter",
// which contradicts the specification's own optional set). Keeping them costs
// nothing, makes a round trip lossless for files we did not write, and leaves the
// judgment to Validate, which is the only place that decides what we are willing to
// publish.
type Doc struct {
	// Name is the skill's identifier and must match its directory name. Required.
	Name string
	// Description states what the skill does and when to use it. Required. It is
	// the only body text loaded at discovery, so it is what activation keys on.
	Description string
	// License names a license or points at a bundled license file. Optional.
	License string
	// Compatibility states environment or product requirements. Optional. This is
	// where a skill that needs a particular runtime says so, rather than inventing
	// a key for it.
	Compatibility string
	// AllowedTools is the specification's allowed-tools field, split on spaces.
	//
	// Treat it as a claim, never as a grant. It is a skill declaring which tools it
	// would like pre-approved, it is marked experimental, and the specification says
	// nothing about trust, so honoring it from a pack we did not author would let a
	// downloaded text file widen its own permissions.
	AllowedTools []string
	// Metadata is the specification's sanctioned extension point, "properties not
	// defined by the Agent Skills spec". Values are strings, so a structured
	// extension has to encode itself.
	Metadata map[string]string
	// Unknown holds undefined top-level keys, preserved for lossless round trips.
	Unknown map[string]string
	// unknownOrder keeps Unknown's original key order so Format round-trips a
	// foreign document byte for byte instead of reordering someone else's file.
	unknownOrder []string
	// Body is the markdown after the frontmatter, with the single newline that
	// separates it from the closing --- removed.
	Body string
}

// Parse reads a SKILL.md into a Doc. It is deliberately tolerant: it fails on input
// it cannot represent, not on input that breaks a specification rule, so a caller
// importing a third-party pack gets the document and can decide what to do about it.
// Call Validate for the specification's constraints.
func Parse(src []byte) (Doc, error) {
	if len(src) > MaxDocSize {
		return Doc{}, fmt.Errorf("%w: %d bytes exceeds %d", ErrTooLarge, len(src), MaxDocSize)
	}
	text := string(src)
	// A byte order mark before the opening --- is common enough from Windows
	// editors that refusing it would reject files whose author did nothing wrong.
	// Spelled as an escape so the source stays readable and greppable.
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

// trimOpener consumes the leading --- line, reporting whether it was there. The
// specification requires it at byte 0, so leading blank lines are not skipped.
func trimOpener(text string) (string, bool) {
	for _, opener := range []string{"---\r\n", "---\n"} {
		if strings.HasPrefix(text, opener) {
			return text[len(opener):], true
		}
	}
	// A document that is nothing but "---" has an opener and no closer, which is
	// unterminated rather than absent. Report it as opened so the caller gets the
	// more specific error.
	if strings.TrimRight(text, "\r\n") == "---" {
		return "", true
	}
	return "", false
}

// splitAtCloser divides the text after the opener into the frontmatter block and the
// body at the first line that is exactly ---, reporting whether that line exists.
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

// nextLine returns the line beginning at offset and the offset of the line after it.
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

// parseFrontmatter reads the block between the --- lines. The grammar is one
// key: value pair per line at indent zero, with metadata optionally opening an
// indented block of the same shape, plus block scalars and full-line comments.
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

// assign places a parsed scalar on the field its key names, or records it as unknown.
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
		// metadata with an inline value is a flow mapping or a mistake. This
		// grammar does not read flow collections, and guessing at one would be a
		// worse failure than saying so.
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

// parseNestedMap reads the indented block under metadata, returning it and how many
// lines it consumed. Every entry in the block must share one indent, so a document
// cannot smuggle a second nesting level past a reader that has no way to represent it.
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

// readScalar resolves a value that may be inline or a block scalar introduced by
// | or >, returning it and how many following lines it consumed.
func readScalar(value string, following []string) (string, int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "|" || trimmed == ">" || trimmed == "|-" || trimmed == ">-" ||
		trimmed == "|+" || trimmed == ">+" {
		return readBlockScalar(trimmed, following)
	}
	scalar, err := unquote(trimmed)
	return scalar, 0, err
}

// readBlockScalar reads an indented block under a | or > introducer. A literal block
// keeps its newlines, a folded block joins its lines with spaces, and the chomping
// indicator decides the trailing newline: - strips it, + keeps every one, and the
// default clips to a single newline.
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

	// Trailing blank lines belong to the chomping decision, not to the content.
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
	case '-':
		// strip: no trailing newline at all
	case '+':
		text += strings.Repeat("\n", trailing+1)
	default:
		if text != "" {
			text += "\n"
		}
	}
	return text, consumed, nil
}

// foldLines joins a folded block scalar's lines. A blank line is a paragraph break
// and survives as a newline; everything else joins with a single space.
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

// splitKeyValue divides a key: value line. The key is everything before the first
// colon; a line with no colon is not a mapping entry and this grammar has nothing
// else it could be.
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

// unquote resolves a plain, single-quoted or double-quoted scalar. Anchors, aliases,
// explicit tags and flow collections are refused rather than guessed at: this
// grammar covers what the format uses, and pretending to understand the rest would
// turn a parse error into a wrong value.
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

// unquoteDouble resolves a double-quoted scalar with the escapes the format
// realistically uses. An unrecognized escape is an error, not a passthrough, so a
// document never parses into something subtly different from what it says.
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

// splitLines splits a frontmatter block into lines without keeping the newlines.
func splitLines(front string) []string {
	if front == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(front, "\n"), "\n")
}

// isSkippable reports whether a frontmatter line carries no mapping entry: a blank
// line, or a full-line comment.
//
// Only full-line comments count. A trailing " # ..." stays part of the value,
// because guessing where a comment starts inside an unquoted scalar is how a
// description silently loses its second half.
func isSkippable(raw string) bool {
	trimmed := strings.TrimSpace(strings.TrimRight(raw, "\r"))
	return trimmed == "" || strings.HasPrefix(trimmed, "#")
}

// indentOf counts the leading spaces on a line. Tabs are not indentation in YAML and
// are not treated as such here.
func indentOf(raw string) int {
	for i := range len(raw) {
		if raw[i] != ' ' {
			return i
		}
	}
	return len(raw)
}

// Validate reports whether a document satisfies the specification. It is the gate for
// anything we publish, and it is deliberately not applied on read.
//
// dirName is the name of the directory holding the SKILL.md; the specification
// requires the name field to match it. Pass an empty string to skip that check when
// the document has no directory yet, which is the case for one being authored in
// memory.
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

// ValidateName applies the specification's rule for the name field: 1 to 64
// characters, lowercase alphanumeric and hyphens only, no leading or trailing
// hyphen, and no consecutive hyphens.
//
// Written out rather than expressed as a regular expression so each rejection can
// say which rule it broke. A loader reporting "invalid name" against a pattern
// leaves an author guessing; naming the rule does not.
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

// Format renders a document back to SKILL.md bytes. Round trips are lossless for
// anything Parse accepted, including the unknown keys and their original order, so
// importing and re-exporting a foreign pack does not quietly rewrite it.
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
		// Unlike a defined field, an undefined one is written even when its value is
		// empty. Omitting a blank optional field of ours keeps a rendered document
		// tidy; omitting someone else's key loses information we promised to carry.
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

// unknownKeys returns the unknown keys in the order Parse saw them, falling back to
// sorted order for a document assembled in memory rather than read from a file.
func (d Doc) unknownKeys() []string {
	if len(d.unknownOrder) == len(d.Unknown) {
		return d.unknownOrder
	}
	return sortedKeys(d.Unknown)
}

// writeField emits one key: value line, skipping an empty optional value so a
// rendered document carries no blank fields.
func writeField(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(quote(value))
	b.WriteString("\n")
}

// quote renders a top-level scalar, preferring a block scalar for multi-line values
// because a description is read by people, and falling back to a double-quoted
// scalar whenever a block would not survive the trip back. A value that round trips
// is worth more than a pretty file.
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

// blockSafe reports whether a multi-line value survives a literal block scalar.
//
// Two shapes do not. A value whose first content line is itself indented sets the
// block's indent above the indent the later lines get, and YAML ends the block at the
// first line below it, truncating the value. A value with a trailing newline cannot
// be expressed with the strip indicator this writer uses, since trailing blank lines
// are exactly what strip removes. Both go out double-quoted instead.
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

// quoteInline renders a scalar on one line, which is the only shape a value nested
// under metadata can take: parseNestedMap reads a single line per key, so a block
// scalar written there would not be read back at all.
func quoteInline(s string) string {
	if needsQuoting(s) {
		return `"` + escapeDouble(s) + `"`
	}
	return s
}

// needsQuoting reports whether a plain scalar would be read back as something other
// than itself: one this grammar refuses, one that would look like a comment or a
// nested key, or one whose whitespace would be trimmed away.
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

// escapeDouble escapes a scalar for a double-quoted context, covering exactly the
// escapes unquoteDouble resolves.
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

// sortedKeys returns a map's keys in sorted order, so rendering is deterministic and
// a formatted document does not churn between runs.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
