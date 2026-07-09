package integrations

import (
	"bytes"
	"testing"
)

// FuzzDecodeBody drives the response-body decoder a flow step consumes. The body
// and its declared content type are attacker-influenced (a called API decides
// both), so the bar is that no pair panics: a JSON body becomes a typed value, a
// text/* body (or one that fails to parse as JSON) is kept as a string, and an
// empty body is nil.
func FuzzDecodeBody(f *testing.F) {
	seeds := []struct {
		raw string
		ct  string
	}{
		{`{"items":[1,2,3]}`, "application/json"},
		{`[true,null,1.5]`, "application/json"},
		{`42`, "text/plain"}, // a bare scalar under text/* stays a string
		{`42`, "application/json"},
		{`plain text`, "text/html; charset=utf-8"},
		{``, "application/json"},
		{`   `, ""},
		{`{"a":{"b":{"c":1}}}`, ""}, // no content type: decoded as JSON
		{`{bad json`, "application/json"},
		{`"quoted"`, "TEXT/PLAIN"}, // case-insensitive text/* prefix
	}
	for _, s := range seeds {
		f.Add([]byte(s.raw), s.ct)
	}

	f.Fuzz(func(t *testing.T, raw []byte, contentType string) {
		// Contract: never panics, and an empty body always decodes to nil.
		v := decodeBody(raw, contentType)
		if len(bytes.TrimSpace(raw)) == 0 && v != nil {
			t.Fatalf("empty body decoded to non-nil %#v", v)
		}
	})
}
