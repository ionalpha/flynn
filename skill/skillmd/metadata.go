package skillmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The metadata encoding convention.
//
// The specification's metadata map is the sanctioned place for "properties not
// defined by the Agent Skills spec", and its values are strings. Everything we
// carry beyond the defined fields goes there, and anything that is not already a
// string has to encode itself. This file is that convention, written once because
// it is a contract with a second consumer: Ion Alpha's importer reads what Flynn
// writes, and a convention that drifts between the two silently loses data.
//
// Two rules, and the reasons they are these rules rather than the shorter ones.
//
// Keys are namespaced with a reverse-DNS prefix. The metadata map is a single flat
// namespace shared with every other tool that writes to it, so an unprefixed
// "check" or "tags" key is a collision waiting for the first pack authored against
// another runtime. The prefix makes our keys unmistakably ours and makes someone
// else's unmistakably not, which is what lets a foreign pack round-trip through us
// untouched.
//
// Lists encode as a JSON array, not as a delimited string. A delimited encoding
// has to answer what happens to a value containing the delimiter, and every answer
// is either an escaping scheme nobody implements the same way twice or silent data
// loss when a tag holds a space. JSON is unambiguous for every string, is in the
// standard library on both sides, and distinguishes an empty list from an absent
// key, which a delimited encoding cannot.
const (
	// MetadataPrefix namespaces every metadata key Flynn writes. Reserved: a key
	// under it is ours to define, and a reader may assume no other tool writes there.
	MetadataPrefix = "ionagent.io/"

	// MetaCheck is the skill's verification command, the shell line that re-grades it
	// as the environment changes. A plain string, so it is stored as written.
	MetaCheck = MetadataPrefix + "check"

	// MetaTags is the skill's tag list, JSON-encoded per EncodeList.
	MetaTags = MetadataPrefix + "tags"

	// MetaTitle is the skill's human-readable title, which the format has no field
	// for: its name is an identifier, constrained to lowercase and hyphens, and a
	// title is prose. Written only when the title says something the name does not,
	// so a skill whose title is just its slug does not carry a redundant key.
	MetaTitle = MetadataPrefix + "title"
)

// EncodeList renders a list of strings as one metadata value. A nil or empty list
// encodes as "[]" rather than "", so a caller that stored an empty list and one
// that stored nothing are distinguishable by whether the key is present at all.
func EncodeList(vs []string) string {
	if vs == nil {
		vs = []string{}
	}
	// The error is discarded because marshaling a []string cannot produce one: every
	// Go string encodes, invalid UTF-8 included, which encoding/json rewrites to the
	// replacement rune rather than refusing. Returning an error here would put a
	// branch in every caller for a case that cannot occur.
	b, _ := json.Marshal(vs)
	return string(b)
}

// DecodeList reads a metadata value written by EncodeList. It is strict: a value
// that is not a JSON array of strings is an error, never a best guess. A pack from
// a registry can carry anything under our prefix, and a decoder that falls back to
// treating the raw string as a single-element list turns a malformed field into a
// plausible tag that no one notices.
func DecodeList(v string) ([]string, error) {
	// Decoded through *string rather than string so a null element is visible. Into a
	// []string, JSON null becomes "" without an error, which would turn a malformed
	// list into one carrying a silent empty tag.
	var raw []*string
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return nil, fmt.Errorf("%w: metadata list %q is not a JSON string array: %w", ErrInvalid, v, err)
	}
	if raw == nil {
		// "null" unmarshals into a nil slice without erroring. It is not what
		// EncodeList writes, and it is not an empty list.
		return nil, fmt.Errorf("%w: metadata list is null, want a JSON string array", ErrInvalid)
	}
	out := make([]string, len(raw))
	for i, p := range raw {
		if p == nil {
			return nil, fmt.Errorf("%w: metadata list %q has a null at element %d", ErrInvalid, v, i)
		}
		out[i] = *p
	}
	return out, nil
}

// IsOurs reports whether a metadata key is in the namespace Flynn reserves. Use it
// to split a foreign pack's metadata from our own: keys it rejects are carried
// through untouched on export, because they belong to whoever wrote them.
func IsOurs(key string) bool { return strings.HasPrefix(key, MetadataPrefix) }
