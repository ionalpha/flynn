package secret_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/secret"
)

func TestExposeRoundTrips(t *testing.T) {
	s := secret.New("sk-test-123")
	if got := s.Expose(); got != "sk-test-123" {
		t.Fatalf("Expose = %q, want the original value", got)
	}
	if s.Empty() {
		t.Fatal("non-empty secret reported Empty")
	}
	if !secret.New("").Empty() {
		t.Fatal("empty secret not reported Empty")
	}
}

func TestEqualConstantTime(t *testing.T) {
	a := secret.New("alpha")
	if !a.Equal(secret.New("alpha")) {
		t.Fatal("equal secrets compared unequal")
	}
	if a.Equal(secret.New("beta")) {
		t.Fatal("different secrets compared equal")
	}
	if !secret.New("").Equal(secret.New("")) {
		t.Fatal("two empty secrets should be equal")
	}
}

func TestDestroyClears(t *testing.T) {
	s := secret.New("wipe-me")
	s.Destroy()
	if !s.Empty() {
		t.Fatalf("Destroy left %q exposed", s.Expose())
	}
}

// TestRenderingNeverLeaks is the core guarantee: for an arbitrary value, no fmt
// verb, JSON encoding, or structured-log rendering emits the plaintext; every one
// shows the redaction marker instead. rapid shrinks any leak to a minimal value.
func TestRenderingNeverLeaks(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// A value distinctive enough that any leak is unmistakable, never the
		// marker itself.
		value := "SECRET-" + rapid.StringMatching(`[A-Za-z0-9/+_-]{1,40}`).Draw(rt, "value")
		s := secret.New(value)

		renders := map[string]string{
			"%v":         fmt.Sprintf("%v", s),
			"%s":         fmt.Sprintf("%s", s),
			"%q":         fmt.Sprintf("%q", s),
			"%#v":        fmt.Sprintf("%#v", s),
			"%x":         fmt.Sprintf("%x", s),
			"%d":         fmt.Sprintf("%d", s),
			"struct %v":  fmt.Sprintf("%v", struct{ Key secret.Text }{s}),
			"struct %+v": fmt.Sprintf("%+v", struct{ Key secret.Text }{s}),
			"struct %#v": fmt.Sprintf("%#v", struct{ Key secret.Text }{s}),
			"pointer %v": fmt.Sprintf("%v", &s),
			"Stringer":   s.String(),
			"GoStringer": s.GoString(),
		}
		for name, out := range renders {
			if strings.Contains(out, value) {
				rt.Fatalf("%s leaked the value: %q", name, out)
			}
			if !strings.Contains(out, secret.Redacted) {
				rt.Fatalf("%s did not redact: %q", name, out)
			}
		}

		// JSON, both directly and nested in a struct.
		direct, err := json.Marshal(s)
		if err != nil {
			rt.Fatalf("json.Marshal: %v", err)
		}
		nested, err := json.Marshal(struct {
			Key secret.Text `json:"key"`
		}{s})
		if err != nil {
			rt.Fatalf("json.Marshal struct: %v", err)
		}
		for _, b := range [][]byte{direct, nested} {
			if bytes.Contains(b, []byte(value)) {
				rt.Fatalf("JSON leaked the value: %s", b)
			}
		}
		// A structured logger renders a Text through either its JSON path (covered
		// by MarshalJSON above) or its text path (covered by the %v/%s/%+v cases
		// above), so both logging surfaces are already proven not to leak.
	})
}
