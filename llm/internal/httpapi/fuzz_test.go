package httpapi

import "testing"

// FuzzStatusError drives the shared provider error-body classifier. On a non-2xx
// response every adapter routes the body through statusError, which parses the
// provider error envelope to decide retry-vs-fail. The body is untrusted (a
// hostile or broken endpoint, reachable via a base-URL override, controls it), so
// the bar is that no body panics and statusError always returns a non-nil,
// classified error: a body it cannot parse falls back to using the raw text as the
// message rather than faulting.
func FuzzStatusError(f *testing.F) {
	seeds := []string{
		`{"error":{"type":"insufficient_quota","message":"you're out"}}`,
		`{"error":{"code":"insufficient_quota"}}`,
		`{"error":{"message":"credit balance is too low"}}`,
		`{"error":{"message":"rate limited"}}`,
		`{"error":{"type":123}}`, // wrong scalar type inside the envelope
		`{"error":"a string, not an object"}`,
		`plain text error page`,
		`{}`,
		`null`,
		``,
	}
	for _, s := range seeds {
		f.Add(429, []byte(s))
	}
	f.Add(500, []byte(`<html>gateway</html>`))
	f.Add(400, []byte(`{"error":{"message":"bad request"}}`))

	f.Fuzz(func(t *testing.T, code int, body []byte) {
		if err := statusError("prov", code, body); err == nil {
			t.Fatalf("statusError returned nil for code=%d body=%q", code, body)
		}
	})
}
