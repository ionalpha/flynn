package sandbox

import "context"

// GrantRoot gives the sandbox its access to the root before any command asks for
// it, so a host can pay for a large tree while the user is still typing. A
// command that arrives first, or while this runs, waits for the one grant. It
// does nothing for a Local whose commands need no grant, and refuses where the
// Local would refuse to launch, leaving the directory untouched. Cancelling ctx
// ends the wait, as in [WithGrantProgress].
func (l *Local) GrantRoot(ctx context.Context) error {
	if err := l.guardLaunch(); err != nil {
		return err
	}
	if !l.denyNetwork && !l.readonlyFS && !l.seccomp && l.egress == nil {
		return nil
	}
	return l.grantRoot(ctx)
}
