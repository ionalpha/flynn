// Package procs counts the child processes this process has started and not yet
// reaped. A spawned command never waited on leaks a zombie that no other diagnostic
// sees: heap and goroutines stay flat until the process table fills.
//
// Spawners keep the count, so reading it is an atomic load whatever the machine's
// process count. Walking /proc or a Windows snapshot instead costs an open per
// process per read, and after pid reuse counts processes that name this pid as
// parent rather than the children it started.
//
// A registry holds no *os.Process and never calls Wait, because two owners of a
// process's exit race; reaping stays with the spawner.
package procs

import "sync/atomic"

// Registry counts live children. The zero Registry is ready to use and safe for
// concurrent use.
type Registry struct {
	live atomic.Int64
}

// Live reports how many children are started and unreaped, in one atomic load.
func (r *Registry) Live() int { return int(r.live.Load()) }

// Started records a started child and returns the function that records its
// reaping; call it where the child is already waited on. It is idempotent, so a
// spawn path that releases on error and again from a reap goroutine cannot drive the
// count negative.
func (r *Registry) Started() (reaped func()) {
	r.live.Add(1)

	var done atomic.Bool
	return func() {
		if done.CompareAndSwap(false, true) {
			r.live.Add(-1)
		}
	}
}

// std is the process-wide registry, because a child of this process is a
// process-wide fact and separate registries would each report a partial count.
var std Registry

// Live reports this process's started and unreaped children; a diagnostics bundle
// reads it once per timeline sample.
func Live() int { return std.Live() }

// Started records a child of this process; see Registry.Started.
func Started() (reaped func()) { return std.Started() }
