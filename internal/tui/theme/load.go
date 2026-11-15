package theme

import (
	"encoding/json"
	"fmt"
	"io"
)

// file is the on-disk theme: a name, an optional built-in base and the roles it
// overrides.
type file struct {
	Name   string         `json:"name"`
	Base   string         `json:"base,omitempty"`
	Styles map[Role]Style `json:"styles"`
}

// Load reads a JSON theme layered over its base (default when unset), so a user
// theme overrides only what it names. Unknown roles are refused: a misspelled
// role would otherwise style nothing, silently.
func Load(r io.Reader) (*Theme, error) {
	var f file
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("theme: parse: %w", err)
	}
	base := Builtin(f.Base)
	if base == nil {
		return nil, fmt.Errorf("theme: unknown base %q (built-ins: default, high-contrast, mono)", f.Base)
	}
	for role := range f.Styles {
		if !knownRole(role) {
			return nil, fmt.Errorf("theme: unknown role %q", role)
		}
	}
	styles := make(map[Role]Style, len(base.styles)+len(f.Styles))
	for role, st := range base.styles {
		styles[role] = st
	}
	for role, st := range f.Styles {
		styles[role] = st
	}
	name := f.Name
	if name == "" {
		name = "custom"
	}
	return &Theme{name: name, styles: styles}, nil
}

func knownRole(r Role) bool {
	switch r {
	case UserText, UserPrefix, AssistantText, Code, Quote, Link, Emphasis,
		Strong, Heading, SyntaxKeyword, SyntaxString, SyntaxNumber, SyntaxComment,
		SyntaxFunction, SyntaxType,
		ToolName, ToolDetail, ToolOutput, Admitted, Rejected,
		Trust, DiffAdded, DiffRemoved, DiffContext, DiffLocation, Status,
		StatusBusy, RecordRecording, RecordSealed, RecordVerified, RecordFailed,
		Success, Warning, Error, Muted, Border, Overlay, Selection, Placeholder,
		PasteChip, QueuedChip:
		return true
	}
	return false
}
