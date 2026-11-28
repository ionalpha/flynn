package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/secret"
)

// TestStatusErrorRedactsTheKeyItSent: a refusal that echoes the request's key, in a
// JSON message or a raw body, yields a fault whose text carries neither the key nor
// its bearer form, for every header an adapter puts a key in.
func TestStatusErrorRedactsTheKeyItSent(t *testing.T) {
	const key = "sk-httpapi-probe-91c0e2aa7f" // gitleaks:allow (a fake key the test echoes)
	headers := map[string]string{
		"Authorization":  "Bearer " + key,
		"X-Api-Key":      key,
		"X-Goog-Api-Key": key,
		"Api-Key":        key,
	}
	bodies := map[string]string{
		"json": `{"error":{"type":"authentication_error","message":"invalid key ` + key + `"}}`,
		"raw":  "proxy refused Bearer " + key,
	}
	for hname, hval := range headers {
		for bname, body := range bodies {
			t.Run(hname+"/"+bname, func(t *testing.T) {
				h := http.Header{}
				h.Set(hname, hval)
				err := statusError("prov", http.StatusUnauthorized, []byte(body), credentials(h)...)
				if err == nil {
					t.Fatal("no error for a 401")
				}
				if strings.Contains(err.Error(), key) {
					t.Fatalf("fault text carries the key: %q", err.Error())
				}
				if !strings.Contains(err.Error(), secret.Redacted) {
					t.Fatalf("fault text %q has no redaction marker", err.Error())
				}
			})
		}
	}
}
