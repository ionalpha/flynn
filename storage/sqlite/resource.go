package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// resourceCols matches the resources table column order (and the scanResource scan
// order); keep them in lockstep with migration 0003.
const resourceCols = `id, api_version, kind, name, scope_instance, scope_project, scope_workspace,
	labels, annotations, spec, status,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, writer_actor, deleted,
	finalizers, deletion_timestamp, owner_references,
	version, content_hash, spec_hash, valid_from, valid_to, created_at, updated_at`

// Resources returns a durable resource.Store backed by this Store's database, so
// resource events share the spine, the file, and a transaction with state. The
// returned store admits writes against reg.
func (s *Store) Resources(reg *resource.Registry) resource.Store {
	return &resourceStore{p: s, st: resource.NewStamper(s.instanceID, s.clk, s.hlc, s.gen, reg)}
}

type resourceStore struct {
	p  *Store
	st *resource.Stamper
}

var _ resource.Store = (*resourceStore)(nil)

// Close closes the shared database (the resource store and the Store share it).
func (s *resourceStore) Close() error { return s.p.Close() }

// Snapshot implements resource.Store: it checkpoints every projected resource (live
// and tombstoned) onto the event log, anchored at the resource stream's head Seq,
// so a Replay resumes from the snapshot and folds only the events after it instead
// of replaying the whole stream.
func (s *resourceStore) Snapshot(ctx context.Context) error {
	rows, err := s.p.reads().QueryContext(ctx, `SELECT `+resourceCols+` FROM resources ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var all []resource.Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var lastSeq int64
	if err := s.p.reads().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE stream = ?`, resource.ResourceStream).Scan(&lastSeq); err != nil {
		return err
	}

	payload, err := resource.MarshalSnapshot(all, lastSeq)
	if err != nil {
		return err
	}
	snap := spine.Snapshot{Stream: resource.ResourceStream, Seq: lastSeq, Payload: payload}
	if s.p.snapCodec != nil {
		if snap, err = s.p.snapCodec.Seal(ctx, s.p.Log(), snap); err != nil {
			return err
		}
	}
	return s.p.Log().SaveSnapshot(ctx, snap)
}

// maybeSnapshot counts one committed mutation toward the automatic snapshot
// cadence and checkpoints the stream when the cadence is reached. It runs after
// the mutation's transaction commits and best effort: a snapshot failure never
// fails the write it followed.
func (s *resourceStore) maybeSnapshot(ctx context.Context) {
	if s.p.snapEvery <= 0 {
		return
	}
	if s.p.snapPending.Add(1) < int64(s.p.snapEvery) {
		return
	}
	s.p.snapPending.Store(0)
	_ = s.Snapshot(ctx)
}

// Log exposes the shared spine so the resource stream can be observed or folded;
// the event-sourced capability the conformance suite checks.
func (s *resourceStore) Log() spine.Log { return s.p.Log() }

// commit runs one resource mutation through the command path: build stamps the
// record and produces the event (doing its tx-scoped lookup for CAS), then the
// event is appended to the spine and the record projected into the table, both
// in one tx. The projection writes the typed record the Stamper returned (the
// same post-image the event payload carries), so the live path never decodes
// what it just encoded; applyResourceEvent performs the identical projection
// from the payload during Rebuild.
func (s *resourceStore) commit(ctx context.Context, build func(tx *sql.Tx) (resource.Resource, spine.AppendInput, error)) error {
	err := s.p.tx(ctx, func(tx *sql.Tx) error {
		rec, in, err := build(tx)
		if err != nil {
			return err
		}
		if _, _, _, err := insertEventTx(ctx, tx, s.p, in); err != nil {
			return err
		}
		return upsertResourceRow(ctx, tx, s.p, rec)
	})
	if err == nil {
		s.maybeSnapshot(ctx)
	}
	return err
}

// Rebuild reprojects the resources table from the resource event stream, the proof
// the log is authoritative; idempotent (every event is a post-image applied by id).
// It resumes from the stream's latest usable snapshot and folds only the events
// after it, so a rebuild stays bounded as the stream grows; a snapshot that cannot
// be verified (with a codec set) or decoded is skipped and the whole stream is
// folded instead - only slower, never wrong. The full fold from the start remains
// the deep audit path: rebuild with no snapshot stored and compare.
func (s *resourceStore) Rebuild(ctx context.Context) error {
	restored, afterSeq := s.snapshotForRebuild(ctx)
	events, err := s.p.Log().Read(ctx, spine.Query{Stream: resource.ResourceStream, AfterSeq: afterSeq})
	if err != nil {
		return err
	}
	return s.p.tx(ctx, func(tx *sql.Tx) error {
		for _, r := range restored {
			if err := upsertResourceRow(ctx, tx, s.p, r); err != nil {
				return err
			}
		}
		for _, e := range events {
			if err := applyResourceEvent(ctx, tx, s.p, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// snapshotForRebuild returns the records and seq of the latest usable resource
// snapshot, or (nil, 0) to fold the whole stream: no snapshot, one the codec
// rejects, or one that does not decode all fall back the same way. With a codec
// set, a snapshot that fails verification is never restored - the fallback is the
// fail-closed path, not an error.
func (s *resourceStore) snapshotForRebuild(ctx context.Context) ([]resource.Resource, int64) {
	snap, found, err := s.p.Log().LatestSnapshot(ctx, resource.ResourceStream, 0)
	if err != nil || !found {
		return nil, 0
	}
	if s.p.snapCodec != nil {
		if snap, err = s.p.snapCodec.Open(ctx, snap); err != nil {
			return nil, 0
		}
	}
	restored, lastSeq, err := resource.UnmarshalSnapshot(snap.Payload)
	if err != nil {
		return nil, 0
	}
	return restored, lastSeq
}

func (s *resourceStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	var rec resource.Resource
	err := s.commit(ctx, func(tx *sql.Tx) (resource.Resource, spine.AppendInput, error) {
		existing, err := getResourceByKeyTx(ctx, tx, s.p, r.Kind, r.Scope, r.Name)
		if err != nil {
			return resource.Resource{}, spine.AppendInput{}, err
		}
		rc, ev, err := s.st.Put(existing, r)
		rec = rc
		return rc, ev, err
	})
	if err != nil {
		return resource.Resource{}, err
	}
	return rec, nil
}

func (s *resourceStore) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	row := s.p.reads().QueryRowContext(ctx,
		`SELECT `+resourceCols+` FROM resources
		 WHERE kind = ? AND scope_instance = ? AND scope_project = ? AND scope_workspace = ? AND name = ? AND deleted = 0`,
		kind, scope.Instance, scope.Project, scope.Workspace, name)
	r, err := scanResource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return resource.Resource{}, resource.ErrNotFound
	}
	return r, err
}

func (s *resourceStore) GetByID(ctx context.Context, id string) (resource.Resource, error) {
	row := s.p.reads().QueryRowContext(ctx, `SELECT `+resourceCols+` FROM resources WHERE id = ? AND deleted = 0`, id)
	r, err := scanResource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return resource.Resource{}, resource.ErrNotFound
	}
	return r, err
}

func (s *resourceStore) List(ctx context.Context, kind string, scope resource.Scope, sel resource.Selector) ([]resource.Resource, error) {
	rows, err := s.p.reads().QueryContext(ctx,
		`SELECT `+resourceCols+` FROM resources
		 WHERE kind = ? AND scope_instance = ? AND scope_project = ? AND scope_workspace = ? AND deleted = 0
		 ORDER BY name, id`,
		kind, scope.Instance, scope.Project, scope.Workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]resource.Resource, 0)
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		// Label selectors are matched in Go over the decoded labels; the SQL narrows
		// to (kind, scope) first. A labels index can optimize this later.
		if sel.Matches(r.Labels) {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

func (s *resourceStore) ListAll(ctx context.Context, kind string, sel resource.Selector) ([]resource.Resource, error) {
	rows, err := s.p.reads().QueryContext(ctx,
		`SELECT `+resourceCols+` FROM resources
		 WHERE kind = ? AND deleted = 0
		 ORDER BY scope_instance, scope_project, scope_workspace, name, id`,
		kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]resource.Resource, 0)
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		if sel.Matches(r.Labels) {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// ListKeys is the resource.KeyLister capability: the keys of every live resource
// of a kind, reading only the address columns so a resync sweep never decodes
// record payloads.
func (s *resourceStore) ListKeys(ctx context.Context, kind string) ([]resource.Key, error) {
	rows, err := s.p.reads().QueryContext(ctx,
		`SELECT scope_instance, scope_project, scope_workspace, name FROM resources
		 WHERE kind = ? AND deleted = 0
		 ORDER BY scope_instance, scope_project, scope_workspace, name`,
		kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []resource.Key
	for rows.Next() {
		k := resource.Key{Kind: kind}
		if err := rows.Scan(&k.Scope.Instance, &k.Scope.Project, &k.Scope.Workspace, &k.Name); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// errAlreadyTerminating is an internal sentinel: deleting a resource that already
// has a DeletionTimestamp is an idempotent no-op, so the command path rolls back
// (writes nothing) and Delete reports success.
var errAlreadyTerminating = errors.New("sqlite: resource already terminating")

func (s *resourceStore) Delete(ctx context.Context, kind string, scope resource.Scope, name string) error {
	err := s.commit(ctx, func(tx *sql.Tx) (resource.Resource, spine.AppendInput, error) {
		existing, err := getResourceByKeyTx(ctx, tx, s.p, kind, scope, name)
		if err != nil {
			return resource.Resource{}, spine.AppendInput{}, err
		}
		if existing == nil || existing.Deleted {
			return resource.Resource{}, spine.AppendInput{}, resource.ErrNotFound
		}
		if existing.DeletionTimestamp != nil {
			return resource.Resource{}, spine.AppendInput{}, errAlreadyTerminating
		}
		return s.st.Delete(*existing)
	})
	if errors.Is(err, errAlreadyTerminating) {
		return nil
	}
	return err
}

func (s *resourceStore) Merge(ctx context.Context, remote resource.Resource) (resource.MergeResult, error) {
	if err := resource.ValidateForMerge(remote); err != nil {
		return resource.MergeResult{}, err
	}
	if err := s.st.Registry().Validate(remote.APIVersion, remote.Kind, remote.Spec); err != nil {
		return resource.MergeResult{}, err
	}
	var res resource.MergeResult
	err := s.p.tx(ctx, func(tx *sql.Tx) error {
		current, err := getResourceByIDTx(ctx, tx, remote.ID)
		if err != nil {
			return err
		}
		if current == nil {
			res = resource.MergeResult{Outcome: resource.MergeApplied, Resource: remote}
			return appendMergeEvent(ctx, tx, s.p, remote)
		}
		winner, take := resource.Resolve(remote, *current)
		if !take {
			out := resource.MergeUnchanged
			if winner.UpdatedHLC != remote.UpdatedHLC || winner.LastWriterID != remote.LastWriterID {
				out = resource.MergeIgnored
			}
			res = resource.MergeResult{Outcome: out, Resource: *current}
			return nil
		}
		res = resource.MergeResult{Outcome: resource.MergeApplied, Resource: winner}
		return appendMergeEvent(ctx, tx, s.p, winner)
	})
	if err != nil {
		return resource.MergeResult{}, err
	}
	if res.Outcome == resource.MergeApplied {
		s.maybeSnapshot(ctx)
	}
	return res, nil
}

// appendMergeEvent records a merge post-image on the spine and projects it, both in
// tx, so a replicated record lands on the log and into the table exactly like a
// local write while keeping the remote envelope verbatim.
func appendMergeEvent(ctx context.Context, tx *sql.Tx, p *Store, r resource.Resource) error {
	in, err := resource.MergeEvent(r)
	if err != nil {
		return err
	}
	if _, _, _, err := insertEventTx(ctx, tx, p, in); err != nil {
		return err
	}
	return upsertResourceRow(ctx, tx, p, r)
}

// getResourceByIDTx loads the stored resource by id within tx, tombstones included
// (merge resolves against the live-or-tombstoned record), or nil when none exists.
func getResourceByIDTx(ctx context.Context, tx *sql.Tx, id string) (*resource.Resource, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+resourceCols+` FROM resources WHERE id = ?`, id)
	r, err := scanResource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// resourceByKeySQL is the CAS lookup on every resource Put/Delete, prepared at
// Open (stmts.resourceKeyTx). Tombstones included, so a put can resurrect one.
const resourceByKeySQL = `SELECT ` + resourceCols + ` FROM resources
	WHERE kind = ? AND scope_instance = ? AND scope_project = ? AND scope_workspace = ? AND name = ?`

// getResourceByKeyTx loads the stored resource for (kind, scope, name) within tx,
// tombstones included (so a put can resurrect it), or nil when none exists.
func getResourceByKeyTx(ctx context.Context, tx *sql.Tx, p *Store, kind string, scope resource.Scope, name string) (*resource.Resource, error) {
	row := tx.StmtContext(ctx, p.stmts.resourceKeyTx).QueryRowContext(ctx,
		kind, scope.Instance, scope.Project, scope.Workspace, name)
	r, err := scanResource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// applyResourceEvent projects one resource event into the table. Shared by the live
// write path (commit) and Rebuild, so a rebuilt table equals a live one.
func applyResourceEvent(ctx context.Context, tx *sql.Tx, p *Store, e spine.Event) error {
	switch e.Type {
	case resource.EvPut, resource.EvDeleted, resource.EvMerged:
		r, err := resource.DecodeResource(e.Payload)
		if err != nil {
			return err
		}
		return upsertResourceRow(ctx, tx, p, r)
	default:
		return fmt.Errorf("sqlite: unknown resource event %q", e.Type)
	}
}

const upsertResourceSQL = `INSERT INTO resources (` + resourceCols + `)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET
		api_version=excluded.api_version, kind=excluded.kind, name=excluded.name,
		scope_instance=excluded.scope_instance, scope_project=excluded.scope_project, scope_workspace=excluded.scope_workspace,
		labels=excluded.labels, annotations=excluded.annotations, spec=excluded.spec, status=excluded.status,
		sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
		updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
		last_writer_id=excluded.last_writer_id, writer_actor=excluded.writer_actor, deleted=excluded.deleted,
		finalizers=excluded.finalizers, deletion_timestamp=excluded.deletion_timestamp, owner_references=excluded.owner_references,
		version=excluded.version, content_hash=excluded.content_hash, spec_hash=excluded.spec_hash,
		valid_from=excluded.valid_from, valid_to=excluded.valid_to,
		created_at=excluded.created_at, updated_at=excluded.updated_at`

func upsertResourceRow(ctx context.Context, tx *sql.Tx, p *Store, r resource.Resource) error {
	_, err := tx.StmtContext(ctx, p.stmts.resourceUpsert).ExecContext(ctx,
		r.ID, r.APIVersion, r.Kind, r.Name, r.Scope.Instance, r.Scope.Project, r.Scope.Workspace,
		marshalStringMap(r.Labels), marshalStringMap(r.Annotations), rawOrNil(r.Spec), rawOrNil(r.Status),
		r.SyncVersion, r.OriginInstanceID, r.UpdatedHLC.Wall, int64(r.UpdatedHLC.Counter), r.LastWriterID, string(writerActorOrDefault(r.WriterActor)), boolToInt(r.Deleted),
		marshalStringSlice(r.Finalizers), timeOrNil(r.DeletionTimestamp), marshalOwnerRefs(r.OwnerReferences),
		r.Version, r.ContentHash, r.SpecHash, timeOrNil(r.ValidFrom), timeOrNil(r.ValidTo),
		formatTime(r.CreatedAt), formatTime(r.UpdatedAt))
	return err
}

func scanResource(sc interface{ Scan(...any) error }) (resource.Resource, error) {
	var (
		r                resource.Resource
		labels, annots   string
		spec, status     sql.NullString
		wall, counter    int64
		writerActor      string
		deleted          int
		finalizers       string
		deletionTS       sql.NullString
		ownerRefs        string
		validFrom        sql.NullString
		validTo          sql.NullString
		created, updated string
	)
	if err := sc.Scan(&r.ID, &r.APIVersion, &r.Kind, &r.Name,
		&r.Scope.Instance, &r.Scope.Project, &r.Scope.Workspace,
		&labels, &annots, &spec, &status,
		&r.SyncVersion, &r.OriginInstanceID, &wall, &counter, &r.LastWriterID, &writerActor, &deleted,
		&finalizers, &deletionTS, &ownerRefs,
		&r.Version, &r.ContentHash, &r.SpecHash, &validFrom, &validTo, &created, &updated); err != nil {
		return resource.Resource{}, err
	}
	r.WriterActor = spine.ActorType(writerActor)
	r.Finalizers = unmarshalStringSlice(finalizers)
	r.DeletionTimestamp = nullToTimePtr(deletionTS)
	r.OwnerReferences = unmarshalOwnerRefs(ownerRefs)
	r.Labels = unmarshalStringMap(labels)
	r.Annotations = unmarshalStringMap(annots)
	if spec.Valid {
		r.Spec = json.RawMessage(spec.String)
	}
	if status.Valid {
		r.Status = json.RawMessage(status.String)
	}
	r.UpdatedHLC = hlcTime(wall, counter)
	r.Deleted = deleted != 0
	r.ValidFrom = nullToTimePtr(validFrom)
	r.ValidTo = nullToTimePtr(validTo)
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	return r, nil
}

// --- value helpers ----------------------------------------------------------

func marshalStringMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func unmarshalStringMap(s string) map[string]string {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

func marshalStringSlice(s []string) string {
	if len(s) == 0 {
		return "[]"
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalStringSlice(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func marshalOwnerRefs(refs []resource.OwnerReference) string {
	if len(refs) == 0 {
		return "[]"
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalOwnerRefs(s string) []resource.OwnerReference {
	if s == "" || s == "[]" {
		return nil
	}
	var out []resource.OwnerReference
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// rawOrNil returns nil (SQL NULL) for empty raw JSON, else the JSON text.
func rawOrNil(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// timeOrNil returns nil (SQL NULL) for a nil time, else its canonical string.
func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// writerActorOrDefault normalizes a provenance actor for storage, defaulting the
// zero value to the agent so the column never holds an empty string.
func writerActorOrDefault(a spine.ActorType) spine.ActorType {
	if a == "" {
		return spine.ActorAgent
	}
	return a
}

func nullToTimePtr(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	return &t
}
