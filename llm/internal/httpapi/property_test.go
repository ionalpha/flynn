package httpapi

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
)

// TestPropStatusClassification pins the retry-vs-fail decision over arbitrary
// statuses and error bodies: a 429 is terminal exactly when the body carries a
// quota signal (the structured insufficient_quota type/code or a
// credit/billing/quota phrase, any casing), other 429s and all 5xx are
// transient, and everything else is terminal. This is the one classifier every
// provider adapter shares, so the property holds for all of them at once.
func TestPropStatusClassification(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		status := rapid.SampledFrom([]int{400, 401, 403, 404, 408, 429, 500, 502, 503, 529}).Draw(t, "status")

		quotaWord := rapid.SampledFrom([]string{"credit", "billing", "quota"}).Draw(t, "quotaWord")
		structural := rapid.Bool().Draw(t, "structural")
		hasQuota := rapid.Bool().Draw(t, "hasQuota")
		upper := rapid.Bool().Draw(t, "upper")

		msg := "request failed for reasons"
		typ := "some_error"
		if hasQuota {
			if structural {
				typ = "insufficient_quota"
			} else {
				w := quotaWord
				if upper {
					w = strings.ToUpper(w)
				}
				msg = "your " + w + " situation is bad"
			}
		}
		body := fmt.Sprintf(`{"error":{"type":%q,"message":%q}}`, typ, msg)

		err := statusError("prov", status, []byte(body))
		got := fault.Classify(err)

		var want fault.Class
		switch {
		case status == 429 && hasQuota:
			want = fault.Terminal
		case status == 429 || status >= 500:
			want = fault.Transient
		default:
			want = fault.Terminal
		}
		if got != want {
			t.Fatalf("status %d quota=%v: class %v, want %v (err %v)", status, hasQuota, got, want, err)
		}
	})
}
