package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

type memory struct{ p *Store }

const memoryCols = `id, kind, content, subject, supersedes, scope_instance, scope_project, scope_workspace, sources, anchors, created_at, expires_at, tainted,
	sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id, deleted`

// memoryColsQualified is memoryCols against the `m` alias, for the recall query,
// which joins the FTS table and so cannot use bare column names. Recall appends
// one more expression, the relevance score, after these nineteen.
const memoryColsQualified = `m.id, m.kind, m.content, m.subject, m.supersedes, m.scope_instance, m.scope_project, m.scope_workspace, m.sources, m.anchors, m.created_at, m.expires_at, m.tainted,
	m.sync_version, m.origin_instance_id, m.updated_hlc_wall, m.updated_hlc_counter, m.last_writer_id, m.deleted`

// memoryScoreCol is the ordinal of the relevance score in a recall's select list,
// which ORDER BY names rather than repeating the bm25 expression, so it is
// evaluated once per row. It tracks the column count in memoryColsQualified.
const memoryScoreCol = 20

// memoryLiveSQL is the predicate for a row a recall may return: not tombstoned,
// and not past its expiry as of the bound instant. Both recall shapes use it, so
// the prepared single-scope statement and the built query cannot disagree on what
// "live" means. It binds one parameter, the read time in unix nanoseconds.
const memoryLiveSQL = `m.deleted = 0 AND (m.expires_at = 0 OR m.expires_at > ?)`

// encodeSources renders provenance for storage as a JSON array. Empty provenance
// is the empty string rather than "[]" so an item that never had a source keeps a
// cheap, obviously-empty column value.
//
// It returns no error because it cannot fail: encoding a []string is total, so
// dropping the error is honest, where a defensive error return would add a branch
// no test could ever reach and no caller could ever handle.
func encodeSources(sources []string) string {
	if len(sources) == 0 {
		return ""
	}
	b, _ := json.Marshal(sources)
	return string(b)
}

// decodeSources reads back what encodeSources wrote.
func decodeSources(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("sqlite: decode memory sources: %w", err)
	}
	return out, nil
}

// encodeSupersedes renders the superseded ids the same way provenance is rendered:
// a JSON array, or the empty string for none. They are a separate pair of helpers
// rather than a reuse of encodeSources so the decode error names the column the
// operator has to go and look at.
func encodeSupersedes(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

// decodeSupersedes reads back what encodeSupersedes wrote.
func decodeSupersedes(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("sqlite: decode memory supersedes: %w", err)
	}
	return out, nil
}

// encodeAnchors renders anchors for storage as a JSON array of {kind, id}. Like
// encodeSources, no anchors is the empty string rather than "[]". The item's
// anchors are already canonical (state.NormalizeAnchors, on the write path), so
// this preserves their order rather than imposing one.
func encodeAnchors(anchors []state.Anchor) string {
	if len(anchors) == 0 {
		return ""
	}
	out := make([]storedAnchor, len(anchors))
	for i, a := range anchors {
		out[i] = storedAnchor{Kind: a.Kind, ID: a.ID}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// decodeAnchors reads back what encodeAnchors wrote.
func decodeAnchors(raw string) ([]state.Anchor, error) {
	if raw == "" {
		return nil, nil
	}
	var stored []storedAnchor
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, fmt.Errorf("sqlite: decode memory anchors: %w", err)
	}
	out := make([]state.Anchor, len(stored))
	for i, a := range stored {
		out[i] = state.Anchor{Kind: a.Kind, ID: a.ID}
	}
	return out, nil
}

// storedAnchor is the JSON shape of an anchor in the anchors column: lowercase
// keys, matching the memory facade's resource spec, so the same anchor reads the
// same on either backend.
type storedAnchor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// expiryNanos renders an expiry for storage: unix nanoseconds, with 0 reserved for
// "never", which is what the zero time means on the item.
func expiryNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// expiryTime is expiryNanos in reverse.
func expiryTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// scanMemoryRow scans the memory columns named by memoryCols, and the trailing
// relevance score too when score is non-nil (the recall shapes select it as one
// more expression). One scanner so the column list and its readers cannot drift
// apart.
func scanMemoryRow(sc interface{ Scan(...any) error }, score *float64) (state.MemoryItem, error) {
	var (
		m             state.MemoryItem
		supersedes    string
		sources       string
		anchors       string
		created       string
		expires       int64
		tainted       int
		wall, counter int64
		deleted       int
	)
	dst := []any{
		&m.ID, &m.Kind, &m.Content, &m.Subject, &supersedes,
		&m.Scope.Instance, &m.Scope.Project, &m.Scope.Workspace, &sources, &anchors, &created, &expires, &tainted,
		&m.SyncVersion, &m.OriginInstanceID, &wall, &counter, &m.LastWriterID, &deleted,
	}
	if score != nil {
		dst = append(dst, score)
	}
	if err := sc.Scan(dst...); err != nil {
		return state.MemoryItem{}, err
	}
	decodedSupersedes, err := decodeSupersedes(supersedes)
	if err != nil {
		return state.MemoryItem{}, err
	}
	m.Supersedes = decodedSupersedes
	decoded, err := decodeSources(sources)
	if err != nil {
		return state.MemoryItem{}, err
	}
	m.Sources = decoded
	decodedAnchors, err := decodeAnchors(anchors)
	if err != nil {
		return state.MemoryItem{}, err
	}
	m.Anchors = decodedAnchors
	m.CreatedAt = parseTime(created)
	m.ExpiresAt = expiryTime(expires)
	m.Tainted = tainted != 0
	m.UpdatedHLC = hlcTime(wall, counter)
	m.Deleted = deleted != 0
	if score != nil {
		m.Score = *score
	}
	return m, nil
}

func scanMemory(sc interface{ Scan(...any) error }) (state.MemoryItem, error) {
	return scanMemoryRow(sc, nil)
}

// scanScoredMemory scans a recall row: the item's columns plus the trailing
// relevance score.
func scanScoredMemory(sc interface{ Scan(...any) error }) (state.MemoryItem, error) {
	var score float64
	return scanMemoryRow(sc, &score)
}

func upsertMemoryRow(ctx context.Context, tx *sql.Tx, it state.MemoryItem) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memory_items (`+memoryCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
			kind=excluded.kind, content=excluded.content,
			subject=excluded.subject, supersedes=excluded.supersedes,
			scope_instance=excluded.scope_instance, scope_project=excluded.scope_project, scope_workspace=excluded.scope_workspace,
			sources=excluded.sources, anchors=excluded.anchors, created_at=excluded.created_at, expires_at=excluded.expires_at,
			tainted=excluded.tainted,
			sync_version=excluded.sync_version, origin_instance_id=excluded.origin_instance_id,
			updated_hlc_wall=excluded.updated_hlc_wall, updated_hlc_counter=excluded.updated_hlc_counter,
			last_writer_id=excluded.last_writer_id, deleted=excluded.deleted`,
		it.ID, it.Kind, it.Content, it.Subject, encodeSupersedes(it.Supersedes),
		it.Scope.Instance, it.Scope.Project, it.Scope.Workspace, encodeSources(it.Sources),
		encodeAnchors(it.Anchors), formatTime(it.CreatedAt), expiryNanos(it.ExpiresAt), boolToInt(it.Tainted),
		it.SyncVersion, it.OriginInstanceID, it.UpdatedHLC.Wall, int64(it.UpdatedHLC.Counter), it.LastWriterID, boolToInt(it.Deleted))
	return err
}

// projectMemory writes a memory-item post-image: the row and its FTS index
// together. Shared by the live command path and applyEvent (Rebuild), so both
// project identically.
func projectMemory(ctx context.Context, tx *sql.Tx, it state.MemoryItem) error {
	if err := upsertMemoryRow(ctx, tx, it); err != nil {
		return err
	}
	if err := reindexMemory(ctx, tx, it); err != nil {
		return err
	}
	return reindexMemoryAnchors(ctx, tx, it)
}

// reindexMemoryAnchors keeps the anchor lookup table holding rows only while the
// item is live, the same rule reindexMemory applies to the content index, so an
// anchored recall and a lexical one agree on what a tombstone means without either
// read having to filter for it.
//
// It rewrites the item's rows wholesale rather than diffing: an item carries a
// handful of anchors, and a delete-then-insert is both cheaper to reason about and
// correct for the case a rebuild replays a post-image whose anchors differ from
// what is on disk.
func reindexMemoryAnchors(ctx context.Context, tx *sql.Tx, it state.MemoryItem) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_anchors WHERE item_id = ?`, it.ID); err != nil {
		return err
	}
	if it.Deleted {
		return nil
	}
	for _, a := range it.Anchors {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory_anchors (item_id, kind, ref_id) VALUES (?,?,?)`, it.ID, a.Kind, a.ID); err != nil {
			return err
		}
	}
	return nil
}

// reindexMemory keeps the memory FTS index holding an entry only while the item
// is live, so a tombstone drops out of recall.
func reindexMemory(ctx context.Context, tx *sql.Tx, it state.MemoryItem) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE item_id = ?`, it.ID); err != nil {
		return err
	}
	if it.Deleted {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_fts (item_id, content) VALUES (?, ?)`, it.ID, it.Content)
	return err
}

func (m *memory) Write(ctx context.Context, it state.MemoryItem) (state.MemoryItem, error) {
	var rec state.MemoryItem
	err := m.p.commit(ctx, func(*sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		r, ev, err := m.p.st.WriteMemory(it)
		rec = r
		return ev, func(tx *sql.Tx) error { return projectMemory(ctx, tx, r) }, err
	})
	if err != nil {
		return state.MemoryItem{}, err
	}
	return rec, nil
}

func (m *memory) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	query := strings.TrimSpace(q.Query)
	chain := q.ScopeChain()
	// One clock reading bound into both recall shapes and into the Go post-filter,
	// so a single call cannot judge two rows against two different instants. This is
	// the read time itself, not an expiry, so it goes through UnixNano directly:
	// expiryNanos reserves 0 for "never", which is a meaningless answer for a clock.
	now := m.p.clk.Now()
	liveAt := now.UnixNano()

	// The single-scope, no-FTS shape is the agent-startup read; it runs on the
	// prepared statement (a non-positive Limit becomes SQLite's LIMIT -1, no
	// limit, so both limit shapes share it). A widened read cannot: the prepared
	// statement binds exactly one scope triple, so it falls through to the built
	// query below, which is the only shape that can express a variable-length
	// resolution chain.
	// Anything the query language cannot answer correctly is applied after the
	// rows come back, and forces the cap to be applied there too - a SQL LIMIT
	// would otherwise truncate rows that the Go stage was going to drop anyway,
	// returning fewer results than asked for.
	postFilter := !q.Since.IsZero() || !q.Until.IsZero() || q.MinScore > 0

	if query == "" && len(chain) == 1 && len(q.Kinds) == 0 && len(q.Subjects) == 0 && len(q.Anchors) == 0 && !postFilter {
		limit := q.Limit
		if limit <= 0 {
			limit = -1
		}
		rows, err := m.p.stmts.memoryRecall.QueryContext(ctx, liveAt, q.Scope.Instance, q.Scope.Project, q.Scope.Workspace, limit)
		if err != nil {
			return nil, err
		}
		items, err := collectMemory(rows)
		if err != nil {
			return nil, err
		}
		// No query, so nothing was graded and every row is an equally good match.
		for i := range items {
			items[i].Score = 1
		}
		return items, nil
	}

	var sb strings.Builder
	args := make([]any, 0, 8)
	if query == "" {
		// Nothing to rank against, so every row scores 1: state.MemoryItem.Score
		// reserves 1 for "matched, no opinion on how well".
		sb.WriteString(`SELECT ` + memoryColsQualified + `, 1.0
			FROM memory_items m WHERE ` + memoryLiveSQL)
		args = append(args, liveAt)
	} else {
		// FTS5 computes bm25 for the MATCH regardless; the contract used to discard
		// it. bm25 is <= 0 with a more negative value meaning a better match, so
		// -b/(1-b) maps it onto [0,1) increasing in match quality, which is the
		// direction and range Score is defined in.
		sb.WriteString(`SELECT ` + memoryColsQualified + `, (-bm25(memory_fts)) / (1.0 - bm25(memory_fts))
			FROM memory_items m JOIN memory_fts ON memory_fts.item_id = m.id
			WHERE memory_fts MATCH ? AND ` + memoryLiveSQL)
		args = append(args, ftsPhrase(query), liveAt)
	}
	// Kind and subject are exact matches on indexed columns, so they belong in the
	// query rather than in a post-filter: they cut the rows the sort has to order.
	args = writeInClause(&sb, args, "m.kind", q.Kinds)
	args = writeInClause(&sb, args, "m.subject", q.Subjects)
	// Anchors are an indexed lookup through the projection table, expressed as an
	// EXISTS rather than a join so an item anchored to two of the refs asked for
	// comes back once. It belongs in SQL for the same reason kind does: it is the
	// selector that cuts the row set hardest, and a ride-along read supplies it with
	// no lexical query at all, so filtering afterwards would mean pulling the whole
	// scope back to keep a handful of rows.
	for i, a := range q.Anchors {
		if i == 0 {
			sb.WriteString(` AND EXISTS (SELECT 1 FROM memory_anchors ma WHERE ma.item_id = m.id AND (`)
		} else {
			sb.WriteString(` OR `)
		}
		sb.WriteString(`(ma.kind = ? AND ma.ref_id = ?)`)
		args = append(args, a.Kind, a.ID)
	}
	if len(q.Anchors) > 0 {
		sb.WriteString(`))`)
	}
	// The chain is one scope, or that scope's ancestors when the read widened, so
	// the predicate is an OR over its triples. Nil means unfiltered, no predicate.
	for i, sc := range chain {
		if i == 0 {
			sb.WriteString(` AND (`)
		} else {
			sb.WriteString(` OR `)
		}
		sb.WriteString(`(m.scope_instance = ? AND m.scope_project = ? AND m.scope_workspace = ?)`)
		args = append(args, sc.Instance, sc.Project, sc.Workspace)
	}
	if len(chain) > 0 {
		sb.WriteString(`)`)
	}
	// A widened recall ranks most-specific scope first, matching state.Scope.Depth
	// and state.SortRecall, so a workspace's own memory outranks the project
	// memory it inherits. The CASE takes no arguments because it reads the
	// innermost set column rather than comparing against the chain: within one
	// ancestor chain every level has a distinct innermost column, which is exactly
	// what Depth reports. It has to be ordered in SQL rather than after collection,
	// because LIMIT would otherwise truncate the wrong rows.
	sb.WriteString(` ORDER BY`)
	if q.Order == state.OrderRelevance {
		fmt.Fprintf(&sb, ` %d DESC,`, memoryScoreCol)
	}
	if q.RanksByScope() {
		sb.WriteString(` CASE
			WHEN m.scope_workspace <> '' THEN 0
			WHEN m.scope_project <> '' THEN 1
			WHEN m.scope_instance <> '' THEN 2
			ELSE 3 END,`)
	}
	sb.WriteString(` m.created_at DESC, m.id DESC`)
	if q.Limit > 0 && !postFilter {
		sb.WriteString(` LIMIT ?`)
		args = append(args, q.Limit)
	}

	rows, err := m.p.reads().QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	items, err := collectScoredMemory(rows)
	if err != nil {
		return nil, err
	}
	if !postFilter {
		return items, nil
	}
	// The CreatedAt window is applied here rather than as a SQL range: created_at
	// is stored as RFC3339Nano, which drops trailing zeros from the fractional
	// second, so it is not fixed-width and does not compare lexicographically
	// ("...T00:00:00.000000001Z" sorts before "...T00:00:00Z"). Comparing parsed
	// times is the only correct answer available here.
	out := items[:0]
	for _, it := range items {
		if !q.Selects(it, now) || it.Score < q.MinScore {
			continue
		}
		out = append(out, it)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

// writeInClause appends ` AND <col> IN (?, ?, ...)` to sb and the values to args,
// returning the extended args. An empty value list writes nothing at all, which is
// what an unset filter means: matching everything, not matching an empty set.
//
// The column name is a constant from this file and never a caller's string, so the
// only thing crossing into the SQL text is one this package wrote.
func writeInClause(sb *strings.Builder, args []any, col string, vals []string) []any {
	if len(vals) == 0 {
		return args
	}
	sb.WriteString(` AND ` + col + ` IN (`)
	for i, v := range vals {
		if i > 0 {
			sb.WriteString(`, `)
		}
		sb.WriteString(`?`)
		args = append(args, v)
	}
	sb.WriteString(`)`)
	return args
}

// collectMemory drains rows into memory items, closing rows on every path.
func collectMemory(rows *sql.Rows) ([]state.MemoryItem, error) {
	defer func() { _ = rows.Close() }()
	out := make([]state.MemoryItem, 0)
	for rows.Next() {
		it, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// collectScoredMemory drains recall rows, which carry a trailing score column.
func collectScoredMemory(rows *sql.Rows) ([]state.MemoryItem, error) {
	defer func() { _ = rows.Close() }()
	out := make([]state.MemoryItem, 0)
	for rows.Next() {
		it, err := scanScoredMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (m *memory) Delete(ctx context.Context, id string) error {
	return m.p.commit(ctx, func(tx *sql.Tx) (spine.AppendInput, func(*sql.Tx) error, error) {
		existing, err := getLiveMemoryTx(ctx, tx, id)
		if err != nil {
			return spine.AppendInput{}, nil, err
		}
		r, ev, err := m.p.st.DeleteMemory(existing)
		return ev, func(tx *sql.Tx) error { return projectMemory(ctx, tx, r) }, err
	})
}
