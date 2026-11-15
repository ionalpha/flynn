package secret

import (
	"context"
	"errors"
	"os"
)

// ErrNotFound means the credential is not configured, as distinct from a backend
// failure such as a locked keychain.
var ErrNotFound = errors.New("secret: not found")

// Source resolves a credential by name, so no other code reads a raw variable or
// file. An implementation must be safe for concurrent use and must return
// [ErrNotFound], never an empty [Text], for an absent reference.
type Source interface {
	Lookup(ctx context.Context, ref string) (Text, error)
}

// EnvSource reads the environment variable named by the reference. It is the
// default [Source] and the only code that reads a credential from the
// environment.
type EnvSource struct{}

// Lookup treats an empty variable as unset, so a blank export fails here and
// not later at the API.
func (EnvSource) Lookup(_ context.Context, ref string) (Text, error) {
	v, ok := os.LookupEnv(ref)
	if !ok || v == "" {
		return Text{}, ErrNotFound
	}
	return New(v), nil
}

var _ Source = EnvSource{}
