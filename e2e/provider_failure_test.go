package e2e

import (
	"testing"
)

// TestProviderPermanentFailureFailsFast asserts that a permanent provider failure stops
// the run fast with a typed error rather than retrying into a hang. A bad key (401) and
// an exhausted quota (a 400 the classifier recognizes as permanent) must each end the
// run after exactly one model call, with a non-zero exit and the provider's status
// surfaced. Retrying either would burn time and money against a request that can never
// succeed, so "exactly one call" is the property that matters.
func TestProviderPermanentFailureFailsFast(t *testing.T) {
	cases := []struct {
		name  string
		reply oaiReply
		want  string
	}{
		{
			name:  "bad_key_401",
			reply: oaiReply{Status: 401, ErrType: "invalid_request_error", ErrMsg: "invalid api key"},
			want:  "HTTP 401",
		},
		{
			name:  "exhausted_quota_400",
			reply: oaiReply{Status: 400, ErrType: "insufficient_quota", ErrMsg: "you exceeded your current quota"},
			want:  "HTTP 400",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeOpenAI(t, c.reply)
			in := newInstance(t).withModel(fake)

			res := in.run("-no-learn", "goal", "trigger a permanent provider failure")
			requireExit(t, res, 1, "permanent failure")
			requireContains(t, res.combined(), c.want, "typed provider error")

			if n := fake.count(); n != 1 {
				t.Fatalf("permanent failure retried: expected exactly 1 model call, got %d", n)
			}
		})
	}
}
