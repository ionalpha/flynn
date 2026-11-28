// Package paging reads a paginated collection with a page cap that cannot pass
// for the end of the collection.
//
// A cap is needed, because the responder picks the next cursor and a bad one would
// loop forever. Reaching it is not the same as reaching the last page: a caller that
// treats the items read so far as the whole collection reports a pull request's
// conversations as tidied while every thread past the cap stays open, or approves a
// diff whose later files it never saw. Every page loop in the repository goes through
// [All] or [Prefix], which say which of the two happened, and a gate test fails a loop
// bounded by a page cap written anywhere else.
package paging

import (
	"context"
	"errors"
	"fmt"
)

// ErrTruncated is wrapped by the error [All] returns when the page cap is reached
// with pages still to come.
var ErrTruncated = errors.New("paging: the page cap was reached with pages still to come")

// Fetch reads the page at cursor ("" for the first) and returns its items and the
// cursor of the next page, or "" when it was the last. A responder that says more
// pages exist without saying where they are has not said the collection ended, so
// Fetch reports that as an error rather than as a "" cursor.
type Fetch[T any] func(ctx context.Context, cursor string) (items []T, next string, err error)

// All reads every page, at most maxPages of them. It fails with an error wrapping
// [ErrTruncated] when the cap is reached and the last page read named a next one,
// because a list missing its tail would be read as complete. A fetch error is
// returned as it is. A cancelled ctx stops it before the next fetch.
func All[T any](ctx context.Context, maxPages int, fetch Fetch[T]) ([]T, error) {
	items, complete, err := Prefix(ctx, maxPages, 0, fetch)
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, fmt.Errorf("%w (%d pages)", ErrTruncated, maxPages)
	}
	return items, nil
}

// Prefix reads pages until the collection ends, maxPages have been read, or more
// than maxItems items are held (maxItems <= 0 means no item limit). It returns at
// most maxItems items and complete, which is true only when they are the whole
// collection: the last page named no next one and nothing was cut. A caller that
// can show a partial read (a diff marked incomplete, so approval is refused) uses
// it; one that cannot uses [All].
func Prefix[T any](ctx context.Context, maxPages, maxItems int, fetch Fetch[T]) (items []T, complete bool, err error) {
	cursor := ""
	for range maxPages {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		batch, next, err := fetch(ctx, cursor)
		if err != nil {
			return nil, false, err
		}
		items = append(items, batch...)
		if maxItems > 0 && len(items) > maxItems {
			return items[:maxItems], false, nil
		}
		if next == "" {
			return items, true, nil
		}
		if maxItems > 0 && len(items) == maxItems {
			// Exactly full with a next page named: whether that page holds anything is
			// unknown, and fetching it only to discard it costs a request.
			return items, false, nil
		}
		cursor = next
	}
	return items, false, nil
}
