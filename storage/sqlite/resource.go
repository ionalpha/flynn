package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// resourceCols matches the resources table column order (and the scanResource scan
// order); keep them in lockstep with migration 0003.
const resourceCols = `id, api_version, kind, name, scope_instance, scope_project, scope_workspace,
	labels, annotations, spec, status,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, writer_actor, deleted,
	version, content_hash, valid_from, valid_to, created_at, updated_at`

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

// Log exposes the shared spine so the resource stream can be observed or folded;
// the event-sourced capability the conformance suite checks.
func (s *resourceStore) Log() spine.Log { return s.p.Log() }

// commit runs one resource mutation through the command path: build stamps the
// record and produces the event (doing its tx-scoped lookup for CAS), then the
// event is appended to the spine and projected into the table, both in one tx.
func (s *resourceStore) commit(ctx context.Context, build func(tx *sql.Tx) (spine.AppendInput, error)) error {
	return s.p.tx(ctx, func(tx *sql.Tx) error {
		in, err := build(tx)
		if err != nil {
			return err
		}
		e, err := appendTx(ctx, tx, s.p.clk, in)
		if err != nil {
			return err
		}
		return applyResourceEvent(ctx, tx, e)
	})
}

// Rebuild reprojects the resources table from the resource event stream, the proof
// the log is authoritative; idempotent (every event is a post-image applied by id).
func (s *resourceStore) Rebuild(ctx context.Context) error {
	events, err := s.p.Log().Read(ctx, spine.Query{Stream: resource.ResourceStream})
	if err != nil {
		return err
	}
	return s.p.tx(ctx, func(tx *sql.Tx) error {
		for _, e := range events {
			if err := applyResourceEvent(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *resourceStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	var rec resource.Resource
	err := s.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, error) {
		existing, err := getResourceByKeyTx(ctx, tx, r.Kind, r.Scope, r.Name)
		if err != nil {
			return spine.AppendInput{}, err
		}
		rc, ev, err := s.st.Put(existing, r)
		rec = rc
		return ev, err
	})
	if err != nil {
		return resource.Resource{}, err
	}
	return rec, nil
}

func (s *resourceStore) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	row := s.p.db.QueryRowContext(ctx,
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
	row := s.p.db.QueryRowContext(ctx, `SELECT `+resourceCols+` FROM resources WHERE id = ? AND deleted = 0`, id)
	r, err := scanResource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return resource.Resource{}, resource.ErrNotFound
	}
	return r, err
}

func (s *resourceStore) List(ctx context.Context, kind string, scope resource.Scope, sel resource.Selector) ([]resource.Resource, error) {
	rows, err := s.p.db.QueryContext(ctx,
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
	rows, err := s.p.db.QueryContext(ctx,
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

func (s *resourceStore) Delete(ctx context.Context, kind string, scope resource.Scope, name string) error {
	return s.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, error) {
		existing, err := getResourceByKeyTx(ctx, tx, kind, scope, name)
		if err != nil {
			return spine.AppendInput{}, err
		}
		if existing == nil || existing.Deleted {
			return spine.AppendInput{}, resource.ErrNotFound
		}
		_, ev, err := s.st.Delete(*existing)
		return ev, err
	})
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
			return appendMergeEvent(ctx, tx, s.p.clk, remote)
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
		return appendMergeEvent(ctx, tx, s.p.clk, winner)
	})
	if err != nil {
		return resource.MergeResult{}, err
	}
	return res, nil
}

// appendMergeEvent records a merge post-image on the spine and projects it, both in
// tx, so a replicated record lands on the log and into the table exactly like a
// local write while keeping the remote envelope verbatim.
func appendMergeEvent(ctx context.Context, tx *sql.Tx, clk clock.Clock, r resource.Resource) error {
	in, err := resource.MergeEvent(r)
	if err != nil {
		return err
	}
	e, err := appendTx(ctx, tx, clk, in)
	if err != nil {
		return err
	}
	return applyResourceEvent(ctx, tx, e)
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

// getResourceByKeyTx loads the stored resource for (kind, scope, name) within tx,
// tombstones included (so a put can resurrect it), or nil when none exists.
func getResourceByKeyTx(ctx context.Context, tx *sql.Tx, kind string, scope resource.Scope, name string) (*resource.Resource, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+resourceCols+` FROM resources
		 WHERE kind = ? AND scope_instance = ? AND scope_project = ? AND scope_workspace = ? AND name = ?`,
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
func applyResourceEvent(ctx context.Context, tx *sql.Tx, e spine.Event) error {
	switch e.Type {
	case resource.EvPut, resource.EvDeleted, resource.EvMerged:
		r, err := resource.DecodeResource(e.Payload)
		if err != nil {
			return err
		}
		return upsertResourceRow(ctx, tx, r)
	default:
		return fmt.Errorf("sqlite: unknown resource event %q", e.Type)
	}
}

func upsertResourceRow(ctx context.Context, tx *sql.Tx, r resource.Resource) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO resources (`+resourceCols+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
			api_version=excluded.api_version, kind=excluded.kind, name=excluded.name,
			scope_instance=excluded.scope_instance, scope_project=excluded.scope_project, scope_workspace=excluded.scope_workspace,
			labels=excluded.labels, annotations=excluded.annotations, spec=excluded.spec, status=excluded.status,
			sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id, writer_actor=excluded.writer_actor, deleted=excluded.deleted,
			version=excluded.version, content_hash=excluded.content_hash,
			valid_from=excluded.valid_from, valid_to=excluded.valid_to,
			created_at=excluded.created_at, updated_at=excluded.updated_at`,
		r.ID, r.APIVersion, r.Kind, r.Name, r.Scope.Instance, r.Scope.Project, r.Scope.Workspace,
		marshalStringMap(r.Labels), marshalStringMap(r.Annotations), rawOrNil(r.Spec), rawOrNil(r.Status),
		r.SyncVersion, r.OriginInstanceID, r.UpdatedHLC.Wall, int64(r.UpdatedHLC.Counter), r.LastWriterID, string(writerActorOrDefault(r.WriterActor)), boolToInt(r.Deleted),
		r.Version, r.ContentHash, timeOrNil(r.ValidFrom), timeOrNil(r.ValidTo),
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
		validFrom        sql.NullString
		validTo          sql.NullString
		created, updated string
	)
	if err := sc.Scan(&r.ID, &r.APIVersion, &r.Kind, &r.Name,
		&r.Scope.Instance, &r.Scope.Project, &r.Scope.Workspace,
		&labels, &annots, &spec, &status,
		&r.SyncVersion, &r.OriginInstanceID, &wall, &counter, &r.LastWriterID, &writerActor, &deleted,
		&r.Version, &r.ContentHash, &validFrom, &validTo, &created, &updated); err != nil {
		return resource.Resource{}, err
	}
	r.WriterActor = spine.ActorType(writerActor)
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
