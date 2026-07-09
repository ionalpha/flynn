package sandbox

import "os/exec"

// LookPath resolves a program name against the host's PATH and returns its absolute
// path. It is the sanctioned resolver for callers outside this package, which may not
// import os/exec: spawning is confined to the sandbox boundary, and resolving a name is
// the step before spawning. Resolving does not run anything.
//
// A confined child does not resolve names for itself. The AppContainer it runs in cannot
// read most of the host, so a bare program name it inherits on PATH does not resolve to
// anything it may execute, and the launch fails with an error about the name rather than
// the access. Callers pass the resolved absolute path into a launch and grant the child
// read access to the directory holding it (see WithReadableDir), so the failure modes are
// "not installed" and "not reachable from inside the sandbox", which are different
// problems with different fixes.
func LookPath(name string) (string, error) { return exec.LookPath(name) }
