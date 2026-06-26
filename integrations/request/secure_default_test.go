package request_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/integrations/request"
)

// TestDefaultTransportRefusesPrivateEgress proves the secure default: a Transport
// built with no options dials through netguard, so it refuses a private, loopback,
// or cloud-metadata target rather than silently allowing a server-side request
// forgery. The dial is blocked at the egress gate (after address resolution, before
// connect), so no real connection is attempted.
func TestDefaultTransportRefusesPrivateEgress(t *testing.T) {
	cases := map[string]string{
		"loopback":       "http://127.0.0.1:9/",
		"private":        "http://10.0.0.1:9/",
		"cloud-metadata": "http://169.254.169.254/latest/meta-data/",
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			tr := request.New() // no WithDoer: exercise the real default
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := tr.Do(context.Background(), req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatalf("expected the default transport to refuse %s, got nil error", url)
			}
			if got := fault.Classify(err); got != fault.Forbidden {
				t.Fatalf("expected a Forbidden egress refusal, got class %q: %v", got, err)
			}
			if !strings.Contains(err.Error(), "egress") {
				t.Fatalf("expected an egress denial, got: %v", err)
			}
		})
	}
}
