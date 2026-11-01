package sqlitex

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestIsBusyIgnoresErrorsThatAreNotSQLite: only the driver's own contention codes may
// keep Open retrying; a cancelled context or a plain failure must end the wait at once.
func TestIsBusyIgnoresErrorsThatAreNotSQLite(t *testing.T) {
	for _, err := range []error{
		context.Canceled,
		errors.New("database is locked (5) (SQLITE_BUSY)"),
		fmt.Errorf("open: %w", context.DeadlineExceeded),
	} {
		if isBusy(err) {
			t.Errorf("isBusy(%v) = true, want false", err)
		}
	}
}
