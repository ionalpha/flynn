//go:build ignore

// Test fixture for secret-expose.yml. Lives under .semgrep, which the Go tool
// skips (it ignores dot-directories), so it is never built or linted. The
// annotations drive `semgrep --test`: a `ruleid:` line must produce a finding, an
// `ok:` line must not.
package fixtures

import (
	"fmt"
	"log"
)

type cred struct{ b []byte }

func (c cred) Expose() string { return string(c.b) }

func leaks(c cred, w *log.Logger) {
	// ruleid: secret-expose-not-logged-or-formatted
	_ = fmt.Sprintf("token=%s", c.Expose())
	// ruleid: secret-expose-not-logged-or-formatted
	log.Printf("authenticating with %s", c.Expose())
	// ruleid: secret-expose-not-logged-or-formatted
	_ = fmt.Errorf("auth with %s failed", c.Expose())
	// ruleid: secret-expose-not-logged-or-formatted
	fmt.Println("Bearer " + c.Expose())
}

func safe(c cred) {
	// ok: secret-expose-not-logged-or-formatted
	header := "Bearer " + c.Expose()
	setHeader("Authorization", header)
	// ok: secret-expose-not-logged-or-formatted
	store("api_key", c.Expose())
}

func setHeader(key, value string) {}
func store(ref, value string)     {}
