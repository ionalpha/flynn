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

	rows, err := s.db.QueryContext(ctx,
		`EXPLAIN QUERY PLAN
		 SELECT scope_instance, scope_project, scope_workspace, name FROM resources
		 WHERE kind = ? AND deleted = 0
		 ORDER BY scope_instance, scope_project, scope_workspace, name`,
		"Widget")
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

	plan := strings.Join(details, "; ")
	if !strings.Contains(plan, "USING COVERING INDEX idx_resources_live_keys") {
		t.Fatalf("ListKeys is not a covering scan of idx_resources_live_keys; plan: %s", plan)
	}
	// A covering scan in index order needs no sort pass; a TEMP B-TREE in the plan
	// means the ORDER BY stopped matching the index.
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("ListKeys plan sorts through a temp b-tree instead of reading in index order; plan: %s", plan)
	}
}
