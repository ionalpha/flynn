package secret

import (
	"context"
	"errors"
	"os"
)

// ErrNotFound is returned by a Source when a reference resolves to no secret. It
// is a distinct sentinel so a caller can tell "this credential is not configured"
// apart from a backend failure (a locked keychain, an unreachable vault).
var ErrNotFound = errors.New("secret: not found")

// Source resolves a named reference to a secret value. It is the vault seam: the
// agent asks for a credential by name and a backend supplies it, so the rest of
// the code never reads a raw environment variable or file. The default EnvSource
// keeps the agent zero-setup; an OS keychain, an age-encrypted file vault, a KMS,
// or a remote broker implement the same interface and swap in without touching the
// call sites. A Source must be safe for concurrent use and must return ErrNotFound
// (not an empty Text) for an absent reference.
type Source interface {
	Lookup(ctx context.Context, ref string) (Text, error)
}

// EnvSource resolves a reference to the process environment variable of the same
// name. It is the standalone default: it needs no configuration and works on every
// host. It is the only place the agent reads a credential from the environment, so
// the environment-as-secret-store assumption lives behind the port rather than
// scattered through the code.
type EnvSource struct{}

// Lookup returns the value of the environment variable named ref, or ErrNotFound
// if it is unset or empty. An empty variable is treated as absent so a blank
// export does not silently produce an empty credential that fails later at the API.
func (EnvSource) Lookup(_ context.Context, ref string) (Text, error) {
	v, ok := os.LookupEnv(ref)
	if !ok || v == "" {
		return Text{}, ErrNotFound
	}
	return New(v), nil
}

var _ Source = EnvSource{}
