package closeerr

import (
	"errors"
	"testing"

	"pgregory.net/rapid"
)

// TestKeepProperty: for any prior and close outcome, a prior error is never replaced and a
// nil prior takes the close error.
func TestKeepProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var prior, closeEr error
		if rapid.Bool().Draw(t, "priorFails") {
			prior = errors.New("prior")
		}
		if rapid.Bool().Draw(t, "closeFails") {
			closeEr = errors.New("close")
		}
		err := prior
		Keep(&err, fakeCloser{closeEr})
		want := prior
		if want == nil {
			want = closeEr
		}
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	})
}
