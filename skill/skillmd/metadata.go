package skillmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Metadata keys Flynn writes. Every value is a string, and the importer on the host
// reads what Flynn writes, so a change here loses data on the other side.
const (
	// MetadataPrefix namespaces every key Flynn writes, because the metadata map is
	// one flat namespace shared with every tool and an unprefixed "tags" would
	// collide. It is the product's domain: the string travels to other runtimes and
	// must identify the writer, and a bare "flynn/" is a name anyone can use.
	MetadataPrefix = "flynnhq.com/"

	// retiredPrefix is the namespace written before 2026-08-09. It is read and never
	// written, so old keys disappear as skills are rewritten, with no migration over
	// files on other people's disks.
	retiredPrefix = "ionagent.io/"

	// MetaCheck is the shell command that re-grades the skill, stored as written.
	MetaCheck = MetadataPrefix + "check"

	// MetaTags is the tag list, encoded by [EncodeList].
	MetaTags = MetadataPrefix + "tags"

	// MetaTitle is the prose title, which the format has no field for. It is written
	// only when it differs from the name.
	MetaTitle = MetadataPrefix + "title"
)

// EncodeList renders a list as a JSON array, because a delimited string needs an
// escaping scheme for the delimiter. A nil list encodes as "[]", so an empty list
// and an absent key stay distinct.
func EncodeList(vs []string) string {
	if vs == nil {
		vs = []string{}
	}
	// Marshalling a []string cannot fail; invalid UTF-8 becomes the replacement rune.
	b, _ := json.Marshal(vs)
	return string(b)
}

// DecodeList reads a value written by [EncodeList]. Anything but a JSON array of
// strings is an error, because a registry pack can put anything under our prefix
// and a fallback would turn a malformed field into a plausible tag.
func DecodeList(v string) ([]string, error) {
	// *string exposes a null element, which a []string would decode as "".
	var raw []*string
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return nil, fmt.Errorf("%w: metadata list %q is not a JSON string array: %w", ErrInvalid, v, err)
	}
	if raw == nil {
		// "null" decodes to nil without error and is not an empty list.
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

// IsOurs reports whether key is under [MetadataPrefix] or the retired prefix. Other
// keys pass through export untouched. The retired prefix counts as ours, or export
// would carry both spellings for the rest of the skill's life.
func IsOurs(key string) bool {
	return strings.HasPrefix(key, MetadataPrefix) || strings.HasPrefix(key, retiredPrefix)
}

// Get reads key, such as [MetaCheck], falling back to its retired spelling. The
// current spelling wins when both are present.
func Get(meta map[string]string, key string) (string, bool) {
	if v, ok := meta[key]; ok {
		return v, true
	}
	if !strings.HasPrefix(key, MetadataPrefix) {
		return "", false
	}
	v, ok := meta[retiredPrefix+strings.TrimPrefix(key, MetadataPrefix)]
	return v, ok
}
