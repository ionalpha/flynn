package sandbox

import "sync"

// scratchState is the sandbox's scratch directory: created on the first confined launch and
// removed on Close, so a Local that never runs a confined command leaves nothing on disk.
// Only the Windows tiers use it (see scratchDir); the other platforms already give a
// confined child a writable temp directory of its own.
type scratchState struct { //nolint:unused // used by the Windows tiers only
	mu  sync.Mutex
	dir string
}
