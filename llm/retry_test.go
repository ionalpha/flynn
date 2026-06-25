package llm_test

import (
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

func TestRetryClass(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		quotaExhausted bool
		want           fault.Class
	}{
		{"rate-limit 429 retries", 429, false, fault.Transient},
		{"quota 429 is terminal", 429, true, fault.Terminal},
		{"500 retries", 500, false, fault.Transient},
		{"503 retries", 503, false, fault.Transient},
		{"529 overloaded retries", 529, false, fault.Transient},
		{"400 is terminal", 400, false, fault.Terminal},
		{"401 auth is terminal", 401, false, fault.Terminal},
		{"403 is terminal", 403, false, fault.Terminal},
		{"404 is terminal", 404, false, fault.Terminal},
		// A quota flag never makes a non-429 transient or a 5xx terminal: it only
		// promotes a 429 to terminal.
		{"quota flag does not touch 500", 500, true, fault.Transient},
		{"quota flag does not touch 400", 400, true, fault.Terminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := llm.RetryClass(tc.status, tc.quotaExhausted); got != tc.want {
				t.Fatalf("RetryClass(%d, %v) = %s, want %s", tc.status, tc.quotaExhausted, got, tc.want)
			}
		})
	}
}
