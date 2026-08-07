package state

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/spine"
)

// errLogDown is the sentinel the injected log failures below carry.
var errLogDown = errors.New("state test: log unavailable")

// breakingLog fails the reads a Replay depends on. The shared fault injectors
// wrap the append path (a write that never lands); the reconstruction path reads
// the snapshot index and the stream, and a Replay that swallowed a failure there
// would silently return a provider missing the tail of its own history.
type breakingLog struct {
	spine.Log
	readErr error
	snapErr error
}

func (l breakingLog) Read(ctx context.Context, q spine.Query) ([]spine.Event, error) {
	if l.readErr != nil {
		return nil, l.readErr
	}
	return l.Log.Read(ctx, q)
}

func (l breakingLog) LatestSnapshot(ctx context.Context, stream string, upToSeq int64) (spine.Snapshot, bool, error) {
	if l.snapErr != nil {
		return spine.Snapshot{}, false, l.snapErr
	}
	return l.Log.LatestSnapshot(ctx, stream, upToSeq)
}

// sealFailCodec is a snapshot codec whose signer is unavailable: it cannot seal.
// Open passes through, since a snapshot it never sealed is never restored.
type sealFailCodec struct{ err error }

func (c sealFailCodec) Seal(context.Context, spine.Log, spine.Snapshot) (spine.Snapshot, error) {
	return spine.Snapshot{}, c.err
}

func (sealFailCodec) Open(_ context.Context, s spine.Snapshot) (spine.Snapshot, error) { return s, nil }

var _ spine.SnapshotCodec = sealFailCodec{}

// TestProjectRecordsRejectsIncompleteEvent: the projection is the read model, so
// an event missing the post-image it is supposed to carry must be refused rather
// than folded as a zero-valued record. Folding it would leave the live provider
// and a replayed one disagreeing about a record neither can produce.
func TestProjectRecordsRejectsIncompleteEvent(t *testing.T) {
	turn := &Turn{ID: "t1", SessionID: "s1"}

	for _, tc := range []struct {
		name    string
		evType  string
		records payloadRecords
		wantMsg string
	}{
		{name: "created session missing", evType: EvSessionCreated, wantMsg: `missing "session"`},
		{name: "deleted session missing", evType: EvSessionDeleted, wantMsg: `missing "session"`},
		{name: "turn missing", evType: EvTurnAppended, wantMsg: `missing "turn"`},
		{
			name:    "turn without its session",
			evType:  EvTurnAppended,
			records: payloadRecords{Turn: turn},
			wantMsg: `missing "session"`,
		},
		{name: "upserted skill missing", evType: EvSkillUpserted, wantMsg: `missing "skill"`},
		{name: "deleted skill missing", evType: EvSkillDeleted, wantMsg: `missing "skill"`},
		{name: "written item missing", evType: EvMemoryWritten, wantMsg: `missing "item"`},
		{name: "deleted item missing", evType: EvMemoryDeleted, wantMsg: `missing "item"`},
		{name: "pushed usage missing", evType: EvMemoryPushed, wantMsg: `missing "usage"`},
		{name: "used usage missing", evType: EvMemoryUsed, wantMsg: `missing "usage"`},
		{name: "unknown type", evType: "state.invented", wantMsg: `unknown event type`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCore(nil, nil)
			err := c.projectRecords(tc.evType, tc.records)
			if err == nil {
				t.Fatalf("event %q with no post-image was projected", tc.evType)
			}
			if got := err.Error(); !strings.Contains(got, tc.wantMsg) {
				t.Fatalf("error = %q, want it to report %q", got, tc.wantMsg)
			}
			if len(c.sessions) != 0 || len(c.turns) != 0 || len(c.skillsByID) != 0 || len(c.memItems) != 0 {
				t.Fatal("a rejected event still touched the read model")
			}
		})
	}
}

// TestApplyRejectsUndecodableRecord: a replayed event whose payload cannot be
// decoded into its typed record fails the fold. Replay must not paper over it: a
// provider rebuilt from a stream it could not fully decode would silently be
// missing state.
func TestApplyRejectsUndecodableRecord(t *testing.T) {
	for _, tc := range []struct {
		name    string
		evType  string
		payload map[string]any
	}{
		{name: "session", evType: EvSessionCreated, payload: map[string]any{keySession: "not a record"}},
		{name: "turn", evType: EvTurnAppended, payload: map[string]any{keyTurn: "not a record"}},
		{name: "skill", evType: EvSkillUpserted, payload: map[string]any{keySkill: "not a record"}},
		{name: "item", evType: EvMemoryWritten, payload: map[string]any{keyItem: "not a record"}},
		{name: "unmarshalable value", evType: EvMemoryWritten, payload: map[string]any{keyItem: math.NaN()}},
		{name: "usage", evType: EvMemoryPushed, payload: map[string]any{keyUsage: "not a record list"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCore(nil, nil)
			if err := c.apply(spine.Event{Type: tc.evType, Seq: 1, Payload: tc.payload}); err == nil {
				t.Fatal("an undecodable payload was applied to the read model")
			}
			if len(c.sessions) != 0 || len(c.skillsByID) != 0 || len(c.memItems) != 0 {
				t.Fatal("a rejected event still touched the read model")
			}
		})
	}
}

// TestDecodeRecordRejectsUnserializableValue: the exported decoders a durable
// backend projects with must refuse a payload value they cannot re-serialize,
// rather than handing back a zero-valued record that would diverge from the
// in-memory core's projection of the same event.
func TestDecodeRecordRejectsUnserializableValue(t *testing.T) {
	bad := math.Inf(1)

	if _, err := DecodeSession(map[string]any{keySession: bad}); err == nil {
		t.Fatal("DecodeSession accepted an unserializable value")
	}
	if _, err := DecodeTurn(map[string]any{keyTurn: bad}); err == nil {
		t.Fatal("DecodeTurn accepted an unserializable value")
	}
	if _, err := DecodeSkill(map[string]any{keySkill: bad}); err == nil {
		t.Fatal("DecodeSkill accepted an unserializable value")
	}
	if _, err := DecodeMemoryItem(map[string]any{keyItem: bad}); err == nil {
		t.Fatal("DecodeMemoryItem accepted an unserializable value")
	}
	if _, err := DecodeMemoryUsage(map[string]any{keyUsage: bad}); err == nil {
		t.Fatal("DecodeMemoryUsage accepted an unserializable value")
	}
	// An empty list is refused too. A push event that names no item is not a push,
	// and projecting it as a no-op would let a malformed stream fold clean.
	if _, err := DecodeMemoryUsage(map[string]any{keyUsage: []MemoryUsage{}}); err == nil {
		t.Fatal("DecodeMemoryUsage accepted an empty post-image list")
	}
}

// TestStamperInputRejectsUnencodablePayload: a state event whose payload cannot be
// serialized produces no AppendInput, so the mutation fails before anything is
// appended rather than recording an event the fold could not replay.
func TestStamperInputRejectsUnencodablePayload(t *testing.T) {
	p := NewMemory(WithInstanceID("node-3")).(*memProvider)
	st := p.core.st

	if got := st.InstanceID(); got != "node-3" {
		t.Fatalf("InstanceID = %q, want node-3", got)
	}
	in, err := st.input(EvMemoryWritten, map[string]any{keyItem: math.NaN()})
	if err == nil {
		t.Fatal("an unencodable payload produced an event")
	}
	if in.RawPayload != nil || in.Type != "" {
		t.Fatalf("a failed stamp still produced an event: %+v", in)
	}
}

// TestAutomaticSnapshotCadence: with a cadence set, the provider checkpoints every
// k mutations, so a Replay folds at most k events past the last checkpoint. Before
// the kth mutation there is nothing to resume from.
func TestAutomaticSnapshotCadence(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewManual(time.Unix(1_700_000_000, 0))
	log := spine.NewMemoryLog(spine.WithClock(clk))
	p := NewMemory(WithEventLog(log), WithClock(clk), WithSnapshotEvery(3))

	for i := range 2 {
		if _, err := p.Sessions().Create(ctx, Session{Title: "s"}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, found, err := log.LatestSnapshot(ctx, StateStream, 0); err != nil || found {
		t.Fatalf("checkpointed before the cadence was reached (found=%v err=%v)", found, err)
	}

	if _, err := p.Sessions().Create(ctx, Session{Title: "third"}); err != nil {
		t.Fatal(err)
	}
	snap, found, err := log.LatestSnapshot(ctx, StateStream, 0)
	if err != nil || !found {
		t.Fatalf("no checkpoint at the cadence (found=%v err=%v)", found, err)
	}
	if snap.Seq != 3 {
		t.Fatalf("checkpoint anchored at Seq %d, want 3 (the mutation that triggered it)", snap.Seq)
	}
	decoded, err := UnmarshalSnapshot(snap.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sessions) != 3 {
		t.Fatalf("checkpoint holds %d sessions, want 3", len(decoded.Sessions))
	}

	// The cadence restarts: the next two mutations do not checkpoint again.
	for range 2 {
		if _, err := p.Sessions().Create(ctx, Session{Title: "more"}); err != nil {
			t.Fatal(err)
		}
	}
	if snap, _, err := log.LatestSnapshot(ctx, StateStream, 0); err != nil || snap.Seq != 3 {
		t.Fatalf("checkpoint moved to Seq %d before the next cadence (err=%v)", snap.Seq, err)
	}
}

// TestSnapshotSealFailure: an explicit Snapshot whose codec cannot seal reports the
// failure (an unsealed snapshot must never be stored), while the same failure on
// the automatic cadence is swallowed: a checkpoint is a cache, so losing one may
// never fail the mutation that triggered it.
func TestSnapshotSealFailure(t *testing.T) {
	ctx := context.Background()
	codec := sealFailCodec{err: errLogDown}

	t.Run("explicit", func(t *testing.T) {
		p := NewMemory(WithSnapshotCodec(codec)).(*memProvider)
		if _, err := p.Sessions().Create(ctx, Session{Title: "s"}); err != nil {
			t.Fatal(err)
		}
		if err := p.Snapshot(ctx); !errors.Is(err, errLogDown) {
			t.Fatalf("Snapshot error = %v, want the seal failure", err)
		}
	})

	t.Run("automatic cadence", func(t *testing.T) {
		log := spine.NewMemoryLog()
		p := NewMemory(WithEventLog(log), WithSnapshotCodec(codec), WithSnapshotEvery(1))
		ses, err := p.Sessions().Create(ctx, Session{Title: "s"})
		if err != nil {
			t.Fatalf("a failed checkpoint failed the mutation that triggered it: %v", err)
		}
		if _, found, err := log.LatestSnapshot(ctx, StateStream, 0); err != nil || found {
			t.Fatalf("an unsealed snapshot was stored (found=%v err=%v)", found, err)
		}
		// The mutation itself is on the log, so the state is still a fold of it.
		rebuilt, err := Replay(ctx, log)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rebuilt.Sessions().Get(ctx, ses.ID); err != nil {
			t.Fatalf("the mutation behind the failed checkpoint was lost: %v", err)
		}
	})
}

// TestReplayReportsLogFailures: a Replay that cannot read the snapshot index or the
// stream fails loudly. Returning a partially-folded provider would look healthy
// while silently missing state, which is the one outcome an event-sourced rebuild
// may not produce.
func TestReplayReportsLogFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("snapshot index", func(t *testing.T) {
		log := breakingLog{Log: spine.NewMemoryLog(), snapErr: errLogDown}
		if _, err := Replay(ctx, log); !errors.Is(err, errLogDown) {
			t.Fatalf("Replay error = %v, want the snapshot read failure", err)
		}
	})

	t.Run("stream", func(t *testing.T) {
		log := breakingLog{Log: spine.NewMemoryLog(), readErr: errLogDown}
		if _, err := Replay(ctx, log); !errors.Is(err, errLogDown) {
			t.Fatalf("Replay error = %v, want the stream read failure", err)
		}
	})

	t.Run("event the fold cannot apply", func(t *testing.T) {
		log := spine.NewMemoryLog()
		p := NewMemory(WithEventLog(log))
		if _, err := p.Sessions().Create(ctx, Session{Title: "s"}); err != nil {
			t.Fatal(err)
		}
		// An event of a type the projection does not know, appended straight onto the
		// state stream: the fold must refuse it rather than skip it.
		if _, err := log.Append(ctx, envelope.EventInput(StateStream, "state.invented", spine.ActorAgent, "local", []byte(`{}`))); err != nil {
			t.Fatal(err)
		}
		if _, err := Replay(ctx, log); err == nil {
			t.Fatal("Replay folded a stream carrying an event it cannot project")
		}
	})
}

// TestAppendTurnKeepsAnExplicitCreatedAt: a turn that already carries a creation
// time (a turn replicated from another instance, or one a host stamps itself)
// keeps it, and the session's UpdatedAt is bumped to that same time rather than to
// the local clock, so the session and its transcript never disagree.
func TestAppendTurnKeepsAnExplicitCreatedAt(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewManual(time.Unix(1_700_000_000, 0))
	p := NewMemory(WithClock(clk))

	ses, err := p.Sessions().Create(ctx, Session{Title: "s"})
	if err != nil {
		t.Fatal(err)
	}
	stamped := clk.Now().Add(-time.Hour)
	turn, err := p.Sessions().AppendTurn(ctx, Turn{SessionID: ses.ID, Role: "user", Content: "hi", CreatedAt: stamped})
	if err != nil {
		t.Fatal(err)
	}
	if !turn.CreatedAt.Equal(stamped) {
		t.Fatalf("turn CreatedAt = %v, want the supplied %v", turn.CreatedAt, stamped)
	}
	got, err := p.Sessions().Get(ctx, ses.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.Equal(stamped) {
		t.Fatalf("session UpdatedAt = %v, want the turn's own time %v", got.UpdatedAt, stamped)
	}
}

// TestRecallNarrowsToItsScope: recall is scoped, so a query in one project never
// surfaces another project's memory, whatever the query text matches.
func TestRecallNarrowsToItsScope(t *testing.T) {
	ctx := context.Background()
	p := NewMemory()
	mine := Scope{Project: "alpha"}
	theirs := Scope{Project: "beta"}

	if _, err := p.Memory().Write(ctx, MemoryItem{Kind: "fact", Content: "shared secret", Scope: mine}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Memory().Write(ctx, MemoryItem{Kind: "fact", Content: "shared secret", Scope: theirs}); err != nil {
		t.Fatal(err)
	}

	got, err := p.Memory().Recall(ctx, RecallQuery{Query: "secret", Scope: mine})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Scope != mine {
		t.Fatalf("recall in %+v returned %d items across scopes: %+v", mine, len(got), got)
	}
	all, err := p.Memory().Recall(ctx, RecallQuery{Query: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("an unscoped recall returned %d items, want both", len(all))
	}
}

// TestRecallCapsAtItsLimit: a limited recall returns the head of the same total
// order it would otherwise return, so prefetching into a bounded context window is
// deterministic rather than dependent on map iteration.
func TestRecallCapsAtItsLimit(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewManual(time.Unix(1_700_000_000, 0))
	p := NewMemory(WithClock(clk))

	// Written at the same instant, so the order is decided by the ID tiebreak.
	for _, content := range []string{"fact one", "fact two", "fact three"} {
		if _, err := p.Memory().Write(ctx, MemoryItem{Kind: "fact", Content: content}); err != nil {
			t.Fatal(err)
		}
	}

	full, err := p.Memory().Recall(ctx, RecallQuery{Query: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 3 {
		t.Fatalf("unlimited recall returned %d items, want 3", len(full))
	}
	capped, err := p.Memory().Recall(ctx, RecallQuery{Query: "fact", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 2 {
		t.Fatalf("limited recall returned %d items, want 2", len(capped))
	}
	for i, it := range capped {
		if it.ID != full[i].ID {
			t.Fatalf("limited recall item %d = %q, want the head of the full order (%q)", i, it.ID, full[i].ID)
		}
	}
}
