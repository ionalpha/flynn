// Package procs counts the child processes this process has started and has not yet
// reaped.
//
// The count exists for one reason: an agent that spawns a sandboxed command and never
// waits on it leaks a zombie, and that leak is invisible to every other diagnostic. The
// heap stays flat, the goroutine count stays flat, and the process dies of a full
// process table.
//
// The count is kept by the spawners, not by reading the operating system. A registry
// increments when a child is started and decrements when it is waited on, so reading it
// is an atomic load whose cost does not depend on how many processes exist on the
// machine. The alternative, walking /proc or a Windows process snapshot on every read,
// costs one file open per process on the box per read, and answers a subtly different
// question: it counts processes that currently *name* this pid as their parent, which
// after pid reuse is not the same set as the children this process actually started.
//
// A registry never holds an *os.Process and never calls Wait. Reaping stays with the
// spawner that owns the command; two owners of a process's exit is a race.
package procs

import "sync/atomic"

// Registry counts live children. The zero Registry is ready to use, and every method is
// safe to call from any goroutine.
type Registry struct {
	live atomic.Int64
}

// Live reports how many children have been started and not yet reaped. It is a single
// atomic load: its cost is flat in the number of processes on the machine and in the
// number of children this process has spawned over its life.
func (r *Registry) Live() int { return int(r.live.Load()) }

// Started records that a child process has been started, and returns the function that
// records its reaping. The caller invokes the returned function where it already waits
// on the child, so the registry learns of the exit from the one goroutine that owns it.
//
// The returned function is idempotent: calling it twice decrements once. A spawn path
// that releases on an error path and again from a reap goroutine is therefore safe, and
// no sequence of calls can drive the count negative.
func (r *Registry) Started() (reaped func()) {
	r.live.Add(1)

	var done atomic.Bool
	return func() {
		if done.CompareAndSwap(false, true) {
			r.live.Add(-1)
		}
	}
}

// std is the registry for this OS process. A child of this process is a process-wide
// fact, like its pid, so the spawners record into one registry rather than each holding
// its own and reporting a partial count.
var std Registry

// Live reports how many children this process has started and not yet reaped. It is the
// function a diagnostics bundle reads once per timeline sample.
func Live() int { return std.Live() }

// Started records a child of this process and returns the idempotent function that
// records its reaping. See Registry.Started.
func Started() (reaped func()) { return std.Started() }
