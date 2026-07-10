//go:build windows

package sandbox

import (
	"os"
	"strconv"
	"testing"
)

// runSuiteWithProfileGuard runs the package's tests and then fails the run if the suite
// left AppContainer profiles registered behind it. A Local that is never Closed leaks one,
// and nothing about the leak is visible from inside the test that caused it, so it went
// unnoticed until a dev box held thousands of them. Checking once around the whole suite
// turns that class of mistake into a build failure instead of a slow accumulation.
//
// It reads this process's own registration count rather than counting the profile
// directory. The directory is shared: the helper processes these tests re-execute, and any
// other test binary running beside them, register and delete profiles of their own there,
// so a directory count would blame this suite for their work and race with it.
func runSuiteWithProfileGuard(m *testing.M) int {
	code := m.Run()
	if code != 0 {
		return code // a failing suite has a better story to tell than its profile count
	}
	if n := LiveProfileCount(); n > 0 {
		_, _ = os.Stderr.WriteString(
			"sandbox: this test suite leaked " + strconv.Itoa(n) + " AppContainer profile(s). " +
				"A Local that is never Closed leaks one; give every NewLocal in a test a t.Cleanup that Closes it.\n",
		)
		return 1
	}
	return code
}
