//go:build !windows

package sandbox

import "testing"

// runSuiteWithProfileGuard just runs the suite off Windows. Only the Windows AppContainer
// tier registers an operating-system object that can outlive the sandbox that made it, so
// there is nothing here for a leak guard to count.
func runSuiteWithProfileGuard(m *testing.M) int { return m.Run() }
