package editor

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// keymapFile maps chord spellings to command names; it lists only changes.
type keymapFile struct {
	Bindings map[string]Command `json:"bindings"`
}

// LoadKeymap reads a JSON keymap over the default map; binding a chord to
// "none" removes its default. Unknown commands and malformed chords are errors,
// since a misspelled name would otherwise bind nothing with no way to debug it.
func LoadKeymap(r io.Reader) (Keymap, error) {
	var f keymapFile
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("keymap: parse: %w", err)
	}
	km := Default()
	for spelling, cmd := range f.Bindings {
		chord, err := ParseChord(spelling)
		if err != nil {
			return nil, err
		}
		if cmd == CmdUnbind {
			delete(km, chord)
			continue
		}
		if _, ok := commands[cmd]; !ok {
			return nil, fmt.Errorf("keymap: chord %q: unknown command %q (commands: %s)", spelling, cmd, knownCommands())
		}
		km[chord] = cmd
	}
	return km, nil
}

func knownCommands() string {
	names := make([]string, 0, len(commands)+1)
	for cmd := range commands {
		names = append(names, string(cmd))
	}
	names = append(names, string(CmdUnbind))
	sort.Strings(names)
	return strings.Join(names, ", ")
}
