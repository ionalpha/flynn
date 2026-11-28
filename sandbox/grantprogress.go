package sandbox

import (
	"context"
	"sync"
	"time"
)

// GrantProgress reports a directory access grant that has to rewrite the access
// list of every file under it. Windows does that once per directory and sandbox
// identity, and on a large tree it takes minutes, so a host shows it rather than
// look hung.
type GrantProgress struct {
	// Dir is the directory being granted.
	Dir string
	// Elapsed is how long this launch has waited for the grant.
	Elapsed time.Duration
	// Done is set on the last report, whether the grant finished, failed or was
	// cancelled.
	Done bool
}

type grantProgressKey struct{}

// WithGrantProgress returns a context that makes launches under it call fn while
// they wait on a grant that has run long enough to look like a hang. A grant that
// finishes sooner is not reported. fn runs on the launching goroutine, so it must
// return quickly. Cancelling the context ends the launch's wait; Windows cannot
// interrupt the rewrite, which carries on while the process lives.
func WithGrantProgress(ctx context.Context, fn func(GrantProgress)) context.Context {
	return context.WithValue(ctx, grantProgressKey{}, fn)
}

// reportGrant calls the function [WithGrantProgress] installed, if any.
func reportGrant(ctx context.Context, p GrantProgress) {
	if fn, ok := ctx.Value(grantProgressKey{}).(func(GrantProgress)); ok && fn != nil {
		fn(p)
	}
}

type probeKey struct{}

// withProbe marks a launch as the confinement probe, which must not grant the
// working directory: the probe asks whether the host allows the mechanism, and
// the answer does not depend on a tree it is never going to touch.
func withProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, probeKey{}, true)
}

func isProbe(ctx context.Context) bool {
	v, _ := ctx.Value(probeKey{}).(bool)
	return v
}

// grantMemo remembers which grants this Local has already confirmed, so a command
// after the first does not read the directory's access list again.
type grantMemo struct {
	mu   sync.Mutex
	done map[string]bool
}

func (g *grantMemo) has(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done[key]
}

func (g *grantMemo) mark(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done == nil {
		g.done = make(map[string]bool)
	}
	g.done[key] = true
}
