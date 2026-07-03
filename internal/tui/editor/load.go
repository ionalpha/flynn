package editor

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// keymapFile is the on-disk keymap shape: chord spellings mapped to command
// names. A user keymap only writes the bindings it changes.
type keymapFile struct {
	Bindings map[string]Command `json:"bindings"`
}

// LoadKeymap reads a JSON keymap and returns it layered over the default
// map, so a user keymap starts from the complete built-in set and only
// overrides what it names. Binding a chord to "none" removes its default.
// Unknown commands and malformed chords are refused rather than ignored: a
// misspelled name in a keymap file would otherwise silently bind nothing,
// which is the kind of failure a user cannot debug.
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
