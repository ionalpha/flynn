package render

import (
	"testing"

	east "github.com/yuin/goldmark/extension/ast"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

// TestTableWithNoColumnsRendersNothing checks that a table node with no cells
// yields no rows. Column widths are sized from the cell count, so a zero count
// would otherwise reach fitColumns with a negative separator budget.
func TestTableWithNoColumnsRendersNothing(t *testing.T) {
	m := NewMarkdown(theme.Default())

	if rows := m.table(east.NewTable(), nil, 40, theme.Style{}); len(rows) != 0 {
		t.Fatalf("a table without columns rendered %q", rows)
	}
}
