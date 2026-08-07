package skillmd_test

import (
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/skill/skillmd"
)

func TestEncodeListRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"one", []string{"debugging"}},
		{"several", []string{"debugging", "go", "testing"}},
		{"spaces", []string{"static analysis", "code review"}},
		{"the delimiters a flat encoding would have used", []string{"a,b", "c d", "e;f", "g|h"}},
		{"quotes and backslashes", []string{`a"b`, `c\d`, `"`}},
		{"newlines", []string{"a\nb", "\n"}},
		{"empty string element", []string{""}},
		{"multi-byte", []string{"日本語", "emoji 🙂"}},
		{"leading and trailing space", []string{"  padded  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := skillmd.DecodeList(skillmd.EncodeList(tc.in))
			if err != nil {
				t.Fatalf("DecodeList: %v", err)
			}
			want := tc.in
			if want == nil {
				want = []string{}
			}
			if len(got) != len(want) {
				t.Fatalf("got %d elements %q, want %d %q", len(got), got, len(want), want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("element %d: got %q, want %q", i, got[i], want[i])
				}
			}
		})
	}
}

// TestEncodeListDistinguishesEmptyFromAbsent pins the property a delimited encoding
// cannot offer: an empty list has a representation of its own, so a reader can tell
// "the author stored no tags" from "the author stored nothing".
func TestEncodeListDistinguishesEmptyFromAbsent(t *testing.T) {
	if got := skillmd.EncodeList(nil); got != "[]" {
		t.Fatalf("EncodeList(nil) = %q, want %q", got, "[]")
	}
	if _, err := skillmd.DecodeList(""); err == nil {
		t.Fatal("DecodeList(\"\") succeeded; an absent value must not decode as an empty list")
	}
}

func TestDecodeListRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"null", "null"},
		{"a bare word", "debugging"},
		{"a delimited list, the encoding we did not pick", "a,b,c"},
		{"an object", `{"a":"b"}`},
		{"an array of numbers", `[1,2]`},
		{"an array of arrays", `[["a"]]`},
		{"an array with a null element", `["a",null]`},
		{"unterminated", `["a"`},
		{"trailing content", `["a"] extra`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := skillmd.DecodeList(tc.in)
			if err == nil {
				t.Fatalf("DecodeList(%q) = %q, want an error", tc.in, got)
			}
			if !errors.Is(err, skillmd.ErrInvalid) {
				t.Errorf("error %v does not wrap ErrInvalid", err)
			}
		})
	}
}

// TestEncodeListRoundTripProperty is the round trip over generated values, which is
// the claim the convention actually rests on: every list of strings survives, so no
// tag can be lost or altered by passing through metadata.
func TestEncodeListRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.SliceOf(rapid.String()).Draw(t, "list")
		got, err := skillmd.DecodeList(skillmd.EncodeList(in))
		if err != nil {
			t.Fatalf("DecodeList: %v", err)
		}
		if len(got) != len(in) {
			t.Fatalf("got %d elements, want %d", len(got), len(in))
		}
		for i := range in {
			if got[i] != in[i] {
				t.Fatalf("element %d: got %q, want %q", i, got[i], in[i])
			}
		}
	})
}

// FuzzDecodeList drives the list decoder with arbitrary input. A metadata value
// under our prefix in a pack fetched from a public registry is untrusted text like
// any other: the decoder must return a list or an error and never panic, and
// anything it accepts must re-encode to something that decodes identically, so a
// crafted value cannot become a tag set that differs from what was reviewed.
func FuzzDecodeList(f *testing.F) {
	for _, s := range []string{
		`[]`, `["a"]`, `["a","b"]`, `["a\nb"]`, `[""]`, `null`, ``, `["a",null]`,
		`[1]`, `{"a":"b"}`, `["a"`, `[["a"]]`, `"a"`, `[` + strings.Repeat(`"a",`, 64) + `"b"]`,
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, v string) {
		got, err := skillmd.DecodeList(v)
		if err != nil {
			return
		}
		if got == nil {
			t.Fatalf("DecodeList(%q) returned a nil list with no error", v)
		}
		again, err := skillmd.DecodeList(skillmd.EncodeList(got))
		if err != nil {
			t.Fatalf("re-decode of an encoded list failed: %v\ninput: %q", err, v)
		}
		if len(again) != len(got) {
			t.Fatalf("re-decode changed the length: %d then %d\ninput: %q", len(got), len(again), v)
		}
		for i := range got {
			if again[i] != got[i] {
				t.Fatalf("re-decode changed element %d: %q then %q\ninput: %q", i, got[i], again[i], v)
			}
		}
	})
}

func TestIsOurs(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{skillmd.MetaCheck, true},
		{skillmd.MetaTags, true},
		{skillmd.MetadataPrefix + "anything", true},
		{"check", false},
		{"tags", false},
		{"", false},
		{"example.com/check", false},
		{"ionagent.io", false},
		{" " + skillmd.MetadataPrefix + "check", false},
	} {
		if got := skillmd.IsOurs(tc.key); got != tc.want {
			t.Errorf("IsOurs(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestReservedKeysAreNamespaced guards the convention itself: every key constant
// this package defines sits under the reserved prefix, so adding one that collides
// with a foreign pack's flat key fails here rather than in a user's import.
func TestReservedKeysAreNamespaced(t *testing.T) {
	for _, key := range []string{skillmd.MetaCheck, skillmd.MetaTags} {
		if !strings.HasPrefix(key, skillmd.MetadataPrefix) {
			t.Errorf("metadata key %q is not under the reserved prefix %q", key, skillmd.MetadataPrefix)
		}
		if rest := strings.TrimPrefix(key, skillmd.MetadataPrefix); rest == "" {
			t.Errorf("metadata key %q is the bare prefix", key)
		}
	}
}
