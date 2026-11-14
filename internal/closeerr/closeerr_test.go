package closeerr

import (
	"errors"
	"testing"
)

type fakeCloser struct{ err error }

func (f fakeCloser) Close() error { return f.err }

func TestKeepFirstErrorWins(t *testing.T) {
	first, closeFail := errors.New("write failed"), errors.New("close failed")
	tests := []struct {
		name    string
		prior   error
		closeEr error
		want    error
	}{
		{"both nil", nil, nil, nil},
		{"close fails alone", nil, closeFail, closeFail},
		{"write error is not masked", first, closeFail, first},
		{"write error stays with a clean close", first, nil, first},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.prior
			Keep(&err, fakeCloser{tc.closeEr})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}
