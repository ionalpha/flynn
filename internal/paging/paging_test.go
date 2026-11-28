package paging

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"pgregory.net/rapid"
)

// pages serves n pages of size items each, numbered from 0, and counts the fetches.
func pages(n, size int, fetched *int) Fetch[int] {
	return func(_ context.Context, cursor string) ([]int, string, error) {
		*fetched++
		page := 0
		if cursor != "" {
			page, _ = strconv.Atoi(cursor)
		}
		items := make([]int, size)
		for i := range items {
			items[i] = page*size + i
		}
		next := ""
		if page+1 < n {
			next = strconv.Itoa(page + 1)
		}
		return items, next, nil
	}
}

func TestAllReadsEveryPage(t *testing.T) {
	var fetched int
	got, err := All(t.Context(), 5, pages(3, 2, &fetched))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 6 || fetched != 3 {
		t.Fatalf("All = %v after %d fetches, want 6 items after 3", got, fetched)
	}
}

// TestAllRefusesToStopAtTheCap checks reaching the cap with a next page named is an
// error wrapping ErrTruncated, never a short list returned as whole.
func TestAllRefusesToStopAtTheCap(t *testing.T) {
	var fetched int
	got, err := All(t.Context(), 3, pages(10, 2, &fetched))
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("All error = %v, want ErrTruncated", err)
	}
	if got != nil || fetched != 3 {
		t.Fatalf("All = %v after %d fetches, want nothing after the cap of 3", got, fetched)
	}
}

// TestAllEndingOnTheCapIsComplete checks a collection whose last page is the cap-th
// one is whole: only a named next page makes the cap a truncation.
func TestAllEndingOnTheCapIsComplete(t *testing.T) {
	var fetched int
	got, err := All(t.Context(), 3, pages(3, 1, &fetched))
	if err != nil || len(got) != 3 {
		t.Fatalf("All = %v, %v, want 3 items", got, err)
	}
}

func TestFetchErrorIsReturned(t *testing.T) {
	boom := errors.New("boom")
	fail := func(context.Context, string) ([]int, string, error) { return []int{1}, "x", boom }
	if _, err := All(t.Context(), 3, fail); !errors.Is(err, boom) {
		t.Fatalf("All error = %v, want boom", err)
	}
	if items, complete, err := Prefix(t.Context(), 3, 0, fail); !errors.Is(err, boom) || complete || items != nil {
		t.Fatalf("Prefix = %v, %v, %v, want boom and nothing", items, complete, err)
	}
}

func TestCancelledContextStopsBeforeTheNextFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var fetched int
	if _, err := All(ctx, 3, pages(3, 1, &fetched)); !errors.Is(err, context.Canceled) || fetched != 0 {
		t.Fatalf("All = %v after %d fetches, want context.Canceled before any", err, fetched)
	}
}

func TestPrefixCutsAtTheItemLimit(t *testing.T) {
	var fetched int
	items, complete, err := Prefix(t.Context(), 10, 5, pages(10, 2, &fetched))
	if err != nil || complete || len(items) != 5 || fetched != 3 {
		t.Fatalf("Prefix = %v, %v, %v after %d fetches, want 5 items, partial, after 3", items, complete, err, fetched)
	}
}

// TestPrefixExactlyFull checks a read that fills the item limit exactly is complete
// when no next page is named and partial when one is, without fetching that page.
func TestPrefixExactlyFull(t *testing.T) {
	var fetched int
	if items, complete, err := Prefix(t.Context(), 10, 4, pages(2, 2, &fetched)); err != nil || !complete || len(items) != 4 {
		t.Fatalf("Prefix = %v, %v, %v, want 4 items, whole", items, complete, err)
	}
	fetched = 0
	if items, complete, err := Prefix(t.Context(), 10, 4, pages(3, 2, &fetched)); err != nil || complete || len(items) != 4 || fetched != 2 {
		t.Fatalf("Prefix = %v, %v, %v after %d fetches, want 4 items, partial, after 2", items, complete, err, fetched)
	}
}

// TestPrefixCompleteMeansWhole checks the property callers rely on: complete is true
// exactly when every item of the collection was returned.
func TestPrefixCompleteMeansWhole(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(t, "pages")
		size := rapid.IntRange(0, 4).Draw(t, "size")
		maxPages := rapid.IntRange(0, 10).Draw(t, "maxPages")
		maxItems := rapid.IntRange(0, 40).Draw(t, "maxItems")
		var fetched int
		items, complete, err := Prefix(context.Background(), maxPages, maxItems, pages(n, size, &fetched))
		if err != nil {
			t.Fatalf("Prefix: %v", err)
		}
		whole := n <= maxPages && (maxItems <= 0 || n*size <= maxItems)
		if complete && len(items) != n*size {
			t.Fatalf("complete with %d of %d items", len(items), n*size)
		}
		if !complete && whole {
			t.Fatalf("partial although all %d items fit (pages %d, maxPages %d, maxItems %d)", n*size, n, maxPages, maxItems)
		}
		if maxItems > 0 && len(items) > maxItems {
			t.Fatalf("%d items over the limit of %d", len(items), maxItems)
		}
		if fetched > maxPages {
			t.Fatalf("%d fetches over the cap of %d", fetched, maxPages)
		}
	})
}
