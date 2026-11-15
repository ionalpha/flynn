package sandbox

import "os/exec"

// LookPath resolves name on the host PATH for callers that may not import
// os/exec. Resolve on the host and grant the directory with [WithReadableDir],
// because an AppContainer child cannot read most of the host, so a bare name fails
// as a name error instead of an access error.
func LookPath(name string) (string, error) { return exec.LookPath(name) }
