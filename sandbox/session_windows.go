//go:build windows

package sandbox

import "context"

// duplexConfinementExpressible is false on Windows: its confinement is an AppContainer
// applied at process creation through a blocking launch, which cannot be carried onto a
// still-writable background process handle. A confined duplex Session is therefore refused
// rather than run unconfined (see Session and errSessionConfineUnsupported); only the
// always-on best-effort baseline is allowed to fall to the unconfined floor.
func duplexConfinementExpressible() bool { return false }

// startSession launches an unconfined duplex session through the standard library. Session
// only ever reaches this on Windows for an unconfined launch or the best-effort baseline
// fallthrough, both of which run without confinement; confined is forced false so a value
// that slipped through cannot become a silent unconfined run under a confined tier.
func (l *Local) startSession(ctx context.Context, spec SessionSpec, confined bool) (*Session, error) {
	return l.startSessionExec(ctx, spec, false)
}
