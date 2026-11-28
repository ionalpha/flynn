//go:build ruleguard

// Package gorules holds this repository's ruleguard rules. gocritic loads the
// file through .golangci.yml; the build tag keeps it out of the module's own
// build. Every rule has a bad and a good example under
// internal/rigor/testdata/ruleguard, checked by the tests in internal/rigor.
package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// logAndReturn reports an error that is logged and then returned. The caller
// receives the error and logs it again, so one failure reads as two.
func logAndReturn(m dsl.Matcher) {
	m.Import("github.com/ionalpha/flynn/observe")
	m.Match(
		`$log.Error($*_); return $err`,
		`$log.Error($*_); return $_, $err`,
		`$log.Error($*_); return $_, $_, $err`,
		`$log.Warn($*_); return $err`,
		`$log.Warn($*_); return $_, $err`,
		`$log.Warn($*_); return $_, $_, $err`,
	).
		Where(m["log"].Type.Implements("observe.Logger") &&
			m["err"].Type.Implements("error") &&
			m["err"].Text != "nil" &&
			!m.File().Name.Matches(`_test\.go$`)).
		Report("error is logged and then returned: do one, and wrap the error with context if the caller needs it")
}

// persistedTimestamp reports time.RFC3339Nano used to write a time. The layout
// trims trailing zeros, so a whole second ("12:00:00Z") sorts after the half
// second that follows it ("12:00:00.5Z") in a text column.
func persistedTimestamp(m dsl.Matcher) {
	m.Match(
		`$t.Format(time.RFC3339Nano)`,
		`$t.AppendFormat($_, time.RFC3339Nano)`,
	).
		Where(m["t"].Type.Is("time.Time") && !m.File().Name.Matches(`_test\.go$`)).
		Report("time.RFC3339Nano trims trailing zeros, so its strings do not sort in time order: use sqlitex.FormatTime or sqlitex.TimeLayout")
}

// unstableSort reports sort.Slice in the packages whose output must be the same
// on every run and every backend. sort.Slice leaves equal elements in any order.
// The package list matches the one the map-order check uses.
func unstableSort(m dsl.Matcher) {
	m.Match(`sort.Slice($_, $_)`).
		Where(m.File().PkgPath.Matches(`^github\.com/ionalpha/flynn/(internal/rigor/testdata/ruleguard/)?(chain|goal|hlc|internal/migrate|internal/spinesink|internal/sqlitex|jobs|mission|resource|session|spine|state|storage/sqlite|usage)(/|$)`) &&
			!m.File().Name.Matches(`_test\.go$`)).
		Report("sort.Slice does not keep the order of equal elements: use sort.SliceStable, slices.SortStableFunc, or slices.SortFunc with a comparison that breaks every tie")
}
