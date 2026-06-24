package resource

import (
	"testing"
	"time"

	"pgregory.net/rapid"
)

func tp(t time.Time) *time.Time { return &t }

func TestValidAt(t *testing.T) {
	created := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		validFrom *time.Time
		validTo   *time.Time
		at        time.Time
		want      bool
	}{
		{"nil-from defaults to creation: before creation", nil, nil, created.Add(-time.Hour), false},
		{"nil-from defaults to creation: at creation", nil, nil, created, true},
		{"nil-from defaults to creation: after creation", nil, nil, created.Add(time.Hour), true},
		{"explicit from: before is invalid", tp(from), nil, from.Add(-time.Nanosecond), false},
		{"explicit from: at from is valid (closed lower)", tp(from), nil, from, true},
		{"explicit to: before to is valid", tp(from), tp(to), to.Add(-time.Nanosecond), true},
		{"explicit to: at to is invalid (open upper)", tp(from), tp(to), to, false},
		{"explicit to: after to is invalid", tp(from), tp(to), to.Add(time.Hour), false},
		{"closed window: inside", tp(from), tp(to), from.Add(24 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Resource{Envelope: Envelope{ValidFrom: tc.validFrom, ValidTo: tc.validTo, CreatedAt: created}}
			if got := r.ValidAt(tc.at); got != tc.want {
				t.Fatalf("ValidAt(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

// TestValidAtProperty is the half-open interval contract: for any creation time,
// any valid window with from <= to, and any query instant, ValidAt is true exactly
// when the instant lies in [from, to), with nil bounds defaulting to creation
// (lower) and unbounded (upper). The interval is half-open, so the lower bound is
// included and the upper bound excluded; adjacent windows therefore tile without
// double-counting an instant.
func TestValidAtProperty(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.AddDate(0, 0, n) }

	rapid.Check(t, func(rt *rapid.T) {
		createdDay := rapid.IntRange(0, 100).Draw(rt, "created")
		hasFrom := rapid.Bool().Draw(rt, "hasFrom")
		hasTo := rapid.Bool().Draw(rt, "hasTo")
		fromDay := rapid.IntRange(0, 100).Draw(rt, "from")
		// Keep the window non-empty: to >= from.
		toDay := rapid.IntRange(fromDay, 200).Draw(rt, "to")
		atDay := rapid.IntRange(-50, 250).Draw(rt, "at")

		r := Resource{Envelope: Envelope{CreatedAt: day(createdDay)}}
		lower := createdDay
		if hasFrom {
			r.ValidFrom = tp(day(fromDay))
			lower = fromDay
		}
		upper := 1 << 30 // unbounded
		if hasTo {
			r.ValidTo = tp(day(toDay))
			upper = toDay
		}

		want := atDay >= lower && atDay < upper
		if got := r.ValidAt(day(atDay)); got != want {
			rt.Fatalf("ValidAt: created=%d from=%v to=%v at=%d => %v, want %v (lower=%d upper=%d)",
				createdDay, r.ValidFrom, r.ValidTo, atDay, got, want, lower, upper)
		}
	})
}
