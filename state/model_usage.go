package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/usage"
)

// UsageStore keeps [usage.Record]s: one account per model call, keyed by
// (Run, Call).
//
// The key makes a collector safe to retry. One that crashes after storing a
// record and before it knows it did sends the same record again, and counting
// that call twice would overstate spend with nothing in the data to show it. So
// appending a stored key is a no-op when the record is identical and is refused
// with [ErrConflict] when it differs: an overwrite would let a late report
// rewrite what a budget already charged. A correction is a new record under a
// new Call.
//
// Every query orders by [SortUsageRecords] and returns At in UTC, so
// [usage.Sum] over a result equals [usage.Sum] over the records appended, on any
// implementation.
type UsageStore interface {
	// Append stores records. It is all or nothing: if any record is invalid
	// ([ErrInvalid], from [usage.Record.Validate]) or conflicts with a stored
	// one ([ErrConflict]), none is stored. A record identical to one already
	// stored, or repeated within the call, is stored once.
	Append(ctx context.Context, records ...usage.Record) error

	// ByRun returns the records of one run, excluding the runs it spawned.
	ByRun(ctx context.Context, run string) ([]usage.Record, error)

	// ByTree returns the records of root and of every run descended from it,
	// where a run is a child of the run named by its records' ParentRun. It is
	// how a total is taken up a spawn tree, and it terminates on a ParentRun
	// cycle. An unknown root returns no records and no error.
	ByTree(ctx context.Context, root string) ([]usage.Record, error)

	// Window returns the records with At in [from, to). A zero from or to leaves
	// that side open. A non-empty principal keeps only that principal's records;
	// an empty one keeps all, so there is no way to ask for records that have no
	// principal.
	Window(ctx context.Context, from, to time.Time, principal string) ([]usage.Record, error)
}

// UsageProvider is implemented by a [Provider] that can store model usage records. A
// caller finds it by type assertion:
//
//	if up, ok := p.(state.UsageProvider); ok { store := up.Usage() }
//
// It is a separate interface because adding a method to [Provider] would break
// every host that already implements it. A provider without it means model usage is
// not stored; a caller should say so and carry on, because losing the account
// of a call is no reason to fail the run that made it.
type UsageProvider interface {
	Usage() UsageStore
}

// SameUsageRecord reports whether a and b are the same record, comparing At as
// an instant so the same call reported in two time zones is one record. It is
// the test [UsageStore.Append] applies to a key that is already stored.
func SameUsageRecord(a, b usage.Record) bool {
	a.At, b.At = a.At.UTC(), b.At.UTC()
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	return aerr == nil && berr == nil && bytes.Equal(ab, bb)
}

// SortUsageRecords orders records by At, then Call, then Run, so every backend
// returns the same order. Run is last only to make the order total: two runs may
// number their calls alike and report the same instant.
func SortUsageRecords(rs []usage.Record) {
	slices.SortFunc(rs, func(a, b usage.Record) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		if c := strings.Compare(a.Call, b.Call); c != 0 {
			return c
		}
		return strings.Compare(a.Run, b.Run)
	})
}

// PlanUsageAppend decides which of records a [UsageStore] must store, so every
// implementation applies the same rules. lookup returns the stored record for a
// key, if any. It returns the records to store, validated, with At in UTC and
// duplicates removed, or an error wrapping [ErrInvalid] or [ErrConflict] with
// nothing to store. An implementation calls it inside the transaction (or under
// the lock) that stores the result, so the check and the write cannot be
// separated by a concurrent append of the same key.
func PlanUsageAppend(records []usage.Record, lookup func(run, call string) (usage.Record, bool, error)) ([]usage.Record, error) {
	fresh := make([]usage.Record, 0, len(records))
	planned := make(map[modelUsageIdent]usage.Record, len(records))
	for _, r := range records {
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		r.At = r.At.UTC()
		key := modelUsageIdent{r.Run, r.Call}
		if prev, ok := planned[key]; ok {
			if !SameUsageRecord(prev, r) {
				return nil, modelUsageConflict(r)
			}
			continue
		}
		stored, ok, err := lookup(r.Run, r.Call)
		if err != nil {
			return nil, err
		}
		planned[key] = r
		if !ok {
			fresh = append(fresh, r)
			continue
		}
		if !SameUsageRecord(stored, r) {
			return nil, modelUsageConflict(r)
		}
	}
	return fresh, nil
}

func modelUsageConflict(r usage.Record) error {
	return fmt.Errorf("%w: usage record (run %q, call %q) is already stored with different content; a correction takes a new call id", ErrConflict, r.Run, r.Call)
}

// modelUsageIdent is the key of a usage record. A struct and not a joined string, so
// no pair of ids can collide with another pair.
type modelUsageIdent struct{ run, call string }

// RecordModelUsage builds the event that records newly stored usage records. The
// payload is the records themselves, so replaying the log restores the store
// without consulting a clock or a price table.
func (s *Stamper) RecordModelUsage(records []usage.Record) (spine.AppendInput, error) {
	return s.input(EvModelUsageRecorded, map[string]any{keyModelUsage: records})
}

// DecodeModelUsageRecords extracts the records from an [EvModelUsageRecorded] payload.
func DecodeModelUsageRecords(payload map[string]any) ([]usage.Record, error) {
	var rs []usage.Record
	if err := decodeRecord(payload, keyModelUsage, &rs); err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("state: event payload is missing %q", keyModelUsage)
	}
	return rs, nil
}
