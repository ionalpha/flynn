package resource_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

const thingAPIVersion = "mem.test.ionagent.io/v1"

var errLogDown = errors.New("log unavailable")

// thingRegistry registers the one unconstrained kind these tests write.
func thingRegistry(t *testing.T) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()
	if err := reg.Register(resource.Kind{APIVersion: thingAPIVersion, Name: "Thing"}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func thing(name string) resource.Resource {
	return resource.Resource{APIVersion: thingAPIVersion, Kind: "Thing", Name: name}
}

// remoteThing builds a Thing as it arrives from another instance: a fully stamped
// record carrying its own envelope, the shape Merge consumes.
func remoteThing(id, name string, scope resource.Scope, wall int64, deleted bool) resource.Resource {
	return resource.Resource{
		APIVersion: thingAPIVersion, Kind: "Thing", ID: id, Name: name, Scope: scope,
		Envelope: resource.Envelope{
			Envelope: envelope.Envelope{
				SyncVersion:      1,
				OriginInstanceID: "remote",
				LastWriterID:     "remote",
				UpdatedHLC:       hlc.Time{Wall: wall},
				Deleted:          deleted,
			},
			Version: 1,
		},
	}
}

// TestPutLeavesNoStateWhenTheLogRejectsTheAppend is the event-sourcing invariant
// under a failing log: the projection is a fold of the log, so a write whose event
// never lands must not be visible. A record projected without its event would make
// a later replay silently lose it.
func TestPutLeavesNoStateWhenTheLogRejectsTheAppend(t *testing.T) {
	ctx := context.Background()
	inner := spine.NewMemoryLog()
	log := testkit.FaultyLog(inner, testkit.Always(errLogDown))
	store := resource.NewMemory(thingRegistry(t), resource.WithEventLog(log))
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.Put(ctx, thing("alpha")); !errors.Is(err, errLogDown) {
		t.Fatalf("Put err = %v, want the log error", err)
	}
	if _, err := store.Get(ctx, "Thing", resource.Scope{}, "alpha"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("a write whose event never landed must not be readable, got %v", err)
	}
	events, err := inner.Read(ctx, spine.Query{Stream: resource.ResourceStream})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("the stream must stay empty, got %d events", len(events))
	}
}

// TestDeleteLeavesTheRecordLiveWhenTheLogRejectsTheAppend holds the same
// invariant on the delete path: a tombstone that could not be recorded must not be
// projected, or the record would come back on the next replay.
func TestDeleteLeavesTheRecordLiveWhenTheLogRejectsTheAppend(t *testing.T) {
	ctx := context.Background()
	// The create appends once and succeeds; the delete's append is the one that fails.
	log := testkit.FaultyLog(spine.NewMemoryLog(), testkit.FailOnCall(2, errLogDown))
	store := resource.NewMemory(thingRegistry(t), resource.WithEventLog(log))
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.Put(ctx, thing("alpha")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Delete(ctx, "Thing", resource.Scope{}, "alpha"); !errors.Is(err, errLogDown) {
		t.Fatalf("Delete err = %v, want the log error", err)
	}
	if _, err := store.Get(ctx, "Thing", resource.Scope{}, "alpha"); err != nil {
		t.Fatalf("a delete whose tombstone never landed must leave the record live, got %v", err)
	}
}

// TestMergeAppliesNothingWhenTheLogRejectsTheAppend covers both merge writes: the
// first-insert of an unseen remote record, and the replacement of a local record by
// a winning remote one. Neither may project without its event.
func TestMergeAppliesNothingWhenTheLogRejectsTheAppend(t *testing.T) {
	ctx := context.Background()

	t.Run("first insert", func(t *testing.T) {
		log := testkit.FaultyLog(spine.NewMemoryLog(), testkit.Always(errLogDown))
		store := resource.NewMemory(thingRegistry(t), resource.WithEventLog(log))
		t.Cleanup(func() { _ = store.Close() })

		if _, err := store.Merge(ctx, remoteThing("rid-1", "alpha", resource.Scope{}, 100, false)); !errors.Is(err, errLogDown) {
			t.Fatalf("Merge err = %v, want the log error", err)
		}
		if _, err := store.GetByID(ctx, "rid-1"); !errors.Is(err, resource.ErrNotFound) {
			t.Fatalf("an unrecorded merge must not be readable, got %v", err)
		}
	})

	t.Run("winning remote replacement", func(t *testing.T) {
		// The first merge records; the second (a later write that wins) fails to append.
		log := testkit.FaultyLog(spine.NewMemoryLog(), testkit.FailOnCall(2, errLogDown))
		store := resource.NewMemory(thingRegistry(t), resource.WithEventLog(log))
		t.Cleanup(func() { _ = store.Close() })

		first := remoteThing("rid-1", "alpha", resource.Scope{}, 100, false)
		res, err := store.Merge(ctx, first)
		if err != nil {
			t.Fatalf("first merge: %v", err)
		}
		if res.Outcome != resource.MergeApplied {
			t.Fatalf("first merge outcome = %v, want applied", res.Outcome)
		}
		later := remoteThing("rid-1", "alpha", resource.Scope{}, 200, false)
		if _, err := store.Merge(ctx, later); !errors.Is(err, errLogDown) {
			t.Fatalf("Merge err = %v, want the log error", err)
		}
		got, err := store.GetByID(ctx, "rid-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.UpdatedHLC.Wall != 100 {
			t.Fatalf("the local record moved to the unrecorded winner (wall %d)", got.UpdatedHLC.Wall)
		}
	})
}

// snapshotErrLog reports a failure when a replay looks for the latest snapshot.
type snapshotErrLog struct {
	spine.Log
	err error
}

func (l snapshotErrLog) LatestSnapshot(context.Context, string, int64) (spine.Snapshot, bool, error) {
	return spine.Snapshot{}, false, l.err
}

// readErrLog reports a failure when a replay reads the stream.
type readErrLog struct {
	spine.Log
	err error
}

func (l readErrLog) Read(context.Context, spine.Query) ([]spine.Event, error) { return nil, l.err }

// TestReplayPropagatesLogFailures proves a rebuild fails loudly rather than
// returning a half-built store: a log that cannot be read, or whose snapshot
// lookup fails, must surface the error instead of an empty projection a caller
// would mistake for "no resources yet".
func TestReplayPropagatesLogFailures(t *testing.T) {
	ctx := context.Background()
	reg := thingRegistry(t)

	for _, tc := range []struct {
		name string
		log  spine.Log
	}{
		{"snapshot lookup fails", snapshotErrLog{Log: spine.NewMemoryLog(), err: errLogDown}},
		{"stream read fails", readErrLog{Log: spine.NewMemoryLog(), err: errLogDown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := resource.Replay(ctx, tc.log, reg)
			if !errors.Is(err, errLogDown) {
				t.Fatalf("Replay err = %v, want the log error", err)
			}
			if store != nil {
				t.Fatal("a failed replay must not return a store")
			}
		})
	}
}

// TestReplayFoldsFromTheStartWhenTheSnapshotIsUnreadable is the "a snapshot is a
// cache, never a source of truth" rule: a stored snapshot whose payload cannot be
// decoded must be discarded and the whole stream folded instead, ending in exactly
// the state the log describes.
func TestReplayFoldsFromTheStartWhenTheSnapshotIsUnreadable(t *testing.T) {
	ctx := context.Background()
	reg := thingRegistry(t)
	log := spine.NewMemoryLog()
	store := resource.NewMemory(reg, resource.WithEventLog(log))
	t.Cleanup(func() { _ = store.Close() })

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := store.Put(ctx, thing(name)); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
	}
	// A snapshot whose payload is not a projection at all: the restore must fail and
	// leave nothing of itself behind.
	if err := log.SaveSnapshot(ctx, spine.Snapshot{
		Stream:  resource.ResourceStream,
		Seq:     3,
		Payload: []byte("{not a projection"),
	}); err != nil {
		t.Fatal(err)
	}

	replayed, err := resource.Replay(ctx, log, reg)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	t.Cleanup(func() { _ = replayed.Close() })
	all, err := replayed.ListAll(ctx, "Thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("a corrupt snapshot must fall back to a full fold, got %d records", len(all))
	}
	for _, r := range all {
		if r.ID == "" || r.ContentHash == "" {
			t.Fatalf("folded record is not the stamped post-image: %+v", r)
		}
	}
}

// TestReplayRejectsAnUnfoldableStream gates the projection vocabulary: an event on
// the resource stream that the fold cannot apply (an unknown type, or a payload
// that is not a resource record) must fail the rebuild rather than skip the event
// and hand back a projection that silently diverges from the log.
func TestReplayRejectsAnUnfoldableStream(t *testing.T) {
	ctx := context.Background()
	reg := thingRegistry(t)

	cases := []struct {
		name string
		in   spine.AppendInput
	}{
		{"unknown event type", spine.AppendInput{
			Stream:     resource.ResourceStream,
			Type:       "resource.invented",
			RawPayload: json.RawMessage(`{"resource":{"Kind":"Thing","Name":"alpha"}}`),
		}},
		{"payload is not a record", spine.AppendInput{
			Stream:     resource.ResourceStream,
			Type:       resource.EvPut,
			RawPayload: json.RawMessage(`{"resource":"not a record"}`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := spine.NewMemoryLog()
			if _, err := log.Append(ctx, tc.in); err != nil {
				t.Fatalf("append: %v", err)
			}
			store, err := resource.Replay(ctx, log, reg)
			if err == nil {
				t.Fatal("Replay must reject a stream it cannot fold")
			}
			if store != nil {
				t.Fatal("a failed replay must not return a store")
			}
		})
	}

	// The unknown type is specifically an admission failure, not an opaque error.
	log := spine.NewMemoryLog()
	if _, err := log.Append(ctx, cases[0].in); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Replay(ctx, log, reg); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for an unknown event type", err)
	}
}

// sealErrCodec fails to seal a snapshot; Open is never reached.
type sealErrCodec struct{ err error }

func (c sealErrCodec) Seal(context.Context, spine.Log, spine.Snapshot) (spine.Snapshot, error) {
	return spine.Snapshot{}, c.err
}

func (c sealErrCodec) Open(_ context.Context, s spine.Snapshot) (spine.Snapshot, error) {
	return s, nil
}

// TestSnapshotFailsWhenTheCodecCannotSeal proves a checkpoint that cannot be
// verified is never written: with a codec configured, an unsealed payload must not
// reach the log (a later replay would then be restoring an unsigned blob).
func TestSnapshotFailsWhenTheCodecCannotSeal(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	store := resource.NewMemory(thingRegistry(t),
		resource.WithEventLog(log),
		resource.WithSnapshotCodec(sealErrCodec{err: errLogDown}),
	)
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Put(ctx, thing("alpha")); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(ctx); !errors.Is(err, errLogDown) {
		t.Fatalf("Snapshot err = %v, want the seal error", err)
	}
	if _, found, err := log.LatestSnapshot(ctx, resource.ResourceStream, 0); err != nil || found {
		t.Fatalf("an unsealed snapshot must not be saved (found=%v err=%v)", found, err)
	}
}

// TestListKeysOrdersByScopeThenName locks the total order the resync sweep relies
// on: keys come back ordered by instance, then project, then workspace, then name,
// so two backends enumerate a kind identically.
func TestListKeysOrdersByScopeThenName(t *testing.T) {
	ctx := context.Background()
	store := resource.NewMemory(thingRegistry(t))
	t.Cleanup(func() { _ = store.Close() })

	scopes := []resource.Scope{
		{Instance: "i2", Project: "p1", Workspace: "w1"},
		{Instance: "i1", Project: "p2", Workspace: "w1"},
		{Instance: "i1", Project: "p1", Workspace: "w2"},
		{Instance: "i1", Project: "p1", Workspace: "w1"},
	}
	for _, sc := range scopes {
		for _, name := range []string{"b", "a"} {
			r := thing(name)
			r.Scope = sc
			if _, err := store.Put(ctx, r); err != nil {
				t.Fatalf("put %v/%s: %v", sc, name, err)
			}
		}
	}

	lister, ok := store.(resource.KeyLister)
	if !ok {
		t.Fatal("the in-memory store must implement KeyLister")
	}
	keys, err := lister.ListKeys(ctx, "Thing")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range keys {
		got = append(got, fmt.Sprintf("%s/%s/%s/%s", k.Scope.Instance, k.Scope.Project, k.Scope.Workspace, k.Name))
	}
	want := []string{
		"i1/p1/w1/a", "i1/p1/w1/b",
		"i1/p1/w2/a", "i1/p1/w2/b",
		"i1/p2/w1/a", "i1/p2/w1/b",
		"i2/p1/w1/a", "i2/p1/w1/b",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ListKeys order:\n got:  %v\n want: %v", got, want)
	}
}

// TestListAllOrdersAcrossInstances gates the cross-scope listing order on the
// outermost axis: a name repeated in two instances is ordered by instance first, so
// a fleet-wide view is deterministic.
func TestListAllOrdersAcrossInstances(t *testing.T) {
	ctx := context.Background()
	store := resource.NewMemory(thingRegistry(t))
	t.Cleanup(func() { _ = store.Close() })

	for _, inst := range []string{"i2", "i1"} {
		r := thing("shared")
		r.Scope = resource.Scope{Instance: inst}
		if _, err := store.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.ListAll(ctx, "Thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 records across instances, got %d", len(all))
	}
	if all[0].Scope.Instance != "i1" || all[1].Scope.Instance != "i2" {
		t.Fatalf("ListAll must order by instance first, got %q then %q",
			all[0].Scope.Instance, all[1].Scope.Instance)
	}
}

// TestMergedTombstoneForAnUnseenScopeIsInert covers the replicated-delete case
// where the local instance has never seen the record's scope: the tombstone must
// project (so a later write is resolved against it) without disturbing the live
// index of the scopes it does know.
func TestMergedTombstoneForAnUnseenScopeIsInert(t *testing.T) {
	ctx := context.Background()
	store := resource.NewMemory(thingRegistry(t))
	t.Cleanup(func() { _ = store.Close() })

	local := thing("alpha")
	if _, err := store.Put(ctx, local); err != nil {
		t.Fatal(err)
	}

	tomb := remoteThing("rid-1", "beta", resource.Scope{Project: "never-seen"}, 100, true)
	res, err := store.Merge(ctx, tomb)
	if err != nil {
		t.Fatalf("merge tombstone: %v", err)
	}
	if res.Outcome != resource.MergeApplied {
		t.Fatalf("outcome = %v, want applied", res.Outcome)
	}
	if _, err := store.GetByID(ctx, "rid-1"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("a merged tombstone must not read as live, got %v", err)
	}
	all, err := store.ListAll(ctx, "Thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Name != "alpha" {
		t.Fatalf("the tombstone disturbed the live index: %+v", all)
	}

	// A later write from the same origin still resolves against the tombstone and
	// resurrects the record, proving the tombstone really was projected.
	revived := remoteThing("rid-1", "beta", resource.Scope{Project: "never-seen"}, 200, false)
	if res, err := store.Merge(ctx, revived); err != nil || res.Outcome != resource.MergeApplied {
		t.Fatalf("resurrect merge = %v, %v; want applied", res.Outcome, err)
	}
	if _, err := store.GetByID(ctx, "rid-1"); err != nil {
		t.Fatalf("the record must be live again after a later write: %v", err)
	}
}
