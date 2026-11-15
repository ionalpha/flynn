package secret

import (
	"context"
	"errors"
)

// Chain returns the first value the sources yield, in order. A source returning
// [ErrNotFound] is skipped; any other error is returned at once, so a locked
// keychain is reported instead of hidden by a later fallback.
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
