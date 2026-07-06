package sqlite

import (
	"context"
	"strings"
	"testing"
)

// TestListKeysCoveringIndex is the query-plan gate for the key-only resync read:
// ListKeys must be answered entirely from idx_resources_live_keys (a covering
// scan), never by scanning the table or probing rows for deleted. If the plan
// stops saying COVERING INDEX, the index no longer matches the query (a column
// was added to the SELECT, the predicate changed, or the migration was dropped)
// and the resync read is paying a per-row table probe again.
func TestListKeysCoveringIndex(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	plan := explainPlan(ctx, t, s,
		`SELECT scope_instance, scope_project, scope_workspace, name FROM resources
		 WHERE kind = ? AND deleted = 0
		 ORDER BY scope_instance, scope_project, scope_workspace, name`,
		"Widget")
	if !strings.Contains(plan, "USING COVERING INDEX idx_resources_live_keys") {
		t.Fatalf("ListKeys is not a covering scan of idx_resources_live_keys; plan: %s", plan)
	}
	// A covering scan in index order needs no sort pass; a TEMP B-TREE in the plan
	// means the ORDER BY stopped matching the index.
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("ListKeys plan sorts through a temp b-tree instead of reading in index order; plan: %s", plan)
	}
}

// TestScopedRecallIndex is the query-plan gate for the agent-startup memory read:
// scoped recall must be an index range scan of idx_memory_items_live_scope,
// returning rows already in (created_at DESC, id DESC) order, never a full table
// scan with a sort pass (memory_items previously had only its id primary key).
func TestScopedRecallIndex(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	plan := explainPlan(ctx, t, s,
		`SELECT `+memoryCols+` FROM memory_items m
		 WHERE m.deleted = 0 AND m.scope_instance = ? AND m.scope_project = ? AND m.scope_workspace = ?
		 ORDER BY m.created_at DESC, m.id DESC LIMIT ?`,
		"i", "p", "w", -1)
	if !strings.Contains(plan, "USING INDEX idx_memory_items_live_scope") {
		t.Fatalf("scoped recall does not use idx_memory_items_live_scope; plan: %s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("scoped recall sorts through a temp b-tree instead of reading in index order; plan: %s", plan)
	}
}

// TestSkillSlugIndex is the query-plan gate for the bare-slug skill lookup: the
// slug fallback in Get must seek idx_skills_live_slug (slug had no index, so the
// lookup was a table scan) and read its single row without a sort pass.
func TestSkillSlugIndex(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	plan := explainPlan(ctx, t, s,
		`SELECT `+skillCols+` FROM skills WHERE slug = ? AND deleted = 0 ORDER BY created_at, id LIMIT 1`,
		"a-slug")
	if !strings.Contains(plan, "USING INDEX idx_skills_live_slug") {
		t.Fatalf("slug lookup does not use idx_skills_live_slug; plan: %s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("slug lookup sorts through a temp b-tree instead of reading in index order; plan: %s", plan)
	}
}

// TestSealedBlobsIndex is the query-plan gate for the tiered-storage archival
// sweep: the correlated NOT EXISTS that names an unsealed reference must seek
// idx_events_payload_blob, never scan the events log. events carries only its
// (stream, seq) primary key, so before the index the probe was a full table scan
// per distinct hot blob (O(distinct_blobs x total_events), growing with the log).
func TestSealedBlobsIndex(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	plan := explainPlan(ctx, t, s, sealedBlobsSelectSQL)
	if !strings.Contains(plan, "USING INDEX idx_events_payload_blob") {
		t.Fatalf("sealed-blobs sweep does not seek idx_events_payload_blob; plan: %s", plan)
	}
	if strings.Contains(plan, "SCAN e") {
		t.Fatalf("sealed-blobs sweep scans the events table instead of probing the blob index; plan: %s", plan)
	}
}

// TestAnyScopeGetIndex is the query-plan gate for the cross-scope by-name lookup
// (resource.AnyScopeGetter): GetAnyScope must seek idx_resources_kind_name and read its
// single row, never scan the kind. The live-keys index leads kind then the scope columns
// before name, so without this index the (kind, name) query seeks by kind and then scans
// every live row of the kind to test name.
func TestAnyScopeGetIndex(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	plan := explainPlan(ctx, t, s,
		`SELECT `+resourceCols+` FROM resources
		 WHERE kind = ? AND name = ? AND deleted = 0
		 ORDER BY scope_instance, scope_project, scope_workspace, id
		 LIMIT 1`,
		"Widget", "w-1")
	if !strings.Contains(plan, "USING INDEX idx_resources_kind_name") {
		t.Fatalf("GetAnyScope does not seek idx_resources_kind_name; plan: %s", plan)
	}
	if strings.Contains(plan, "SCAN resources") {
		t.Fatalf("GetAnyScope scans the resources table instead of seeking the (kind, name) index; plan: %s", plan)
	}
}

// explainPlan runs EXPLAIN QUERY PLAN over query and joins the detail rows, the
// shared harness of the plan-shape gates above.
func explainPlan(ctx context.Context, t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "; ")
}
