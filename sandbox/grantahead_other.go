//go:build !windows

package sandbox

import "context"

// grantRoot has nothing to do where confinement is applied to the process and
// leaves the directory's permissions alone.
func (l *Local) grantRoot(context.Context) error { return nil }
