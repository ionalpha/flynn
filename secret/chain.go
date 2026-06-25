package secret

import (
	"context"
	"errors"
)

// Chain resolves a reference through an ordered list of Sources, returning the
// first value found. It is how a binary composes credential backends without the
// call site knowing the order: try the OS keychain, then a sealed file, then the
// environment, and stop at the first hit. A Source that returns ErrNotFound is
// skipped; any other error stops the chain and is returned, so a locked keychain
// or a corrupt vault surfaces rather than being silently masked by a later
// fallback. With no source holding the reference the chain returns ErrNotFound.
func Chain(sources ...Source) Source { return chain(sources) }

type chain []Source

func (c chain) Lookup(ctx context.Context, ref string) (Text, error) {
	for _, s := range c {
		v, err := s.Lookup(ctx, ref)
		switch {
		case err == nil:
			return v, nil
		case errors.Is(err, ErrNotFound):
			continue
		default:
			return Text{}, err
		}
	}
	return Text{}, ErrNotFound
}
